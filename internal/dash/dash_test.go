package dash

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildActive runs BuildRows over the given runs and returns the ACTIVE
// rows in their final (grouped) order. when is keyed by bare run slug
// (callers here are all in one project) and re-qualified to
// "<project>/<slug>" internally, matching the keys BuildRows now uses;
// chained is the ChainedChild edge map keyed by "<project>/<slug>".
func buildActive(t *testing.T, runs []*run.Metadata, when map[string]time.Time, chained map[string]string) []Row {
	t.Helper()
	next := make(map[string]NextDecision)
	qWhen := make(map[string]time.Time, len(when))
	for _, md := range runs {
		key := md.Project + "/" + md.ID
		if md.Status == run.StatusInProgress && md.Workflow != IdeaWorkflow {
			next[key] = NextDecision{Stage: "code"}
		}
		if w, ok := when[md.ID]; ok {
			qWhen[key] = w
		}
	}
	idx := &run.JournalIndex{LastActivity: qWhen, ChainedChild: chained}
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

// TestRenderPrintsHistogramAboveFactoryArt guards the CLI integration
// seam: Render emits the pre-built histogram lines before the factory
// art, which in turn precedes the section headers.
func TestRenderPrintsHistogramAboveFactoryArt(t *testing.T) {
	var buf strings.Builder
	hist := []string{"  ▂▄█", "", "  activity · last 60 days        peak 3 runs/day"}
	Render(&buf, time.Now().UTC(), hist, nil, 0, 0, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := buf.String()
	capIdx := strings.Index(out, "activity · last 60 days")
	activeIdx := strings.Index(out, "ACTIVE")
	if capIdx < 0 {
		t.Fatalf("histogram caption missing from render:\n%s", out)
	}
	if activeIdx < 0 || capIdx > activeIdx {
		t.Fatalf("histogram must precede the ACTIVE header:\n%s", out)
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
	// Chain members are marked Chained; head and singleton are not. A
	// chain never indents — b and c are peer stages, both at Depth 0 —
	// unlike spawn lineage, which deepens Depth per generation.
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{activeRun("p", "a"), activeRun("p", "b"), activeRun("p", "c"), activeRun("p", "x")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour), "c": base.Add(-2 * time.Hour), "x": base.Add(-3 * time.Hour)}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	want := map[string]bool{"p/a": false, "p/b": true, "p/c": true, "p/x": false}
	for _, r := range active {
		key := r.Project + "/" + r.Run
		if r.Chained != want[key] {
			t.Errorf("%s Chained = %v, want %v", key, r.Chained, want[key])
		}
		if r.Depth != 0 {
			t.Errorf("%s Depth = %d, want 0 (chains never indent)", key, r.Depth)
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
	if !byKey["p/c"].Chained {
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
	// b heads its own visible unit b→c, and the unit's grouping is
	// unaffected by a's departure.
	//
	// b does carry the connector, though — the assertion here used to be
	// the reverse. A settled predecessor is still a predecessor: the thread
	// is executing and b is what runs next, so the row says so, with the
	// note naming the run it follows (a bare arrow would read as attaching
	// to whatever unrelated row sits above).
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	merged := &run.Metadata{ID: "a", Project: "p", Workflow: "sdlc", Status: run.StatusMerged}
	runs := []*run.Metadata{merged, activeRun("p", "b"), activeRun("p", "c")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour), "c": base.Add(-2 * time.Hour)}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c"}
	active := buildActive(t, runs, when, chained)
	assertOrder(t, active, "p/b", "p/c")
	if !active[0].Chained {
		t.Errorf("p/b follows a settled parent and should draw the connector")
	}
	if !strings.Contains(active[0].Note, "chained after p/a (merged)") {
		t.Errorf("p/b Note = %q, want the settled-parent hint", active[0].Note)
	}
	if !active[1].Chained {
		t.Errorf("p/c should be a connected member under p/b")
	}
	if strings.Contains(active[1].Note, "chained after") {
		t.Errorf("p/c carries a settled-parent hint it has no claim to: %q", active[1].Note)
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
	if active[2].Chained {
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

// rowsByKey runs BuildRows and returns the rows keyed by
// "<project>/<run>" so a test can assert one run's bucket and note
// without depending on section order.
// next and sessionDocs are keyed by bare slug for callsite brevity and
// re-qualified to "<project>/<slug>" here to match BuildRows' keys.
func rowsByKey(t *testing.T, runs []*run.Metadata, next map[string]NextDecision, sessionDocs map[string][]string) map[string]Row {
	t.Helper()
	qNext := make(map[string]NextDecision, len(next))
	qSession := make(map[string][]string, len(sessionDocs))
	for _, md := range runs {
		key := md.Project + "/" + md.ID
		if dec, ok := next[md.ID]; ok {
			qNext[key] = dec
		}
		if docs, ok := sessionDocs[md.ID]; ok {
			qSession[key] = docs
		}
	}
	rows, err := BuildRows(Inputs{
		Now:              time.Now().UTC(),
		Runs:             runs,
		Index:            &run.JournalIndex{},
		NextByRun:        qNext,
		SessionDocsByRun: qSession,
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	m := make(map[string]Row, len(rows))
	for _, r := range rows {
		m[r.Project+"/"+r.Run] = r
	}
	return m
}

// TestChatInProgressRendersOpenResumeNeverDone is the core of the
// "chat is never done" change: whether the operator has never chatted
// (next stage "chat") or has walked the single stage (Done), an
// in-progress chat renders one resumable ACTIVE row — never the
// `done · close?` nag that fires for multi-stage workflows.
func TestChatInProgressRendersOpenResumeNeverDone(t *testing.T) {
	md := &run.Metadata{ID: "ponder", Project: "moe", Workflow: ChatWorkflow, Status: run.StatusInProgress}
	for name, dec := range map[string]NextDecision{
		"never-chatted": {Stage: ChatDocID, Perpetual: true},
		"chatted-done":  {Stage: ChatDocID, Done: true, Perpetual: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, ok := rowsByKey(t, []*run.Metadata{md}, map[string]NextDecision{"ponder": dec}, nil)["moe/ponder"]
			if !ok {
				t.Fatal("chat run missing from rows")
			}
			if r.Bucket != BucketActiveRuns {
				t.Fatalf("bucket=%v want ACTIVE", r.Bucket)
			}
			if r.Note != "chat:open · resume?" {
				t.Fatalf("note=%q want %q", r.Note, "chat:open · resume?")
			}
			if strings.Contains(r.Note, "done") {
				t.Fatalf("chat note must never say done: %q", r.Note)
			}
			if r.Stage != ChatDocID {
				t.Fatalf("stage=%q want %q (factory-art glyph)", r.Stage, ChatDocID)
			}
		})
	}
}

// TestChatInProgressPreservesRunningMarker: a chat row with an open
// session keeps the live `[running]` marker, same as any active row.
func TestChatInProgressPreservesRunningMarker(t *testing.T) {
	md := &run.Metadata{ID: "ponder", Project: "moe", Workflow: ChatWorkflow, Status: run.StatusInProgress}
	r := rowsByKey(t, []*run.Metadata{md},
		map[string]NextDecision{"ponder": {Stage: ChatDocID, Perpetual: true}},
		map[string][]string{"ponder": {ChatDocID}})["moe/ponder"]
	if r.Note != "chat:open · resume? [running]" {
		t.Fatalf("note=%q want %q", r.Note, "chat:open · resume? [running]")
	}
	if r.RunningDoc != ChatDocID {
		t.Fatalf("runningDoc=%q want %q", r.RunningDoc, ChatDocID)
	}
}

// TestPerpetualDoneRendersRepeatableStageNotCloseNag exercises the
// generic perpetual-done branch: a non-chat perpetual workflow whose
// stages are all walked renders its repeatable stage (`wf:stage`), not
// the `· close?` nag. No such workflow ships today (chat, the one
// perpetual workflow, is caught by the earlier chat-specific branch),
// so the input is synthetic — the branch stays for any future
// multi-stage perpetual workflow.
func TestPerpetualDoneRendersRepeatableStageNotCloseNag(t *testing.T) {
	md := &run.Metadata{ID: "big-goal", Project: "moe", Workflow: "planner", Status: run.StatusInProgress}
	r := rowsByKey(t, []*run.Metadata{md},
		map[string]NextDecision{"big-goal": {Stage: "reconcile", Done: true, Perpetual: true}},
		nil)["moe/big-goal"]
	if r.Bucket != BucketActiveRuns {
		t.Fatalf("bucket=%v want ACTIVE", r.Bucket)
	}
	if r.Note != "planner:reconcile" {
		t.Fatalf("note=%q want %q", r.Note, "planner:reconcile")
	}
	if strings.Contains(r.Note, "done") || strings.Contains(r.Note, "close?") {
		t.Fatalf("perpetual done note must not nag to close: %q", r.Note)
	}
	if r.Stage != "reconcile" {
		t.Fatalf("stage=%q want reconcile", r.Stage)
	}
}

// TestCrossProjectSameSlugRowsDontBleed: two in-progress runs sharing a
// slug in different projects must each render their own next-stage note,
// live marker, and age — the qualified "<project>/<slug>" keying is what
// keeps one row from painting the other's state. A bare-slug key would
// collapse the NextByRun / SessionDocsByRun / LastActivity lookups.
func TestCrossProjectSameSlugRowsDontBleed(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{
		{ID: "shared", Project: "alpha", Workflow: "sdlc", Status: run.StatusInProgress},
		{ID: "shared", Project: "beta", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	idx := &run.JournalIndex{LastActivity: map[string]time.Time{
		"alpha/shared": base,
		"beta/shared":  base.Add(-72 * time.Hour),
	}}
	rows, err := BuildRows(Inputs{
		Now:   base,
		Runs:  runs,
		Index: idx,
		NextByRun: map[string]NextDecision{
			"alpha/shared": {Stage: "code"},
			"beta/shared":  {Stage: "design"},
		},
		SessionDocsByRun: map[string][]string{
			"alpha/shared": {"code"}, // alpha has a live session; beta does not.
		},
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	byKey := make(map[string]Row, len(rows))
	for _, r := range rows {
		byKey[r.Project+"/"+r.Run] = r
	}
	if got := byKey["alpha/shared"].Note; got != "sdlc:code [running]" {
		t.Errorf("alpha/shared note = %q, want %q", got, "sdlc:code [running]")
	}
	if got := byKey["beta/shared"].Note; got != "sdlc:design" {
		t.Errorf("beta/shared note = %q, want %q (must not borrow alpha's stage or marker)", got, "sdlc:design")
	}
	if !byKey["alpha/shared"].When.Equal(base) || !byKey["beta/shared"].When.Equal(base.Add(-72*time.Hour)) {
		t.Errorf("ages crossed: alpha=%v beta=%v", byKey["alpha/shared"].When, byKey["beta/shared"].When)
	}
}

// TestChatClosedRendersResumeInCompleted: a closed chat stays in
// COMPLETED (close is a soft archive) but advertises `· resume?` —
// re-entry is cheap. No hasBeenReopened gate; chat has no reopen chain.
func TestChatClosedRendersResumeInCompleted(t *testing.T) {
	md := &run.Metadata{ID: "ponder", Project: "moe", Workflow: ChatWorkflow, Status: run.StatusClosed}
	r := rowsByKey(t, []*run.Metadata{md}, nil, nil)["moe/ponder"]
	if r.Bucket != BucketCompletedRuns {
		t.Fatalf("bucket=%v want COMPLETED", r.Bucket)
	}
	if r.Note != "chat:closed · resume?" {
		t.Fatalf("note=%q want %q", r.Note, "chat:closed · resume?")
	}
}

// chainRows builds rows for a chain head plus its children, with the
// head's empty ladder modelled as `Done: true` (what NextWithIndex
// returns for a stageless workflow) and children on the normal sdlc
// ladder. Keyed by "<project>/<slug>".
func chainRows(t *testing.T, runs []*run.Metadata, chained map[string]string) map[string]Row {
	t.Helper()
	next := make(map[string]NextDecision)
	for _, md := range runs {
		if md.Status != run.StatusInProgress {
			continue
		}
		if md.Workflow == ChainWorkflow {
			next[md.Project+"/"+md.ID] = NextDecision{Done: true}
		} else {
			next[md.Project+"/"+md.ID] = NextDecision{Stage: "design"}
		}
	}
	rows, err := BuildRows(Inputs{
		Now:       time.Now().UTC(),
		Runs:      runs,
		Index:     &run.JournalIndex{ChainedChild: chained},
		NextByRun: next,
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	m := make(map[string]Row, len(rows))
	for _, r := range rows {
		m[r.Project+"/"+r.Run] = r
	}
	return m
}

func chainHead(project, id string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: ChainWorkflow, Status: run.StatusInProgress}
}

// TestChainHeadWithLiveChildHintsKick is the core of the change: a
// loaded head is trivially "done" (no stages), but the operator verb is
// kick — close would drop the batch's collection point without riding
// it.
func TestChainHeadWithLiveChildHintsKick(t *testing.T) {
	head := chainHead("moe", "dev-observability")
	child := &run.Metadata{ID: "fix-a", Project: "moe", Workflow: "sdlc", Status: run.StatusInProgress}
	r := chainRows(t, []*run.Metadata{head, child},
		map[string]string{"moe/dev-observability": "moe/fix-a"})["moe/dev-observability"]
	if r.Bucket != BucketActiveRuns {
		t.Fatalf("bucket=%v want ACTIVE", r.Bucket)
	}
	if r.Note != "chain:parked · kick?" {
		t.Fatalf("note=%q want %q", r.Note, "chain:parked · kick?")
	}
	if strings.Contains(r.Note, "close?") {
		t.Fatalf("loaded head must not nag close: %q", r.Note)
	}
	if r.Stage != "done" {
		t.Fatalf("stage=%q want %q (no advance button, generic glyph)", r.Stage, "done")
	}
}

// TestChainHeadWithoutLiveChildHintsClose: a spent head (all children
// terminal) or an empty one (no edge yet) genuinely is done, so the
// existing close nag stays.
func TestChainHeadWithoutLiveChildHintsClose(t *testing.T) {
	head := chainHead("moe", "dev-observability")
	for name, tc := range map[string]struct {
		runs    []*run.Metadata
		chained map[string]string
	}{
		"spent": {
			runs: []*run.Metadata{head, {
				ID: "fix-a", Project: "moe", Workflow: "sdlc", Status: run.StatusClosed}},
			chained: map[string]string{"moe/dev-observability": "moe/fix-a"},
		},
		"empty": {runs: []*run.Metadata{head}},
	} {
		t.Run(name, func(t *testing.T) {
			r := chainRows(t, tc.runs, tc.chained)["moe/dev-observability"]
			if r.Bucket != BucketActiveRuns {
				t.Fatalf("bucket=%v want ACTIVE", r.Bucket)
			}
			if r.Note != "chain:done · close?" {
				t.Fatalf("note=%q want %q", r.Note, "chain:done · close?")
			}
		})
	}
}

// TestChainHeadKickHintSurvivesGrouping: groupActiveChains draws the
// live child adjacently beneath the head and suppresses the textual
// `· chained →` hint — the kick hint has to survive that pass.
func TestChainHeadKickHintSurvivesGrouping(t *testing.T) {
	head := chainHead("moe", "dev-observability")
	child := &run.Metadata{ID: "fix-a", Project: "moe", Workflow: "sdlc", Status: run.StatusInProgress}
	byKey := chainRows(t, []*run.Metadata{head, child},
		map[string]string{"moe/dev-observability": "moe/fix-a"})
	if got := byKey["moe/dev-observability"].Chained; got {
		t.Errorf("head Chained = %v, want false (heads stay unmarked)", got)
	}
	if got := byKey["moe/fix-a"].Chained; !got {
		t.Errorf("live child Chained = %v, want true (follows the head)", got)
	}
	if got := byKey["moe/dev-observability"].Note; got != "chain:parked · kick?" {
		t.Errorf("head note = %q, want %q (no text chain hint when adjacent)", got, "chain:parked · kick?")
	}
}

// TestRenderChainedActiveRowIsFlushWithArrow: a chain member renders
// flush-left with "→" — the same column as its head, one glyph apart
// from the indented "↳" a spawn descendant would carry. In monospace
// the connector is the only thing separating the two relationships.
func TestRenderChainedActiveRowIsFlushWithArrow(t *testing.T) {
	rows := []Row{
		{Project: "p", Run: "head", Note: "sdlc:design", Bucket: BucketActiveRuns},
		{Project: "p", Run: "next", Note: "sdlc:design", Bucket: BucketActiveRuns, Chained: true},
	}
	var buf bytes.Buffer
	Render(&buf, time.Now().UTC(), nil, rows, 1, 1, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := buf.String()
	// Rows carry a two-space section indent; the chained row adds only
	// the glyph, so both slugs start at the same column as "  p/head".
	if !strings.Contains(out, "\n  → p/next") {
		t.Fatalf("a chained row should render flush with the → connector:\n%s", out)
	}
	if strings.Contains(out, "↳ p/next") {
		t.Fatalf("a chained row must not borrow the lineage connector:\n%s", out)
	}
}

// TestActiveChainHeadWithSettledParent is the incident this change
// exists for: a two-item chain whose first item merged. The tail is the
// next thing to run, but grouping needs both endpoints active, so it
// collapsed to a bare orphan row. It must render with the arrow and a
// note naming what it follows.
func TestActiveChainHeadWithSettledParent(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	shipped := &run.Metadata{ID: "shipped", Project: "p", Workflow: "sdlc", Status: run.StatusMerged}
	runs := []*run.Metadata{shipped, activeRun("p", "tail"), activeRun("p", "x")}
	when := map[string]time.Time{"shipped": base, "tail": base.Add(-time.Hour), "x": base.Add(-2 * time.Hour)}
	active := buildActive(t, runs, when, map[string]string{"p/shipped": "p/tail"})
	assertOrder(t, active, "p/tail", "p/x")

	byKey := map[string]Row{}
	for _, r := range active {
		byKey[r.Project+"/"+r.Run] = r
	}
	tail := byKey["p/tail"]
	if !tail.Chained {
		t.Errorf("p/tail Chained = false; a queued tail must draw the connector")
	}
	if !strings.Contains(tail.Note, "chained after p/shipped (merged)") {
		t.Errorf("p/tail Note = %q, want the settled-parent hint", tail.Note)
	}
	if byKey["p/x"].Chained || strings.Contains(byKey["p/x"].Note, "chained after") {
		t.Errorf("unrelated orphan p/x picked up chain marking: %+v", byKey["p/x"])
	}
}

// TestActiveChainLiveParentStillSuppressesArrowOnHead guards the
// regression the settled-parent branch could introduce: a head whose
// only parent is active is a head, not a member, and must stay unmarked.
func TestActiveChainLiveParentStillSuppressesArrowOnHead(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	runs := []*run.Metadata{activeRun("p", "a"), activeRun("p", "b")}
	when := map[string]time.Time{"a": base, "b": base.Add(-time.Hour)}
	active := buildActive(t, runs, when, map[string]string{"p/a": "p/b"})
	if active[0].Chained {
		t.Errorf("head p/a Chained = true, want false")
	}
	if strings.Contains(active[0].Note, "chained after") {
		t.Errorf("head p/a Note = %q, want no settled-parent hint", active[0].Note)
	}
}
