package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

// `moe usage` answers "where is the token spend actually going" from
// state that already exists: every stage transcript is mirrored into the
// bureaucracy, and every assistant event in one carries its usage and
// the model that produced it. So this is a read-side aggregator over
// files on disk — no new state, no collection pipeline, no daemon. Same
// posture as dash: compute on read, and if the answer is stale it's
// because nothing has run.
//
// Two measures, because the question has two shapes. **Notional API
// dollars** is a comparability unit — under a Max plan the marginal cost
// of a run is zero until a window bites, but "the pulse costs as much as
// six flagship sdlc runs" is only sayable in a common currency. **Raw
// tokens in the selected window** are the thing that actually trips
// limits. Neither measure is a bill; the header says so and the dollar
// column is labelled notional.

// Model prices in US dollars per million tokens, API sticker rates.
//
// Hardcoded on purpose. A runtime config file would be a new seam to
// document, validate and keep in sync for a table that changes a few
// times a year and only feeds a comparability column; a monthly chore
// (projects/<project>/chores/update-model-prices) re-reads the pricing
// docs and ships the one-line diff instead. That makes staleness a
// scheduled, visible thing rather than a silent one.
//
// Match is a prefix, because model ids carry date suffixes
// (claude-haiku-4-5-20251001) and occasionally context tags. Longest
// match wins, so a more specific entry can override a family default.
// A model with no entry is reported with its tokens and no dollar
// figure — inventing a rate would be worse than admitting the gap.
type modelPrice struct {
	prefix string
	input  float64
	output float64
}

var modelPrices = []modelPrice{
	{"claude-fable-5", 10, 50},
	{"claude-opus-4-8", 5, 25},
	{"claude-opus-4-7", 5, 25},
	// Sticker, deliberately: an intro $2/$10 runs through 2026-08-31, but
	// this column is defined as notional sticker and flipping to intro
	// now then back in September would jump comparisons 1.5× for nothing.
	{"claude-sonnet-5", 3, 15},
	{"claude-haiku-4-5", 1, 5},
	// OpenAI API sticker (developers.openai.com/api/docs/pricing). The
	// long-context surcharge above 272K input tokens isn't modeled —
	// codex reports cumulative per-rollout totals that can't be split
	// per request, so everything prices at the short-context rate.
	{"gpt-5.5", 5, 30},
	{"gpt-5.6-sol", 5, 30},
}

// Cache multipliers against the input rate. A hit costs a tenth, which
// holds for the OpenAI entries too (their published cached-input rate is
// exactly 0.1× base). A write costs double, because Claude Code writes
// its cache at the 1-hour TTL — 99.3% of cache-write tokens in this
// bureaucracy — and a 1-hour write bills at 2× base input; a 5-minute
// write would be 1.25×. The write multiplier is Claude-only in effect:
// codex reports no cache-write bucket, so those rows never touch it.
const (
	cacheWriteMultiplier = 2.0
	cacheReadMultiplier  = 0.10
)

