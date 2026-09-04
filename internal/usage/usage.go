// Package usage answers "where is the token spend actually going" from
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
// limits. Neither measure is a bill; every renderer says so and the
// dollar column is labelled notional.
//
// Two callers, which is why this is a package rather than a file in
// cli: `moe usage` renders the report as tabwriter text, and serve's
// GET /usage renders it as a page. Neither needs the workflow registry,
// so there is no seam to inject — same call on both sides.
package usage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

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
	// Standard speed, deliberately: fast mode bills $10/$50, but a
	// transcript records only the model id, not the speed. Stages run
	// headless `claude -p` at standard speed, so this is the right
	// notional rate for the tokens this map actually sees.
	{"claude-opus-5", 5, 25},
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

// NotionalCost returns the sticker-price dollars for one model's usage,
// and false when the model isn't in the price map.
func NotionalCost(model string, u transcript.ModelUsage) (float64, bool) {
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

// Filter narrows a Report without narrowing the scan behind it.
type Filter struct {
	// Project limits the report to one project; empty counts every one.
	Project string
	// Cutoff drops stages whose last work turn predates it. A stage with
	// no journal timestamp at all is counted regardless — that is the
	// pre-journal and mid-write case, dropping it would under-report,
	// and the window is a lens rather than a ledger.
	Cutoff time.Time
}

// Bucket is one (workflow, stage, model) line of the report.
type Bucket struct {
	Workflow string
	Stage    string
	Model    string
	// Runs is how many distinct runs fed this bucket.
	Runs  int
	Usage transcript.ModelUsage
}

// Run is every accepted transcript bucket for one qualified run.
type Run struct {
	Project  string
	ID       string
	Workflow string
	// Stages is how many of the run's stages contributed.
	Stages   int
	Usage    transcript.ModelUsage
	Dollars  float64
	Unpriced int64
	// Last is the run's journal last activity — what run order sorts on.
	// Zero for a run the journal has no commit for.
	Last time.Time
}

// Key is the "project/id" the run is addressed by everywhere else.
func (r Run) Key() string { return r.Project + "/" + r.ID }

// Day is one calendar day's totals, keyed by local 2006-01-02.
type Day struct {
	Usage    transcript.ModelUsage
	Dollars  float64
	Unpriced int64
}

// Report is everything a renderer needs: the per-bucket and per-run
// rows, the per-day totals, and the usage that cannot be fully
// attributed.
type Report struct {
	// Buckets is (workflow, stage, model), most expensive first.
	Buckets []Bucket
	// Runs is in run order — last activity, newest first. A renderer
	// that wants another order sorts this slice itself; it is freshly
	// built per Gather and shared with nobody.
	Runs        []Run
	ByDay       map[string]Day
	Total       transcript.ModelUsage
	Dollars     float64
	Unpriced    map[string]int64
	Transcripts int
	Untimed     int
}

// UnpricedTokens is every token the notional column had to abstain on.
func (r Report) UnpricedTokens() int64 {
	var total int64
	for _, tokens := range r.Unpriced {
		total += tokens
	}
	return total
}

// Starred reports whether the report's totals are partial — some tokens
// priced, some not — and so must carry the star.
func (r Report) Starred() bool { return PartialNotional(r.Total, r.UnpricedTokens()) }

// Gather aggregates the scanned transcripts under root through f.
//
// The scan behind it is memoized on HEAD, so a second call with a
// different window or project is an aggregate over records already in
// memory rather than a re-parse of every JSONL on disk. warn receives
// one line per unparseable transcript, once per HEAD.
func Gather(root string, f Filter, warn io.Writer) (Report, error) {
	sc, err := cachedScan(root, warn)
	if err != nil {
		return Report{}, err
	}
	rep := Report{ByDay: map[string]Day{}, Unpriced: map[string]int64{}}
	byBucket := map[string]*Bucket{}
	bucketRuns := map[string]map[string]bool{}
	byRun := map[string]*Run{}
	runStages := map[string]map[string]bool{}
	for _, st := range sc.stages {
		if f.Project != "" && st.Project != f.Project {
			continue
		}
		if !st.When.IsZero() && st.When.Before(f.Cutoff) {
			continue
		}
		rep.Transcripts++
		if st.When.IsZero() {
			rep.Untimed++
		}
		runKey := st.Project + "/" + st.Run
		runRow := byRun[runKey]
		if runRow == nil {
			runRow = &Run{
				Project:  st.Project,
				ID:       st.Run,
				Workflow: st.Workflow,
				Last:     sc.last[runKey],
			}
			byRun[runKey] = runRow
			runStages[runKey] = map[string]bool{}
		}
		runStages[runKey][st.Stage] = true
		for model, mu := range st.Usage {
			key := st.Workflow + "\x00" + st.Stage + "\x00" + model
			row := byBucket[key]
			if row == nil {
				row = &Bucket{Workflow: st.Workflow, Stage: st.Stage, Model: model}
				byBucket[key] = row
				bucketRuns[key] = map[string]bool{}
			}
			bucketRuns[key][runKey] = true
			row.Usage = Merge(row.Usage, mu)

			rep.Total = Merge(rep.Total, mu)
			runRow.Usage = Merge(runRow.Usage, mu)
			cost, priced := NotionalCost(model, mu)
			rep.Dollars += cost
			runRow.Dollars += cost
			tokens := Tokens(mu)
			if !priced && tokens > 0 {
				rep.Unpriced[model] += tokens
				runRow.Unpriced += tokens
			}
			if !st.When.IsZero() {
				stamp := st.When.Local().Format("2006-01-02")
				day := rep.ByDay[stamp]
				day.Usage = Merge(day.Usage, mu)
				day.Dollars += cost
				if !priced {
					day.Unpriced += tokens
				}
				rep.ByDay[stamp] = day
			}
		}
	}
	for key, row := range byBucket {
		row.Runs = len(bucketRuns[key])
		rep.Buckets = append(rep.Buckets, *row)
	}
	for key, row := range byRun {
		row.Stages = len(runStages[key])
		rep.Runs = append(rep.Runs, *row)
	}
	// Most expensive first: the report exists to name the biggest line
	// item, so it should be the first line read. Unpriced buckets sort by
	// output tokens among themselves and land at the bottom.
	sort.Slice(rep.Buckets, func(i, j int) bool {
		ci, _ := NotionalCost(rep.Buckets[i].Model, rep.Buckets[i].Usage)
		cj, _ := NotionalCost(rep.Buckets[j].Model, rep.Buckets[j].Usage)
		if ci != cj {
			return ci > cj
		}
		return rep.Buckets[i].Usage.Output > rep.Buckets[j].Usage.Output
	})
	SortRunsByRecency(rep.Runs)
	return rep, nil
}

// SortRunsByRecency puts the runs in run order: last activity, newest
// first, then key. Untimed runs (no journal commit) land at the bottom
// — a zero time would otherwise read as 1970 and sort as the oldest
// thing on the board, which is the opposite of what it means.
func SortRunsByRecency(runs []Run) {
	sort.Slice(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Last.IsZero() != b.Last.IsZero() {
			return b.Last.IsZero()
		}
		if !a.Last.Equal(b.Last) {
			return a.Last.After(b.Last)
		}
		return a.Key() < b.Key()
	})
}

