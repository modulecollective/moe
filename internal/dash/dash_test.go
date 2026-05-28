package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildActive runs BuildRows over the given runs and returns the ACTIVE
// rows in their final (grouped) order. when is keyed by run slug;
// chained is the ChainedChild edge map keyed by "<project>/<slug>".
func buildActive(t *testing.T, runs []*run.Metadata, when map[string]time.Time, chained map[string]string) []Row {
	t.Helper()
	next := make(map[string]NextDecision)
	for _, md := range runs {
		if md.Status == run.StatusInProgress && md.Workflow != IdeaWorkflow {
			next[md.ID] = NextDecision{Stage: "code"}
		}
	}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	rows, err := BuildRows(Inputs{Now: time.Now().UTC(), Runs: runs, Index: idx, NextByRun: next})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var active []Row
	for _, r := range rows {
		if r.Bucket == BucketActiveRuns {
			active = append(active, r)
		}
	}
	return active
}

func activeRun(project, id string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: "sdlc", Status: run.StatusInProgress}
}

func assertOrder(t *testing.T, active []Row, want ...string) {
	t.Helper()
	var got []string
	for _, r := range active {
		got = append(got, r.Project+"/"+r.Run)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("active order = %v, want %v", got, want)
	}
}

func TestActiveChainGroupsHeadToTailAndFloats(t *testing.T) {
	// Chain a→b→c floats by its most-recent member (c, 14:00) above the
	// standalone x (13:00), even though the head a (10:00) is older.
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{
		activeRun("p", "a"), activeRun("p", "b"), activeRun("p", "c"), activeRun("p", "x"),
	}
	when := map[string]time.Time{
		"a": base.Add(-4 * time.Hour),
		"b": base.Add(-2 * time.Hour),
		"c": base,
		"x": base.Add(-1 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	assertOrder(t, active, "p/a", "p/b", "p/c", "p/x")
}

func TestActiveChainConnectorFlags(t *testing.T) {
	// Members get Member=true; head and singleton stay false.
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{activeRun("p", "a"), activeRun("p", "b"), activeRun("p", "c"), activeRun("p", "x")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour), "c": base.Add(-2 * time.Hour), "x": base.Add(-3 * time.Hour)}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	want := map[string]bool{"p/a": false, "p/b": true, "p/c": true, "p/x": false}
	for _, r := range active {
		key := r.Project + "/" + r.Run
		if r.Member != want[key] {
			t.Errorf("%s Member = %v, want %v", key, r.Member, want[key])
		}
	}
}

func TestActiveChainHintAdjacentSuppressedFanInRetained(t *testing.T) {
	// Fan-in: a and b both chain to c. a (newer) adopts c adjacently and
	// drops its text hint; b ends as a singleton and keeps "· chained → p/c".
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{activeRun("p", "a"), activeRun("p", "b"), activeRun("p", "c")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour), "c": base.Add(-4 * time.Hour)}
	chained := map[string]string{"p/a": "p/c", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	assertOrder(t, active, "p/a", "p/c", "p/b")
	byKey := map[string]Row{}
	for _, r := range active {
		byKey[r.Project+"/"+r.Run] = r
	}
	if !byKey["p/c"].Member {
		t.Errorf("p/c should be a connected member")
	}
	if strings.Contains(byKey["p/a"].Note, "chained →") {
		t.Errorf("adjacent parent p/a should not carry a text hint, got %q", byKey["p/a"].Note)
	}
	if !strings.Contains(byKey["p/b"].Note, "· chained → p/c") {
		t.Errorf("fan-in second parent p/b should keep its text hint, got %q", byKey["p/b"].Note)
	}
}

func TestActiveTerminalParentChildIsHead(t *testing.T) {
	// Parent a is merged (terminal, leaves the active set); its live child
	// b heads its own visible chain b→c. b must render flush-left (head),
	// not as a member.
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	merged := &run.Metadata{ID: "a", Project: "p", Workflow: "sdlc", Status: run.StatusMerged}
	runs := []*run.Metadata{merged, activeRun("p", "b"), activeRun("p", "c")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour), "c": base.Add(-2 * time.Hour)}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	assertOrder(t, active, "p/b", "p/c")
	if active[0].Member {
		t.Errorf("terminal-parent child p/b should be a head, not a member")
	}
	if !active[1].Member {
		t.Errorf("p/c should be a connected member under p/b")
	}
}

func TestActiveStandaloneBetweenChainsByRecency(t *testing.T) {
	// Two chains with a standalone whose recency falls between them: it
	// stays ungrouped and lands between the chain units by representative time.
	base := time.Date(2026, 5, 28, 15, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{
		activeRun("p", "a"), activeRun("p", "b"), // chain 1, rep = b (15:00)
		activeRun("p", "d"), activeRun("p", "e"), // chain 2, rep = e (10:00)
		activeRun("p", "x"), // standalone, 12:00
	}
	when := map[string]time.Time{
		"a": base.Add(-6 * time.Hour), "b": base,
		"d": base.Add(-7 * time.Hour), "e": base.Add(-5 * time.Hour),
		"x": base.Add(-3 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/d": "p/e"}
	active := buildActive(t, runs, when, chained)
	assertOrder(t, active, "p/a", "p/b", "p/x", "p/d", "p/e")
	if active[2].Member {
		t.Errorf("standalone p/x should be ungrouped")
	}
}

func TestChainHintSameProjectQualified(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	child := &run.Metadata{ID: "next-fix", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/next-fix"}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent, "p/next-fix": child}
	if got, want := chainHint(idx, parent, byKey), " · chained → p/next-fix"; got != want {
		t.Errorf("same-project hint = %q, want %q", got, want)
	}
}

func TestChainHintCrossProjectQualified(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "a", Workflow: "sdlc", Status: run.StatusInProgress}
	child := &run.Metadata{ID: "next-fix", Project: "b", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"a/fix-bug": "b/next-fix"}}
	byKey := map[string]*run.Metadata{"a/fix-bug": parent, "b/next-fix": child}
	if got, want := chainHint(idx, parent, byKey), " · chained → b/next-fix"; got != want {
		t.Errorf("cross-project hint = %q, want %q", got, want)
	}
}

func TestChainHintSuppressesTerminalChild(t *testing.T) {
	// Decision 1: terminal children are filtered at read time. The
	// trailer still lives in history; the dash row must not advertise
	// a chain that wouldn't fire on the ride.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	for _, status := range []string{run.StatusClosed, run.StatusMerged, run.StatusPromoted, run.StatusPushed} {
		child := &run.Metadata{ID: "next-fix", Project: "p", Workflow: "sdlc", Status: status}
		idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/next-fix"}}
		byKey := map[string]*run.Metadata{"p/fix-bug": parent, "p/next-fix": child}
		if got := chainHint(idx, parent, byKey); got != "" {
			t.Errorf("terminal child (%s) hint = %q, want empty", status, got)
		}
	}
}

func TestChainHintNoEdge(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("no edge: hint = %q, want empty", got)
	}
}

func TestChainHintClearedEdgeSuppressed(t *testing.T) {
	// A cleared edge pins ChainedChild[parent] = "" in the index
	// (so an older Chained-To can't re-assert it). The hint must
	// read empty, not show "chained → " with a dangling pointer.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": ""}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("cleared edge: hint = %q, want empty", got)
	}
}

func TestChainHintChildMissingFromDisk(t *testing.T) {
	// Trailer references a child that doesn't exist on disk (race
	// with delete, or a hand-edited trailer). Hint must read empty
	// rather than dangle.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/ghost"}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("ghost child: hint = %q, want empty", got)
	}
}
