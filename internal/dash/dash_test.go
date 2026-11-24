package dash

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

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

// TestMergedSDLCRendersReopenHint: a merged sdlc run whose reopen chain
// is unextended advertises `· reopen?`. The merged row is where the
// operator looks for "how do I keep going", and the gesture they used to
// reach for (re-typing the stage verb) is now a refusal — so this is the
// row that has to name the real door.
func TestMergedSDLCRendersReopenHint(t *testing.T) {
	md := &run.Metadata{ID: "shipped", Project: "moe", Workflow: "sdlc", Status: run.StatusMerged}
	r := rowsByKey(t, []*run.Metadata{md}, nil, nil)["moe/shipped"]
	if r.Bucket != BucketCompletedRuns {
		t.Fatalf("bucket=%v want COMPLETED", r.Bucket)
	}
	if r.Note != "sdlc:merged · reopen?" {
		t.Fatalf("note=%q want %q", r.Note, "sdlc:merged · reopen?")
	}
}

// TestMergedSDLCSuppressesReopenHintOnceReopened: same gate the closed
// rows use — a run that already has a reopen descendant isn't the one to
// reopen again, so the hint drops.
func TestMergedSDLCSuppressesReopenHintOnceReopened(t *testing.T) {
	md := &run.Metadata{ID: "shipped", Project: "moe", Workflow: "sdlc", Status: run.StatusMerged}
	rows, err := BuildRows(Inputs{
		Now:   time.Now().UTC(),
		Runs:  []*run.Metadata{md},
		Index: &run.JournalIndex{ReopenedFrom: map[string]string{"moe/shipped-2026-07-25": "shipped"}},
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	if rows[0].Note != "sdlc:merged" {
		t.Fatalf("note=%q want %q", rows[0].Note, "sdlc:merged")
	}
}

// TestMergedNonSDLCHasNoReopenHint: reopen is an sdlc verb, so marking a
// merged run of another workflow would advertise an action the operator
// can't take.
func TestMergedNonSDLCHasNoReopenHint(t *testing.T) {
	md := &run.Metadata{ID: "groom", Project: "moe", Workflow: "chat", Status: run.StatusMerged}
	r := rowsByKey(t, []*run.Metadata{md}, nil, nil)["moe/groom"]
	if r.Note != "chat:merged" {
		t.Fatalf("note=%q want %q", r.Note, "chat:merged")
	}
}

func chainHead(project, id string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: ChainWorkflow, Status: run.StatusInProgress}
}

// TestServeCluster pins the one line the CLI banner and the web header
// both carry. The compressions matter: "next 15m" not "next sweep in
// 15m", "browse-only" without the parenthetical restating it, and no
// failing token at all on a healthy board.
func TestServeCluster(t *testing.T) {
	for _, tc := range []struct {
		name    string
		armed   bool
		up      string
		host    string
		next    string
		modes   ModeCounts
		failing int
		want    string
	}{
		{name: "armed", armed: true, up: "4m", next: "15m", want: "serve armed · up 4m · next 15m"},
		{name: "armed, no tick scheduled", armed: true, up: "4m", want: "serve armed · up 4m"},
		{name: "unarmed", up: "2h 5m", want: "serve browse-only · up 2h 5m"},
		{
			name: "one failing", armed: true, up: "4m", next: "15m", failing: 1,
			want: "serve armed · up 4m · next 15m · 1 failing",
		},
		{
			name: "unarmed with failures", up: "4m", failing: 3,
			want: "serve browse-only · up 4m · 3 failing",
		},
		// The host rides after uptime, armed only: an unarmed serve runs
		// no jobs and picks no host.
		{name: "rex", armed: true, up: "4m", host: "rex", next: "15m", want: "serve armed · up 4m · rex · next 15m"},
		{name: "local", armed: true, up: "4m", host: "local pty", next: "15m", want: "serve armed · up 4m · local pty · next 15m"},
		{
			name: "rex unusable", armed: true, up: "4m", host: "rex unusable", next: "15m", failing: 1,
			want: "serve armed · up 4m · rex unusable · next 15m · 1 failing",
		},
		// Modes add slots rather than taking one: unlike a hold on the whole
		// clock, they say how much of the board is capped while the rest of
		// the line goes on describing a heartbeat that is still ticking.
		{
			name: "one paused", armed: true, up: "4m", next: "15m",
			modes: ModeCounts{Paused: 1},
			want:  "serve armed · up 4m · next 15m · 1 paused",
		},
		{
			name: "paused and safe", armed: true, up: "4m", next: "15m",
			modes: ModeCounts{Paused: 1, Safe: 2},
			want:  "serve armed · up 4m · next 15m · 1 paused",
		},
		{
			name: "modes with failures", armed: true, up: "4m", next: "15m",
			modes: ModeCounts{Safe: 1}, failing: 2,
			want: "serve armed · up 4m · next 15m · 2 failing",
		},
		// A browse-only serve has no clock, but the modes are still the
		// operator's standing answer for when one arms — and unlike a
		// hold on the whole clock they promise nothing about sweeps that
		// were never going to happen.
		{
			name: "unarmed with a paused project", up: "4m",
			modes: ModeCounts{Paused: 1},
			want:  "serve browse-only · up 4m · 1 paused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServeCluster(tc.armed, tc.up, tc.host, tc.next, tc.modes, tc.failing); got != tc.want {
				t.Errorf("ServeCluster = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPushItem pins the origin item both dashes append to the cluster.
//
// The precedence is the whole design: the live ahead-count outranks
// anything the record says, because a serve that pushed at 10:00 and has
// been refused since 10:05 has a true "pushed 5m ago" and three commits
// nobody can see. And the failure only shows while commits are actually
// waiting — once the queue is empty the outage is history.
func TestPushItem(t *testing.T) {
	now := time.Now()
	failing := now.Add(-12 * time.Minute)

	for _, tc := range []struct {
		name         string
		serve        bool
		ahead        int
		failingSince time.Time
		want         string
	}{
		{
			name: "waiting and refused", serve: true, ahead: 3,
			failingSince: failing,
			want:         "push failing 12m · 3 unpushed",
		},
		{name: "waiting, drain healthy", serve: true, ahead: 3, want: "3 unpushed"},
		{name: "recovered queue, historical failure", serve: true, failingSince: failing, want: ""},
		// A live serve that has pushed nothing yet says nothing. An
		// ahead-count of zero reads the same on a caught-up box and on one
		// with no origin at all, so a standing "in sync" would be
		// permanently false on a local-only bureaucracy.
		{name: "nothing to push, none landed yet", serve: true, want: ""},
		// No serve vouching, nothing waiting: the banner is what it always
		// was. The floor this whole change is measured against.
		{name: "no serve, nothing waiting", want: ""},
		// A dead or absent serve still can't hide commits. Its recorded
		// fields are ignored — "pushed 2m ago" from a process that has
		// since crashed is the one reading that sends the operator away
		// happy while the journal sits on the box.
		{
			name: "no serve, commits waiting", ahead: 3,
			failingSince: failing,
			want:         "3 unpushed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PushItem(now, tc.serve, tc.ahead, tc.failingSince)
			if got != tc.want {
				t.Errorf("PushItem = %q, want %q", got, tc.want)
			}
		})
	}
}

// The two input hints read as action hints beside `· close?`: `· ask?`
// for a question needing the operator, `· input` for prose already given
// and not yet picked up. A run can wear both.
func TestBuildRowsMarksInputHints(t *testing.T) {
	md := &run.Metadata{ID: "fix-it", Project: "alpha", Status: run.StatusInProgress, Workflow: "sdlc"}
	base := func(in Inputs) Inputs {
		in.Runs = []*run.Metadata{md}
		in.Index = &run.JournalIndex{}
		in.NextByRun = map[string]NextDecision{"alpha/fix-it": {Stage: "code"}}
		return in
	}

	rows, err := BuildRows(base(Inputs{AwaitingAnswer: map[string]bool{"alpha/fix-it": true}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Note, "· ask?") {
		t.Fatalf("note = %+v, want the ask hint", rows)
	}

	rows, err = BuildRows(base(Inputs{PendingInput: map[string]bool{"alpha/fix-it": true}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows[0].Note, "· input") {
		t.Fatalf("note = %q, want the pending hint", rows[0].Note)
	}
	if strings.Contains(rows[0].Note, "ask?") {
		t.Fatalf("note = %q, want no ask hint with nothing open", rows[0].Note)
	}

	rows, err = BuildRows(base(Inputs{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rows[0].Note, "input") || strings.Contains(rows[0].Note, "ask?") {
		t.Fatalf("note = %q, want no hints on an untouched run", rows[0].Note)
	}
}

// A run whose last machine turn was unfinished carries `· unfinished`
// on its ACTIVE row. No question mark: the mark is state rather than a question — but
// a row that dropped a turn and a row the loop never reached are
// otherwise identical, which is the archaeology the tombstone exists to
// spare.
func TestBuildRowsMarksAReapedRun(t *testing.T) {
	md := &run.Metadata{ID: "fix-it", Project: "alpha", Status: run.StatusInProgress, Workflow: "sdlc"}
	in := Inputs{
		Runs:      []*run.Metadata{md},
		Index:     &run.JournalIndex{},
		NextByRun: map[string]NextDecision{"alpha/fix-it": {Stage: "design"}},
	}

	rows, err := BuildRows(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || strings.Contains(rows[0].Note, "· unfinished") || strings.Contains(rows[0].Note, "· died") {
		t.Fatalf("note = %+v, want no unfinished marker on an untouched run", rows)
	}

	md.Reaped = &run.ReapNote{Doc: "design", At: "2026-08-23T23:14:14Z", Tip: "81c2d5a71"}
	rows, err = BuildRows(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows[0].Note, "· unfinished") || strings.Contains(rows[0].Note, "· died") {
		t.Fatalf("note = %q, want only the unfinished marker", rows[0].Note)
	}
}

// RenderIndexed places every shown row on the line that carries it, and
// says -1 for rows the board does not show.
func TestRenderIndexedPlacesRows(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	rows := []Row{
		{Project: "p", Run: "act", Bucket: BucketActiveRuns, When: now, Note: "code"},
		{Project: "p", Run: "goal", Bucket: BucketIntents, Note: "Aim"},
		{Project: "p", Run: "chore", Bucket: BucketChores, When: now},
		{Project: "p", Run: "idea", Bucket: BucketBacklog, When: now, Note: "An idea"},
		{Project: "p", Run: "none", Bucket: BucketNone},
	}
	for i := range CompletedCap + 2 {
		rows = append(rows, Row{Project: "p", Run: fmt.Sprintf("done-%d", i), Bucket: BucketCompletedRuns, When: now})
	}
	var buf bytes.Buffer
	lines := RenderIndexed(&buf, now, nil, rows, 1, 1, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := strings.Split(buf.String(), "\n")
	for i, r := range rows {
		switch {
		case r.Bucket == BucketNone || r.Bucket == BucketCompletedRuns && i >= len(rows)-2:
			if lines[i] != -1 {
				t.Errorf("%s placed at %d, want unshown", r.Run, lines[i])
			}
		case lines[i] < 0 || !strings.Contains(out[lines[i]], "p/"+r.Run):
			t.Errorf("%s placed at %d: %q", r.Run, lines[i], out[max(lines[i], 0)])
		}
	}
	var plain bytes.Buffer
	Render(&plain, now, nil, rows, 1, 1, false, FactoryState{}, rand.New(rand.NewSource(1)))
	if plain.String() != buf.String() {
		t.Fatal("RenderIndexed output differs from Render")
	}
}
