package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

// seedThread writes a mirrored transcript for one stage and commits it
// with the `work: update <doc>` trailer the journal index reads for the
// stage's timestamp.
func seedThread(t *testing.T, root, projectID, runID, stage, agent, body string) {
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
	gittest.Run(t, root, "commit", "-m",
		"work: update "+stage+"\n\nMoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Document: "+stage+"\n")
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

	rep, err := gatherUsage(root, "", now.Add(-24*time.Hour), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("gatherUsage: %v", err)
	}
	if rep.transcripts != 2 {
		t.Fatalf("transcripts = %d, want 2", rep.transcripts)
	}
	if len(rep.rows) != 2 {
		t.Fatalf("rows = %+v, want one per (stage, model)", rep.rows)
	}
	// Fable design is the pricier bucket, so it sorts first.
	if rep.rows[0].stage != "design" || rep.rows[0].model != "claude-fable-5" {
		t.Errorf("first row = %+v, want the most expensive bucket first", rep.rows[0])
	}
	if got := rep.total.Steps; got != 2 {
		t.Errorf("total steps = %d, want 2 turns (not 4 lines)", got)
	}
	if got := rep.total.Output; got != 9000 {
		t.Errorf("total output = %d, want 9000", got)
	}
	if rep.dollars <= 0 {
		t.Errorf("notional dollars = %v, want a positive figure", rep.dollars)
	}
	if len(rep.unpriced) != 0 {
		t.Errorf("unpriced = %v, want every seeded model priced", rep.unpriced)
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

	rep, err := gatherUsage(root, "", now.Add(1*time.Hour), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("gatherUsage: %v", err)
	}
	if rep.transcripts != 0 {
		t.Fatalf("transcripts = %d, want the out-of-window stage dropped", rep.transcripts)
	}
}

// TestUsageProjectFilter scopes the walk to one project.
func TestUsageProjectFilter(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "tele", "ship-it", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "moe", "other", "sdlc", run.StatusMerged, now, nil)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed runs")
	seedThread(t, root, "tele", "ship-it", "design", "claude", claudeTurn("msg_1", "claude-fable-5", 1, 2, 3))
	seedThread(t, root, "moe", "other", "design", "claude", claudeTurn("msg_2", "claude-fable-5", 1, 2, 3))

	rep, err := gatherUsage(root, "moe", now.Add(-24*time.Hour), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("gatherUsage: %v", err)
	}
	if rep.transcripts != 1 {
		t.Fatalf("transcripts = %d, want only the filtered project", rep.transcripts)
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

	rep, err := gatherUsage(root, "", now.Add(-24*time.Hour), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("gatherUsage: %v", err)
	}
	if rep.total.Output != 30 {
		t.Errorf("total output = %d, want the tokens counted anyway", rep.total.Output)
	}
	if rep.dollars != 0 {
		t.Errorf("dollars = %v, want no invented figure", rep.dollars)
	}
	if rep.unpriced["some-unlisted-model"] == 0 {
		t.Errorf("unpriced = %v, want the gap named", rep.unpriced)
	}
	var buf bytes.Buffer
	renderUsage(&buf, rep, "", "30d")
	if !strings.Contains(buf.String(), "no price on record for some-unlisted-model") {
		t.Errorf("render = %q, want the missing price surfaced", buf.String())
	}
}

// TestNotionalCostArithmetic pins the price formula against a hand
// figure: 1M cache writes at Opus 4.8's $5/MTok input rate is $5 × 1.25,
// 1M cache reads is $5 × 0.10, and 1M output is $25.
func TestNotionalCostArithmetic(t *testing.T) {
	got, ok := notionalCost("claude-opus-4-8", transcript.ModelUsage{
		CacheWrite: 1_000_000, CacheRead: 1_000_000, Output: 1_000_000,
	})
	if !ok {
		t.Fatal("opus-4-8 must be in the price map")
	}
	want := 6.25 + 0.50 + 25.0
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
	p, ok := priceFor("claude-opus-4-8[1m]")
	if !ok || p.input != 5 {
		t.Errorf("priceFor(context-tagged id) = %+v ok=%v, want the opus-4-8 entry", p, ok)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1500: "2K", 12_437_291: "12.4M"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestUsageCommandEmptyBureaucracy: the verb exits clean and says so
// when there is nothing in the window, rather than printing a bare
// header over an empty table.
func TestUsageCommandEmptyBureaucracy(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := Run([]string{"usage"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "No stage transcripts in the window.") {
		t.Errorf("stdout = %q, want the empty case named", out.String())
	}
}

// TestUsageCommandRejectsBadSince keeps the flag honest — a typo'd
// window should be a usage error, not a silent zero-length window that
// reports nothing.
func TestUsageCommandRejectsBadSince(t *testing.T) {
	root := newTestBureaucracy(t)
	t.Setenv("MOE_HOME", root)
	var out, errb bytes.Buffer
	if code := Run([]string{"usage", "--since", "banana"}, &out, &errb); code != 2 {
		t.Fatalf("exit=%d, want 2 for a malformed --since", code)
	}
}
