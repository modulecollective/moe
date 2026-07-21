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
// six flagship sdlc runs" is only sayable in a common currency. **Tokens
// per day** is the thing that actually trips limits. Neither is a bill;
// the header says so and the column is labelled notional.

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
	{"claude-sonnet-5", 3, 15},
	{"claude-haiku-4-5", 1, 5},
}

// Cache multipliers against the input rate: a write costs a quarter more
// than a fresh read of the same tokens, a hit costs a tenth. Uniform
// across the Claude family, so they live here rather than per-entry.
const (
	cacheWriteMultiplier = 1.25
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
		moePrintln(stderr, "grouped by workflow, stage and model, with notional API-sticker")
		moePrintln(stderr, "dollars alongside. Notional is a comparability unit, not a bill.")
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
	report, err := gatherUsage(root, *projectF, time.Now().Add(-window), stderr)
	if err != nil {
		moePrintf(stderr, "moe usage: %v\n", err)
		return 1
	}
	renderUsage(stdout, report, *projectF, *sinceF)
	return 0
}

// usageReport is everything the render needs: the per-bucket rows, the
// per-day totals, and the tokens that carried no price.
type usageReport struct {
	rows        []usageRow
	byDay       map[string]transcript.ModelUsage
	dayDollars  map[string]float64
	total       transcript.ModelUsage
	dollars     float64
	unpriced    map[string]int64
	transcripts int
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
		byDay:      map[string]transcript.ModelUsage{},
		dayDollars: map[string]float64{},
		unpriced:   map[string]int64{},
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
					cost, priced := notionalCost(model, mu)
					rep.dollars += cost
					if tokens := mu.Input + mu.CacheWrite + mu.CacheRead + mu.Output; !priced && tokens > 0 {
						rep.unpriced[model] += tokens
					}
					if !when.IsZero() {
						day := when.Local().Format("2006-01-02")
						rep.byDay[day] = mergeModelUsage(rep.byDay[day], mu)
						rep.dayDollars[day] += cost
					}
				}
			}
		}
	}
	for _, row := range byBucket {
		rep.rows = append(rep.rows, *row)
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
	fmt.Fprintf(tw, "\t\tTOTAL\t\t%d\t%s\t%s\t%s\t$%.2f\n",
		rep.total.Steps, humanTokens(rep.total.CacheWrite), humanTokens(rep.total.CacheRead),
		humanTokens(rep.total.Output), rep.dollars)
	tw.Flush()

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
			fmt.Fprintf(dw, "%s\t%d steps\t%s in\t%s out\t$%.2f\n",
				d, u.Steps, humanTokens(u.Input+u.CacheWrite+u.CacheRead), humanTokens(u.Output), rep.dayDollars[d])
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

// humanTokens renders a token count at three significant figures with a
// K/M suffix — 12.4M reads at a glance where 12437291 does not.
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
