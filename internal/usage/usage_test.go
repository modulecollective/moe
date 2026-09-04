package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

// newTestBureaucracy initializes a throwaway git repo with scoped git
// config, so commits can happen without polluting ~/.gitconfig.
func newTestBureaucracy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}

// seedRun writes a run.json (and optional canvases) straight to disk —
// run.Scan reads nothing else, and going through run.New would drag in
// the whole open pipeline for a fixture.
func seedRun(t *testing.T, root, projectID, id, workflow, status string, created time.Time, canvases map[string]string) {
	t.Helper()
	md := run.Metadata{
		ID:        id,
		Project:   projectID,
		Status:    status,
		Workflow:  workflow,
		Created:   created.Format("2006-01-02"),
		Documents: map[string]*run.Document{},
	}
	for docID, body := range canvases {
		md.Documents[docID] = &run.Document{}
		rel := run.ContentPath(projectID, id, docID)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, run.Dir(projectID, id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedThread writes a mirrored transcript for one stage and commits it
// with the `work: update <doc>` trailer the journal index reads for the
// stage's timestamp.
func seedThread(t *testing.T, root, projectID, runID, stage, agent, body string) {
	seedThreadAt(t, root, projectID, runID, stage, agent, body, time.Time{})
}

func seedThreadAt(t *testing.T, root, projectID, runID, stage, agent, body string, when time.Time) {
	t.Helper()
	rel := run.ThreadPathFor(agent, projectID, runID, stage)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	args := []string{"commit", "-m",
		"work: update " + stage + "\n\nMoE-Run: " + runID + "\nMoE-Project: " + projectID + "\nMoE-Document: " + stage + "\n"}
	if when.IsZero() {
		gittest.Run(t, root, args...)
		return
	}
	date := when.Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, args...)
}

// claudeTurn is one assistant turn's worth of thread-claude.jsonl,
// written as two lines sharing a message id — the per-content-block
// shape Claude Code actually emits.
func claudeTurn(id, model string, cacheWrite, cacheRead, output int) string {
	line := fmt.Sprintf(
		`{"type":"assistant","message":{"id":%q,"model":%q,"content":[{"type":"text","text":"x"}],`+
			`"usage":{"input_tokens":1,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`,
		id, model, cacheWrite, cacheRead, output)
	return line + "\n" + line + "\n"
}

func codexTurn(model string, input, cached, output int) string {
	return fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}
`, model, input, cached, output)
}

// gather is Gather with the memo bypassed, so each fixture in this file
// sees the tree it just wrote rather than a sibling test's scan. The
// memo itself is covered by TestScanMemoizedOnHEAD.
func gather(t *testing.T, root string, f Filter) Report {
	t.Helper()
	forgetScan(root)
	rep, err := Gather(root, f, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return rep
}

func forgetScan(root string) {
	scanMemo.Lock()
	defer scanMemo.Unlock()
	delete(scanMemo.entries, root)
}

func runByKey(rep Report, key string) (Run, bool) {
	for _, r := range rep.Runs {
		if r.Key() == key {
			return r, true
		}
	}
	return Run{}, false
}

// TestUsageBucketsByWorkflowStageModel is the aggregator end to end over
// a seeded bureaucracy: two stages of one run on two models land in two
// buckets, and the per-turn dedupe survives the walk (each turn is two
// lines on disk).
func TestUsageBucketsByWorkflowStageModel(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "ship-it", "sdlc", run.StatusMerged, now, nil)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed run")
	seedThread(t, root, "tele", "ship-it", "design", "claude",
		claudeTurn("msg_1", "claude-fable-5", 1000, 200000, 5000))
	seedThread(t, root, "tele", "ship-it", "code", "claude",
		claudeTurn("msg_2", "claude-opus-4-8", 2000, 100000, 4000))

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if rep.Transcripts != 2 {
		t.Fatalf("transcripts = %d, want 2", rep.Transcripts)
	}
	if len(rep.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want one per (stage, model)", rep.Buckets)
	}
	// Fable design is the pricier bucket, so it sorts first.
	if rep.Buckets[0].Stage != "design" || rep.Buckets[0].Model != "claude-fable-5" {
		t.Errorf("first bucket = %+v, want the most expensive first", rep.Buckets[0])
	}
	if got := rep.Buckets[0].Runs; got != 1 {
		t.Errorf("bucket runs = %d, want 1", got)
	}
	if got := rep.Total.Steps; got != 2 {
		t.Errorf("total steps = %d, want 2 turns (not 4 lines)", got)
	}
	if got := rep.Total.Output; got != 9000 {
		t.Errorf("total output = %d, want 9000", got)
	}
	if rep.Dollars <= 0 {
		t.Errorf("notional dollars = %v, want a positive figure", rep.Dollars)
	}
	if len(rep.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want every seeded model priced", rep.Unpriced)
	}
}

// TestUsageSinceWindowDropsOlderStages: the window keys on the journal's
// committer time for the stage's work turn, not on file mtimes — the
// bureaucracy is checked out into per-session worktrees, where every
// mtime is the checkout's.
func TestUsageSinceWindowDropsOlderStages(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "ship-it", "sdlc", run.StatusMerged, now, nil)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed run")
	seedThread(t, root, "tele", "ship-it", "design", "claude",
		claudeTurn("msg_1", "claude-fable-5", 1000, 2000, 300))

	rep := gather(t, root, Filter{Cutoff: now.Add(1 * time.Hour)})
	if rep.Transcripts != 0 {
		t.Fatalf("transcripts = %d, want the out-of-window stage dropped", rep.Transcripts)
	}
}

// TestUsageProjectFilter scopes the report to one project.
func TestUsageProjectFilter(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "ship-it", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "moe", "other", "sdlc", run.StatusMerged, now, nil)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed runs")
	seedThread(t, root, "tele", "ship-it", "design", "claude", claudeTurn("msg_1", "claude-fable-5", 1, 2, 3))
	seedThread(t, root, "moe", "other", "design", "claude", claudeTurn("msg_2", "claude-fable-5", 1, 2, 3))

	rep := gather(t, root, Filter{Project: "moe", Cutoff: now.Add(-24 * time.Hour)})
	if rep.Transcripts != 1 {
		t.Fatalf("transcripts = %d, want only the filtered project", rep.Transcripts)
	}
}

func TestUsageByRunGroupsQualifiedRunsAndConservesTotals(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "alpha", "shared", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "beta", "shared", "chat", run.StatusClosed, now, nil)
	seedRun(t, root, "gamma", "small", "sdlc", run.StatusMerged, now, nil)
	gittest.Commit(t, root, "seed runs")
	seedThread(t, root, "alpha", "shared", "design", "claude",
		claudeTurn("a1", "claude-fable-5", 100, 200, 300)+
			claudeTurn("a2", "claude-opus-4-8", 10, 20, 30))
	seedThread(t, root, "alpha", "shared", "design", "codex",
		codexTurn("gpt-5.6-sol", 100, 80, 10))
	seedThread(t, root, "alpha", "shared", "code", "claude",
		claudeTurn("a3", "claude-fable-5", 40, 50, 60))
	seedThread(t, root, "beta", "shared", "design", "claude",
		claudeTurn("b1", "claude-fable-5", 5, 6, 7))
	seedThread(t, root, "gamma", "small", "design", "claude",
		claudeTurn("g1", "claude-fable-5", 5, 6, 7))

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if len(rep.Runs) != 3 {
		t.Fatalf("runs = %+v, want three qualified runs", rep.Runs)
	}
	alpha, ok := runByKey(rep, "alpha/shared")
	if !ok || alpha.Stages != 2 || alpha.Usage.Steps != 4 {
		t.Errorf("alpha/shared = %+v ok=%v, want two stages merged across backends and models", alpha, ok)
	}
	if _, ok := runByKey(rep, "beta/shared"); !ok {
		t.Errorf("runs = %+v, want beta/shared listed", rep.Runs)
	}
	if _, ok := runByKey(rep, "gamma/small"); !ok {
		t.Errorf("runs = %+v, want gamma/small listed", rep.Runs)
	}
	var sum transcript.ModelUsage
	for _, r := range rep.Runs {
		sum = Merge(sum, r.Usage)
	}
	if sum != rep.Total {
		t.Errorf("sum of run usage = %+v, aggregate = %+v", sum, rep.Total)
	}
}

// TestUsageRunOrderIsRecencyThenUntimed pins the page's default order:
// last activity, newest first, with a run the journal has no commit for
// at the bottom rather than sorted as a 1970 zero time.
func TestUsageRunOrderIsRecencyThenUntimed(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "older", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "tele", "newer", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "tele", "undated", "sdlc", run.StatusMerged, now,
		map[string]string{"design": ""})
	gittest.Commit(t, root, "seed runs")
	seedThreadAt(t, root, "tele", "older", "design", "claude",
		claudeTurn("o1", "claude-fable-5", 1, 2, 3), now.Add(-6*time.Hour))
	seedThreadAt(t, root, "tele", "newer", "design", "claude",
		claudeTurn("n1", "claude-fable-5", 1, 2, 3), now.Add(-1*time.Hour))
	// Written but never committed: nothing in the journal dates it.
	abs := filepath.Join(root, run.ThreadPathFor("claude", "tele", "undated", "design"))
	if err := os.WriteFile(abs, []byte(claudeTurn("u1", "claude-fable-5", 1, 2, 3)), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	var got []string
	for _, r := range rep.Runs {
		got = append(got, r.Key())
	}
	want := []string{"tele/newer", "tele/older", "tele/undated"}
	if len(got) != len(want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runs = %v, want %v", got, want)
		}
	}
	if !rep.Runs[2].Last.IsZero() {
		t.Errorf("undated run Last = %v, want the zero time", rep.Runs[2].Last)
	}
}

func TestUsageCutoffKeepsOnlyInWindowStagesInRunView(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "split", "sdlc", run.StatusMerged, now, nil)
	gittest.Commit(t, root, "seed run")
	seedThreadAt(t, root, "tele", "split", "design", "claude",
		claudeTurn("old", "claude-fable-5", 1, 2, 300), now.Add(-48*time.Hour))
	seedThreadAt(t, root, "tele", "split", "code", "claude",
		claudeTurn("new", "claude-fable-5", 1, 2, 30), now.Add(-time.Hour))

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if rep.Transcripts != 1 || len(rep.Runs) != 1 {
		t.Fatalf("report = %+v, want one in-window transcript and run", rep)
	}
	if got := rep.Runs[0]; got.Stages != 1 || got.Usage.Output != 30 {
		t.Errorf("run row = %+v, want only the in-window code stage", got)
	}
}

// TestUsageUnknownModelKeepsTokensDropsDollars: a model with no price on
// record still contributes its tokens; only the dollar column abstains.
// Inventing a rate would be worse than admitting the gap.
func TestUsageUnknownModelKeepsTokensDropsDollars(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "ship-it", "sdlc", run.StatusMerged, now, nil)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed run")
	seedThread(t, root, "tele", "ship-it", "design", "claude",
		claudeTurn("msg_1", "some-unlisted-model", 10, 20, 30))

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if rep.Total.Output != 30 {
		t.Errorf("total output = %d, want the tokens counted anyway", rep.Total.Output)
	}
	if rep.Dollars != 0 {
		t.Errorf("dollars = %v, want no invented figure", rep.Dollars)
	}
	if rep.Unpriced["some-unlisted-model"] == 0 {
		t.Errorf("unpriced = %v, want the gap named", rep.Unpriced)
	}
	if got := Notional(rep.Dollars, rep.Total, rep.UnpricedTokens()); got != "—" {
		t.Errorf("total notional = %q, want a dash when nothing is priced", got)
	}
	if rep.Starred() {
		t.Errorf("Starred() = true, want false when no token is priced")
	}
}

func TestUsageMixedPriceTotalsAreStarred(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "mixed", "sdlc", run.StatusMerged, now, nil)
	gittest.Commit(t, root, "seed run")
	seedThread(t, root, "tele", "mixed", "design", "claude",
		claudeTurn("priced", "claude-fable-5", 0, 0, 1_000_000)+
			claudeTurn("unknown", "some-unlisted-model", 0, 0, 1_000_000))

	rep := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if !rep.Starred() {
		t.Fatalf("Starred() = false, want a partial total starred")
	}
	if got := Notional(rep.Dollars, rep.Total, rep.UnpricedTokens()); got != "$50.00*" {
		t.Errorf("total notional = %q, want $50.00*", got)
	}
	if got := rep.Runs[0]; Notional(got.Dollars, got.Usage, got.Unpriced) != "$50.00*" {
		t.Errorf("run notional = %+v, want the star to reach the run row", got)
	}
}

func TestUsageUntimedTranscriptStaysOutOfByDay(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "untimed", "sdlc", run.StatusMerged, now,
		map[string]string{"design": ""})
	abs := filepath.Join(root, run.ThreadPathFor("claude", "tele", "untimed", "design"))
	if err := os.WriteFile(abs, []byte(claudeTurn("u1", "claude-fable-5", 1, 2, 3)), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := gather(t, root, Filter{Cutoff: now.Add(365 * 24 * time.Hour)})
	if rep.Transcripts != 1 || rep.Untimed != 1 || len(rep.Runs) != 1 || len(rep.ByDay) != 0 {
		t.Errorf("report = %+v, want untimed usage in totals/run but not by-day", rep)
	}
}

// TestScanMemoizedOnHEAD is the whole argument for the cache key: the
// scan is a pure function of HEAD, so a transcript that lands without a
// commit is invisible until one moves HEAD. Same shape as
// TestJournalIndexMemoizedOnHEAD.
func TestScanMemoizedOnHEAD(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "memo", "sdlc", run.StatusMerged, now, nil)
	gittest.Commit(t, root, "seed run")
	seedThread(t, root, "tele", "memo", "design", "claude",
		claudeTurn("m1", "claude-fable-5", 1, 2, 3))

	f := Filter{Cutoff: now.Add(-24 * time.Hour)}
	first := gather(t, root, f)
	if first.Transcripts != 1 {
		t.Fatalf("transcripts = %d, want 1", first.Transcripts)
	}

	// A second transcript on disk but not in a commit: HEAD hasn't
	// moved, so the memo is still authoritative.
	rel := run.ThreadPathFor("claude", "tele", "memo", "code")
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(claudeTurn("m2", "claude-fable-5", 4, 5, 6)), 0o644); err != nil {
		t.Fatal(err)
	}
	warm, err := Gather(root, f, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if warm.Transcripts != 1 {
		t.Errorf("transcripts = %d, want the memo to answer without re-walking", warm.Transcripts)
	}

	// Commit it and HEAD moves, so the next Gather rebuilds.
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m",
		"work: update code\n\nMoE-Run: memo\nMoE-Project: tele\nMoE-Document: code\n")
	fresh, err := Gather(root, f, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if fresh.Transcripts != 2 {
		t.Errorf("transcripts = %d, want the new commit to invalidate the memo", fresh.Transcripts)
	}
}

// TestGatherReusesScanAcrossFilters: the memo is what makes the page's
// project and window controls cheap — changing either must not re-walk.
func TestGatherReusesScanAcrossFilters(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "alpha", "one", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "beta", "two", "sdlc", run.StatusMerged, now, nil)
	gittest.Commit(t, root, "seed runs")
	seedThread(t, root, "alpha", "one", "design", "claude", claudeTurn("a", "claude-fable-5", 1, 2, 3))
	seedThread(t, root, "beta", "two", "design", "claude", claudeTurn("b", "claude-fable-5", 1, 2, 3))

	all := gather(t, root, Filter{Cutoff: now.Add(-24 * time.Hour)})
	if all.Transcripts != 2 {
		t.Fatalf("transcripts = %d, want 2", all.Transcripts)
	}
	scanMemo.Lock()
	cached, ok := scanMemo.entries[root]
	scanMemo.Unlock()
	if !ok {
		t.Fatal("first Gather left no memo entry")
	}
	scoped, err := Gather(root, Filter{Project: "beta", Cutoff: now.Add(-24 * time.Hour)}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if scoped.Transcripts != 1 || scoped.Runs[0].Key() != "beta/two" {
		t.Errorf("scoped report = %+v, want beta alone", scoped)
	}
	scanMemo.Lock()
	after := scanMemo.entries[root]
	scanMemo.Unlock()
	if after.res != cached.res {
		t.Error("a filter change rebuilt the scan; it must aggregate over the cached one")
	}
}

// TestScanIsBoundedByTheWindow is the other half of the memo key: the
// window bounds the walk, so the default `moe usage` doesn't pay to
// parse the whole archive. A narrow scan holds only what its window
// covers; a wider one misses and re-parses; and a narrow request after
// that hits the wider entry, because a narrower window is a subset.
func TestScanIsBoundedByTheWindow(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	old := now.Add(-60 * 24 * time.Hour)
	seedRun(t, root, "tele", "recent", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "tele", "ancient", "sdlc", run.StatusMerged, old, nil)
	gittest.Commit(t, root, "seed runs")
	seedThreadAt(t, root, "tele", "recent", "design", "claude",
		claudeTurn("r", "claude-fable-5", 1, 2, 3), now)
	seedThreadAt(t, root, "tele", "ancient", "design", "claude",
		claudeTurn("a", "claude-fable-5", 1, 2, 3), old)

	narrow := Filter{Cutoff: now.Add(-24 * time.Hour)}
	wide := Filter{Cutoff: now.Add(-90 * 24 * time.Hour)}

	if got := gather(t, root, narrow).Transcripts; got != 1 {
		t.Fatalf("narrow transcripts = %d, want the recent run alone", got)
	}
	scanMemo.Lock()
	first := scanMemo.entries[root]
	scanMemo.Unlock()
	// The discriminating assert: without a cutoff in scan, the walk
	// would have parsed the ancient transcript too and left two stages
	// here, with Gather doing the dropping after the cost was paid.
	if got := len(first.res.stages); got != 1 {
		t.Errorf("narrow scan holds %d stages, want only the one in the window", got)
	}

	widened, err := Gather(root, wide, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if widened.Transcripts != 2 {
		t.Errorf("wide transcripts = %d, want the wider window to re-parse", widened.Transcripts)
	}
	scanMemo.Lock()
	second := scanMemo.entries[root]
	scanMemo.Unlock()
	if second.res == first.res {
		t.Fatal("a wider window reused the narrow scan; it can't, the records aren't there")
	}

	renarrowed, err := Gather(root, narrow, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if renarrowed.Transcripts != 1 {
		t.Errorf("re-narrowed transcripts = %d, want the recent run alone", renarrowed.Transcripts)
	}
	scanMemo.Lock()
	third := scanMemo.entries[root]
	scanMemo.Unlock()
	if third.res != second.res {
		t.Error("a narrower window rebuilt the scan; the wider entry is a superset of it")
	}
}

// TestNotionalCostArithmetic pins the price formula against a hand
// figure: 1M cache writes at Opus 4.8's $5/MTok input rate is $5 × 2 (a
// 1-hour-TTL write), 1M cache reads is $5 × 0.10, and 1M output is $25.
func TestNotionalCostArithmetic(t *testing.T) {
	got, ok := NotionalCost("claude-opus-4-8", transcript.ModelUsage{
		CacheWrite: 1_000_000, CacheRead: 1_000_000, Output: 1_000_000,
	})
	if !ok {
		t.Fatal("opus-4-8 must be in the price map")
	}
	want := 10.0 + 0.50 + 25.0
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// TestPriceForLongestPrefixWins: model ids carry date suffixes, so the
// map matches by prefix — and a more specific entry must beat a shorter
// one that also matches.
func TestPriceForLongestPrefixWins(t *testing.T) {
	if _, ok := priceFor("claude-haiku-4-5-20251001"); !ok {
		t.Error("a dated model id must still match its family prefix")
	}
	if _, ok := priceFor("claude-opus"); ok {
		t.Error("a prefix shorter than every entry must not match")
	}
	// The bureaucracy's largest single share of tokens; without an entry
	// every daily and per-run total in the report goes starred.
	if p, ok := priceFor("claude-opus-5"); !ok || p.input != 5 || p.output != 25 {
		t.Errorf("priceFor(claude-opus-5) = %+v ok=%v, want $5/$25", p, ok)
	}
	p, ok := priceFor("claude-opus-4-8[1m]")
	if !ok || p.input != 5 {
		t.Errorf("priceFor(context-tagged id) = %+v ok=%v, want the opus-4-8 entry", p, ok)
	}
	// The codex stages run these; without an entry their rows go unpriced.
	if p, ok := priceFor("gpt-5.6-sol"); !ok || p.input != 5 || p.output != 30 {
		t.Errorf("priceFor(gpt-5.6-sol) = %+v ok=%v, want $5/$30", p, ok)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1500: "2K", 12_437_291: "12.4M"}
	for n, want := range cases {
		if got := HumanTokens(n); got != want {
			t.Errorf("HumanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