// SortRunsByCost puts the most expensive run first, the order `moe
// usage`'s BY RUN table has always used.
func SortRunsByCost(runs []Run) {
	sort.Slice(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Dollars != b.Dollars {
			return a.Dollars > b.Dollars
		}
		ta, tb := Tokens(a.Usage), Tokens(b.Usage)
		if ta != tb {
			return ta > tb
		}
		return a.Key() < b.Key()
	})
}

// stageUsage is one mirrored transcript: a (project, run, stage, agent)
// file's parsed usage, plus the time the window is applied against.
type stageUsage struct {
	Project  string
	Run      string
	Workflow string
	Stage    string
	// When is the stage's last work turn, falling back to the run's last
	// activity; zero when the journal knows neither.
	When  time.Time
	Usage transcript.Usage
}

// scanResult is the filter-independent half of the report: every
// transcript on disk, plus the per-run last activity the run order
// needs.
type scanResult struct {
	stages []stageUsage
	last   map[string]time.Time
}

// scan walks every run's mirrored transcripts and parses them.
//
// A stage's *when* comes from the journal, not the filesystem: git
// committer time of that stage's most recent work turn, falling back to
// the run's last activity. Mtimes would be the obvious choice and are
// worthless here — the bureaucracy is checked out into per-session
// worktrees, so every file's mtime is its checkout time and `--since 1d`
// would report the whole archive.
func scan(root string, warn io.Writer) (*scanResult, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, fmt.Errorf("build journal index: %w", err)
	}
	sc := &scanResult{last: map[string]time.Time{}}
	for _, md := range mds {
		runKey := md.Project + "/" + md.ID
		sc.last[runKey] = idx.LastActivity[runKey]
		for _, stage := range stageDocsOnDisk(root, md) {
			when := idx.WorkTurnTime[run.WorkTurnKey{Project: md.Project, Run: md.ID, Doc: stage}]
			if when.IsZero() {
				when = idx.LastActivity[runKey]
			}
			for _, agent := range []string{"claude", "codex"} {
				path := filepath.Join(root, run.ThreadPathFor(agent, md.Project, md.ID, stage))
				u, ok := readTranscriptUsage(agent, path, warn)
				if !ok {
					continue
				}
				sc.stages = append(sc.stages, stageUsage{
					Project:  md.Project,
					Run:      md.ID,
					Workflow: md.Workflow,
					Stage:    stage,
					When:     when,
					Usage:    u,
				})
			}
		}
	}
	return sc, nil
}