// notionalCost returns the sticker-price dollars for one model's usage,
// and false when the model isn't in the price map.
func notionalCost(model string, u transcript.ModelUsage) (float64, bool) {
	p, ok := priceFor(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	in := float64(u.Input) + float64(u.CacheWrite)*cacheWriteMultiplier + float64(u.CacheRead)*cacheReadMultiplier
	return in/perMillion*p.input + float64(u.Output)/perMillion*p.output, true
}

func priceFor(model string) (modelPrice, bool) {
	best, found := modelPrice{}, false
	for _, p := range modelPrices {
		if len(model) < len(p.prefix) || model[:len(p.prefix)] != p.prefix {
			continue
		}
		if !found || len(p.prefix) > len(best.prefix) {
			best, found = p, true
		}
	}
	return best, found
}

// usageRow is one (workflow, stage, model) bucket of the report.
type usageRow struct {
	workflow string
	stage    string
	model    string
	runs     map[string]bool
	usage    transcript.ModelUsage
}

// runUsageRow is every accepted transcript bucket for one qualified run.
type runUsageRow struct {
	key      string
	workflow string
	stages   map[string]bool
	usage    transcript.ModelUsage
	dollars  float64
	unpriced int64
}

func init() {
	Register(&Command{
		Name:    "usage",
		Summary: "sum token usage across run transcripts, with notional API dollars",
		Run:     runUsage,
	})
}

func runUsage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectF := fs.String("project", "", "limit to one project")
	sinceF := fs.String("since", "30d", "only count stages whose last turn is within this window (e.g. 7d, 24h)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe usage [--project <id>] [--since <dur>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Sums the token usage recorded in every mirrored stage transcript,")
		moePrintln(stderr, "with workflow/stage/model, selected rolling-window, per-run, and")
		moePrintln(stderr, "by-day views. Notional dollars are a comparability unit, not a bill.")
		moePrintln(stderr, "Each whole stage is dated by its last work turn, falling back to run")
		moePrintln(stderr, "activity; untimed transcripts stay in totals and are marked.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	window, err := chore.ParseDuration(*sinceF)
	if err != nil {
		moePrintf(stderr, "moe usage: --since %q: %v\n", *sinceF, err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if *projectF != "" {
		if err := requireProject(root, *projectF); err != nil {
			moePrintf(stderr, "moe usage: %v\n", err)
			return 1
		}
	}
	now := time.Now()
	report, err := gatherUsage(root, *projectF, now.Add(-window), stderr)
	if err != nil {
		moePrintf(stderr, "moe usage: %v\n", err)
		return 1
	}
	renderUsage(stdout, report, *projectF, *sinceF)
	return 0
}

// usageReport is everything the render needs: the per-bucket and per-run
// rows, the per-day totals, and the usage that cannot be fully attributed.
type usageReport struct {
	rows        []usageRow
	byRun       []runUsageRow
	byDay       map[string]transcript.ModelUsage
	dayDollars  map[string]float64
	dayUnpriced map[string]int64
	total       transcript.ModelUsage
	dollars     float64
	unpriced    map[string]int64
	transcripts int
	untimed     int
}

// gatherUsage walks every run's mirrored transcripts and buckets their
// usage.
//
// A stage's *when* comes from the journal, not the filesystem: git
// committer time of that stage's most recent work turn, falling back to
// the run's last activity. Mtimes would be the obvious choice and are
// worthless here — the bureaucracy is checked out into per-session
// worktrees, so every file's mtime is its checkout time and `--since 1d`
// would report the whole archive.
//
// A stage with no journal timestamp at all is counted regardless of the
// window. That is the pre-journal and mid-write case; dropping it would
// under-report, and the window is a lens rather than a ledger.
func gatherUsage(root, projectFilter string, cutoff time.Time, stderr io.Writer) (usageReport, error) {
	rep := usageReport{
		byDay:       map[string]transcript.ModelUsage{},
		dayDollars:  map[string]float64{},
		dayUnpriced: map[string]int64{},
		unpriced:    map[string]int64{},
	}
	mds, err := run.Scan(root)
	if err != nil {
		return rep, fmt.Errorf("scan runs: %w", err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return rep, fmt.Errorf("build journal index: %w", err)
	}
	byBucket := map[string]*usageRow{}
	byRun := map[string]*runUsageRow{}
	for _, md := range mds {
		if projectFilter != "" && md.Project != projectFilter {
			continue
		}
		runKey := md.Project + "/" + md.ID
		for _, stage := range stageDocsOnDisk(root, md) {
			when := idx.WorkTurnTime[run.WorkTurnKey{Project: md.Project, Run: md.ID, Doc: stage}]
			if when.IsZero() {
				when = idx.LastActivity[runKey]
			}
			if !when.IsZero() && when.Before(cutoff) {
				continue
			}
			for _, agent := range []string{"claude", "codex"} {
				path := filepath.Join(root, run.ThreadPathFor(agent, md.Project, md.ID, stage))
				u, ok := readTranscriptUsage(agent, path, stderr)
				if !ok {
					continue
				}
				rep.transcripts++
				if when.IsZero() {
					rep.untimed++
				}
				runRow := byRun[runKey]
				if runRow == nil {
					runRow = &runUsageRow{key: runKey, workflow: md.Workflow, stages: map[string]bool{}}
					byRun[runKey] = runRow
				}
				runRow.stages[stage] = true
				for model, mu := range u {
					key := md.Workflow + "\x00" + stage + "\x00" + model
					row := byBucket[key]
					if row == nil {
						row = &usageRow{workflow: md.Workflow, stage: stage, model: model, runs: map[string]bool{}}
						byBucket[key] = row
					}
					row.runs[runKey] = true
					row.usage = mergeModelUsage(row.usage, mu)

					rep.total = mergeModelUsage(rep.total, mu)
					runRow.usage = mergeModelUsage(runRow.usage, mu)
					cost, priced := notionalCost(model, mu)
					rep.dollars += cost
					runRow.dollars += cost
					tokens := totalTokens(mu)
					if !priced && tokens > 0 {
						rep.unpriced[model] += tokens
						runRow.unpriced += tokens
					}
					if !when.IsZero() {
						day := when.Local().Format("2006-01-02")
						rep.byDay[day] = mergeModelUsage(rep.byDay[day], mu)
						rep.dayDollars[day] += cost
						if !priced {
							rep.dayUnpriced[day] += tokens
						}
					}
				}
			}
		}
	}
	for _, row := range byBucket {
		rep.rows = append(rep.rows, *row)
	}
	for _, row := range byRun {
		rep.byRun = append(rep.byRun, *row)
	}
	// Most expensive first: the report exists to name the biggest line
	// item, so it should be the first line read. Unpriced buckets sort by
	// output tokens among themselves and land at the bottom.
	sort.Slice(rep.rows, func(i, j int) bool {
		ci, _ := notionalCost(rep.rows[i].model, rep.rows[i].usage)
		cj, _ := notionalCost(rep.rows[j].model, rep.rows[j].usage)
		if ci != cj {
			return ci > cj
		}
		return rep.rows[i].usage.Output > rep.rows[j].usage.Output
	})
	sort.Slice(rep.byRun, func(i, j int) bool {
		if rep.byRun[i].dollars != rep.byRun[j].dollars {
			return rep.byRun[i].dollars > rep.byRun[j].dollars
		}
		ti, tj := totalTokens(rep.byRun[i].usage), totalTokens(rep.byRun[j].usage)
		if ti != tj {
			return ti > tj
		}
		return rep.byRun[i].key < rep.byRun[j].key
	})
	return rep, nil
}

func mergeModelUsage(a, b transcript.ModelUsage) transcript.ModelUsage {
	a.Input += b.Input
	a.CacheWrite += b.CacheWrite
	a.CacheRead += b.CacheRead
	a.Output += b.Output
	a.Steps += b.Steps
	return a
}

func inputTokens(u transcript.ModelUsage) int64 {
	return u.Input + u.CacheWrite + u.CacheRead
}

func totalTokens(u transcript.ModelUsage) int64 {
	return inputTokens(u) + u.Output
}

// stageDocsOnDisk lists the document ids a run has directories for,
// sorted. Read from disk rather than from run.Documents because that map
// records sessions, and a stage can have a mirrored transcript without
// one (a headless one-shot that never resumed).
func stageDocsOnDisk(root string, md *run.Metadata) []string {
	entries, err := os.ReadDir(filepath.Join(root, run.Dir(md.Project, md.ID), "documents"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// readTranscriptUsage parses one thread file, or reports false when it
// isn't there. A parse failure warns and counts what it got: transcripts
// are forensic files, and one truncated tail shouldn't take the report
// down with it.
func readTranscriptUsage(agent, path string, stderr io.Writer) (transcript.Usage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	u, err := transcript.ParseUsage(agent, f)
	if err != nil {
		moePrintf(stderr, "moe usage: %s: %v\n", path, err)
	}
	return u, len(u) > 0
}

func renderUsage(w io.Writer, rep usageReport, projectFilter, since string) {
	scope := "all projects"
	if projectFilter != "" {
		scope = projectFilter
	}
	moePrintf(w, "usage — %s · last %s · %d transcript(s)\n", scope, since, rep.transcripts)
	if rep.transcripts == 0 {
		moePrintln(w, "")
		moePrintln(w, "No stage transcripts in the window.")
		return
	}
	moePrintln(w, "")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKFLOW\tSTAGE\tMODEL\tRUNS\tSTEPS\tCACHE-W\tCACHE-R\tOUTPUT\tNOTIONAL")
	for _, r := range rep.rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			r.workflow, r.stage, r.model, len(r.runs), r.usage.Steps,
			humanTokens(r.usage.CacheWrite), humanTokens(r.usage.CacheRead),
			humanTokens(r.usage.Output), notionalDollars(r.model, r.usage))
	}
	fmt.Fprintf(tw, "\t\tTOTAL\t\t%d\t%s\t%s\t%s\t%s\n",
		rep.total.Steps, humanTokens(rep.total.CacheWrite), humanTokens(rep.total.CacheRead),
		humanTokens(rep.total.Output), totalNotional(rep.dollars, rep.total, unpricedTokens(rep.unpriced)))
	tw.Flush()

	moePrintln(w, "")
	moePrintln(w, "CURRENT ROLLING WINDOW")
	ww := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(ww, "LAST\tSTEPS\tINPUT\tOUTPUT\tTOTAL\tNOTIONAL")
	fmt.Fprintf(ww, "%s\t%d\t%s\t%s\t%s\t%s\n", since, rep.total.Steps,
		humanTokens(inputTokens(rep.total)), humanTokens(rep.total.Output), humanTokens(totalTokens(rep.total)),
		totalNotional(rep.dollars, rep.total, unpricedTokens(rep.unpriced)))
	ww.Flush()

	moePrintln(w, "")
	moePrintf(w, "BY RUN (within last %s)\n", since)
	rw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "RUN\tWORKFLOW\tSTAGES\tSTEPS\tINPUT\tOUTPUT\tTOTAL\tNOTIONAL")
	for _, r := range rep.byRun {
		fmt.Fprintf(rw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			r.key, r.workflow, len(r.stages), r.usage.Steps, humanTokens(inputTokens(r.usage)),
			humanTokens(r.usage.Output), humanTokens(totalTokens(r.usage)),
			totalNotional(r.dollars, r.usage, r.unpriced))
	}
	rw.Flush()

	if len(rep.byDay) > 0 {
		moePrintln(w, "")
		moePrintln(w, "BY DAY")
		days := make([]string, 0, len(rep.byDay))
		for d := range rep.byDay {
			days = append(days, d)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(days)))
		dw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range days {
			u := rep.byDay[d]
			fmt.Fprintf(dw, "%s\t%d steps\t%s in\t%s out\t%s\n",
				d, u.Steps, humanTokens(inputTokens(u)), humanTokens(u.Output),
				totalNotional(rep.dayDollars[d], u, rep.dayUnpriced[d]))
		}
		dw.Flush()
	}

	if len(rep.unpriced) > 0 {
		models := make([]string, 0, len(rep.unpriced))
		for m := range rep.unpriced {
			models = append(models, m)
		}
		sort.Strings(models)
		moePrintln(w, "")
		for _, m := range models {
			moePrintf(w, "no price on record for %s (%s tokens uncounted in the notional column)\n",
				m, humanTokens(rep.unpriced[m]))
		}
		if hasPartialNotional(rep.total, unpricedTokens(rep.unpriced)) {
			moePrintln(w, "* starred totals exclude tokens from models with no price on record")
		}
	}
	if rep.untimed > 0 {
		moePrintln(w, "")
		moePrintf(w, "%d untimed transcript(s) included in aggregate, current-window, and per-run totals; omitted from BY DAY\n", rep.untimed)
	}
	moePrintln(w, "")
	moePrintln(w, "Notional dollars are API sticker prices, for comparing workflows — not a bill.")
}

// notionalDollars renders a bucket's cost, or "—" when the model has no
// price on record. The dash is the honest answer: the tokens are in the
// row either way.
func notionalDollars(model string, u transcript.ModelUsage) string {
	cost, ok := notionalCost(model, u)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cost)
}

func unpricedTokens(models map[string]int64) int64 {
	var total int64
	for _, tokens := range models {
		total += tokens
	}
	return total
}

func hasPartialNotional(u transcript.ModelUsage, unpriced int64) bool {
	return unpriced > 0 && unpriced < totalTokens(u)
}

// totalNotional renders an aggregate cost without implying that a partial
// price is complete. A dash means every token in the aggregate is unpriced.
func totalNotional(dollars float64, u transcript.ModelUsage, unpriced int64) string {
	if totalTokens(u) > 0 && unpriced == totalTokens(u) {
		return "—"
	}
	if hasPartialNotional(u, unpriced) {
		return fmt.Sprintf("$%.2f*", dollars)
	}
	return fmt.Sprintf("$%.2f", dollars)
}

// humanTokens renders a token count compactly with a K/M suffix —
// 12.4M reads at a glance where 12437291 does not.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