// cachedScan memoizes the walk per root on HEAD, exactly the shape
// run.BuildJournalIndex uses and for the same reason: the scan is a pure
// function of the thread files under root and of the journal index, the
// index is a function of HEAD, and the thread files only reach this root
// on a commit that moves it. Stage turns mirror their transcripts into
// the session worktree (internal/cli/stage.go, workRoot :=
// sess.WorktreePath) and session.Close fast-forwards main onto the
// branch, so a new transcript and a new HEAD arrive together; `moe sync`
// is the other way in and also moves HEAD.
//
// That matters because the walk is the expensive half. Parsing 1.2 GB
// of JSONL takes seconds — fine once in a CLI process, ten times the
// dash's tolerable-latency stance on every page load. Behind the memo a
// page hit costs the run scan and the aggregate, and changing the window
// or the project costs only the aggregate.
//
// `moe usage` neither gains nor loses: one process, one miss, the same
// work as before. Its one visible change is that parse warnings are
// emitted once per HEAD rather than once per Gather, which is the right
// behaviour for a server log and indistinguishable for a single call.
func cachedScan(root string, warn io.Writer) (*scanResult, error) {
	head, err := git.HEAD(root)
	if err != nil {
		// No readable HEAD (unborn branch, not a repo) — nothing to key
		// on. Fall through to the walk, which fails the way callers
		// already handle.
		return scan(root, warn)
	}
	scanMemo.Lock()
	defer scanMemo.Unlock()
	if e, ok := scanMemo.entries[root]; ok && e.head == head {
		return e.res, nil
	}
	res, err := scan(root, warn)
	if err != nil {
		return nil, err
	}
	if scanMemo.entries == nil {
		scanMemo.entries = make(map[string]scanMemoEntry, 1)
	}
	scanMemo.entries[root] = scanMemoEntry{head: head, res: res}
	return res, nil
}

// scanMemo caches the last scan per bureaucracy root. Keyed by root with
// HEAD *inside* the value, so a new commit replaces the entry rather
// than stranding a dead scan per commit; no eviction beyond that,
// because a real process works one root. The lock is held across the
// walk, so two concurrent misses cost one walk and the loser waits it
// out — at single-operator scale that trade is free.
//
// **The returned scanResult is shared between callers and must be
// treated as read-only.** Gather only reads it; a caller that starts
// writing would corrupt every other holder of the same HEAD.
var scanMemo struct {
	sync.Mutex
	entries map[string]scanMemoEntry
}

type scanMemoEntry struct {
	head string
	res  *scanResult
}

// Merge sums two models' usage.
func Merge(a, b transcript.ModelUsage) transcript.ModelUsage {
	a.Input += b.Input
	a.CacheWrite += b.CacheWrite
	a.CacheRead += b.CacheRead
	a.Output += b.Output
	a.Steps += b.Steps
	return a
}

// Input is every token that went in, cache traffic included.
func Input(u transcript.ModelUsage) int64 {
	return u.Input + u.CacheWrite + u.CacheRead
}

// Tokens is the whole two-way count.
func Tokens(u transcript.ModelUsage) int64 {
	return Input(u) + u.Output
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
func readTranscriptUsage(agent, path string, warn io.Writer) (transcript.Usage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	u, err := transcript.ParseUsage(agent, f)
	if err != nil {
		cliout.Printf(warn, "moe usage: %s: %v\n", path, err)
	}
	return u, len(u) > 0
}

// ModelNotional renders one model's cost, or "—" when it has no price on
// record. The dash is the honest answer: the tokens are in the row
// either way.
func ModelNotional(model string, u transcript.ModelUsage) string {
	cost, ok := NotionalCost(model, u)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// PartialNotional reports whether an aggregate mixes priced and unpriced
// tokens, which is what the star on a total means.
func PartialNotional(u transcript.ModelUsage, unpriced int64) bool {
	return unpriced > 0 && unpriced < Tokens(u)
}

// Notional renders an aggregate cost without implying that a partial
// price is complete. A dash means every token in the aggregate is
// unpriced.
func Notional(dollars float64, u transcript.ModelUsage, unpriced int64) string {
	if Tokens(u) > 0 && unpriced == Tokens(u) {
		return "—"
	}
	if PartialNotional(u, unpriced) {
		return fmt.Sprintf("$%.2f*", dollars)
	}
	return fmt.Sprintf("$%.2f", dollars)
}

// HumanTokens renders a token count compactly with a K/M suffix —
// 12.4M reads at a glance where 12437291 does not.
func HumanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
