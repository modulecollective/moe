package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/ui"
)

func uiItem(t *testing.T, b ui.Board, project, slug string, kind ui.ItemKind) ui.Item {
	t.Helper()
	for _, it := range b.Items {
		if it.Project == project && it.Run == slug && it.Kind == kind {
			return it
		}
	}
	t.Fatalf("no %v item %s/%s in %+v", kind, project, slug, b.Items)
	return ui.Item{}
}

func takeBoard(t *testing.T, s *uiSource) ui.Board {
	t.Helper()
	select {
	case b := <-s.boards:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no board")
	}
	return ui.Board{}
}

// The board is the dash frame with every completed row, and each item
// sits on the line that names it, carrying what the console may do.
func TestUIBoardIndexesDashRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	if _, err := createIdea(root, "moe", "an-idea", "# An idea\n\nBody \x1b[2J text.\n", "", trailers.Block{}); err != nil {
		t.Fatal(err)
	}
	if err := runopen.TagIdea(root, "moe", "an-idea", "sdlc", true); err != nil {
		t.Fatal(err)
	}
	askOn(t, root, "moe", "change-auth", "Which store?")
	s := newUISource(root, DashFilter{}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	for _, it := range b.Items {
		if !strings.Contains(b.Lines[it.Line], it.Project+"/"+it.Run) {
			t.Fatalf("%s on line %d: %q", it.Label(), it.Line, b.Lines[it.Line])
		}
	}
	if !slices.IsSortedFunc(b.Items, func(x, y ui.Item) int { return x.Line - y.Line }) {
		t.Fatal("items out of line order")
	}
	r := uiItem(t, b, "moe", "change-auth", ui.ItemRun)
	if !r.Notes || !r.Controls || r.Question == nil || r.Question.Text != "Which store?" || r.Terminal || r.Generation != "" {
		t.Fatalf("run item = %+v", r)
	}
	if d := strings.Join(r.Detail, "\n"); !strings.Contains(d, "owner: none") || !strings.Contains(d, "hold: none") || !strings.Contains(d, "open question #") {
		t.Fatalf("run detail:\n%s", d)
	}
	i := uiItem(t, b, "moe", "an-idea", ui.ItemIdea)
	if !i.Editable || !i.Notes || i.Controls || !strings.Contains(i.Body, "Body \x1b[2J text.") {
		t.Fatalf("idea item = %+v", i)
	}
	if d := strings.Join(i.Detail, "\n"); !strings.Contains(d, "tagged sdlc (design only)") {
		t.Fatalf("idea detail:\n%s", d)
	}
}

// A published owner update reaches the console as a pushed board, with
// no gather in between; a gather does not poll the watched endpoint.
func TestUIWatchPushesWithoutGather(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, md := controlFixture(t)
	ownForTest(t, root)
	s := newUISource(root, DashFilter{}, os.Stdout)
	var gathers atomic.Int32
	inner := s.gather
	s.gather = func(now time.Time, cached liveLookup) (DashSnapshot, error) {
		gathers.Add(1)
		return inner(now, cached)
	}
	prev := uiDiscovery
	uiDiscovery = time.Hour // nothing but the watch may repaint
	t.Cleanup(func() { uiDiscovery = prev })
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	gen := ownerGeneration(root, "moe", "change-auth")
	waitBoard := func(want func(ui.Item) bool) ui.Item {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case b := <-s.boards:
				if it := uiItem(t, b, "moe", "change-auth", ui.ItemRun); want(it) {
					return it
				}
			case <-deadline:
				t.Fatal("no matching board")
			}
		}
	}
	it := waitBoard(func(it ui.Item) bool { return it.Generation == gen })
	if !strings.Contains(strings.Join(it.Detail, "\n"), "live: watching") {
		t.Fatalf("detail = %v", it.Detail)
	}
	publishPhase(root, md, runsock.PhaseRunning, "code", "")
	it = waitBoard(func(it ui.Item) bool {
		return strings.Contains(strings.Join(it.Detail, "\n"), "phase: agent running (stage code)")
	})
	if n := gathers.Load(); n != 1 {
		t.Fatalf("%d gathers; the watch should have repainted alone", n)
	}

	// A gather while the watch is healthy reads the cached snapshot
	// instead of polling.
	var probed atomic.Int32
	snap, err := gatherDashSnapshot(root, time.Now(), DashFilter{}, func(key string) (runsock.Status, bool) {
		probed.Add(1)
		return s.cachedLive(key)
	})
	if err != nil || probed.Load() == 0 || snap.Observations["moe/change-auth"].Live == nil {
		t.Fatalf("cached gather: probed=%d err=%v", probed.Load(), err)
	}
}

// Notes and answers go through the input writers; a stale answer is a
// definite refusal, never an unknown outcome.
func TestUIDoInput(t *testing.T) {
	root, _ := controlFixture(t)
	askOn(t, root, "moe", "change-auth", "Which store?")
	f, _ := input.Load(root, "moe", "change-auth")
	ping, _ := f.OpenPing()

	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActNote, Project: "moe", Run: "change-auth", Text: "use sqlite"}); !r.OK {
		t.Fatalf("note = %+v", r)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActAnswer, Project: "moe", Run: "change-auth", QuestionID: ping.ID + 7, Text: "x"}); r.OK || r.Uncertain {
		t.Fatalf("stale answer = %+v", r)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActAnswer, Project: "moe", Run: "change-auth", QuestionID: ping.ID, Text: "postgres"}); !r.OK {
		t.Fatalf("answer = %+v", r)
	}
	f, _ = input.Load(root, "moe", "change-auth")
	if len(f.Pending()) != 2 {
		t.Fatalf("pending = %+v", f.Pending())
	}
}

// Controls use the CLI's writers: a pause holds, resume releases only
// the hold it was shown, and nothing launches.
func TestUIDoControls(t *testing.T) {
	root, _ := controlFixture(t)
	r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"})
	if !r.OK || r.RequestID != "p1" || controlRecord(t, root).Hold == nil {
		t.Fatalf("pause = %+v", r)
	}
	// The same id reads back rather than asking twice.
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"}); !r.OK {
		t.Fatalf("retry = %+v", r)
	}
	if n := len(controlRecord(t, root).Requests); n != 1 {
		t.Fatalf("retry recorded %d requests", n)
	}
	shown := controlRecord(t, root).Revision
	stale := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume", HoldRevision: shown + 1})
	if stale.OK || stale.Uncertain || controlRecord(t, root).Hold == nil {
		t.Fatalf("stale resume = %+v", stale)
	}
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume", HoldRevision: shown}); !r.OK || controlRecord(t, root).Hold != nil {
		t.Fatalf("resume = %+v", r)
	}
	// A park needs the owner it addresses; with none it is refused.
	if r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "park"}); r.OK || r.Uncertain {
		t.Fatalf("ownerless park = %+v", r)
	}
}

// Captures: created untagged, edited only over what was shown, tagged
// and untagged through the shared writer, closed with history kept.
func TestUIDoCaptures(t *testing.T) {
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "new-thing", Workflow: "idea", Text: "# new-thing\n\nfirst\n"}); !r.OK {
		t.Fatalf("create = %+v", r)
	}
	md, err := run.Load(root, "moe", "new-thing")
	if err != nil || md.PromoteTo != "" || md.Workflow != dash.IdeaWorkflow {
		t.Fatalf("created idea = %+v, %v", md, err)
	}
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "new-thing", Workflow: "idea", Text: "# again\n"}); r.OK || r.Uncertain {
		t.Fatalf("duplicate create = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nfirst\n", Text: "# new-thing\n\nsecond\n"}); !r.OK {
		t.Fatalf("edit = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nfirst\n", Text: "# lost\n"}); !r.Changed || r.OK {
		t.Fatalf("stale edit = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "new-thing", Expected: "# new-thing\n\nsecond\n", Text: "# new-thing\n\nsecond\n"}); !r.OK || !strings.Contains(r.Message, "unchanged") {
		t.Fatalf("same edit = %+v", r)
	}
	for _, tc := range []struct {
		wf     string
		design bool
	}{{"sdlc", true}, {"sdlc", false}, {"", false}} {
		if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: tc.wf, DesignOnly: tc.design}); !r.OK {
			t.Fatalf("tag %+v = %+v", tc, r)
		}
		md, _ := run.Load(root, "moe", "new-thing")
		if md.PromoteTo != tc.wf || md.DesignOnly != tc.design || md.Status != run.StatusInProgress {
			t.Fatalf("after tag %+v: %+v", tc, md)
		}
	}
	if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: "nope"}); r.OK {
		t.Fatalf("bad tag = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "a-goal", Workflow: "intent", Text: "# a-goal\n\nMy words.\n"}); !r.OK {
		t.Fatalf("intent = %+v", r)
	}
	if b, _ := os.ReadFile(root + "/" + run.ContentPath("moe", "a-goal", dash.IntentDocID)); string(b) != "# a-goal\n\nMy words.\n" {
		t.Fatalf("intent text = %q", b)
	}
	if r := do(ui.Action{Kind: ui.ActClose, Project: "moe", Run: "new-thing"}); !r.OK {
		t.Fatalf("close = %+v", r)
	}
	if md, _ := run.Load(root, "moe", "new-thing"); md.Status != run.StatusClosed {
		t.Fatalf("closed = %s", md.Status)
	}
	if r := do(ui.Action{Kind: ui.ActTag, Project: "moe", Run: "new-thing", Workflow: "sdlc"}); r.OK || r.Uncertain {
		t.Fatalf("tag closed = %+v", r)
	}
}

// fakeRex answers rex api calls from a table and records them.
type fakeRex struct {
	calls   []string
	answers map[string]string
	errs    map[string]error
}

func (f *fakeRex) install(t *testing.T) {
	t.Helper()
	prev := rexCall
	rexCall = func(_ context.Context, method string, args any) ([]byte, error) {
		b, _ := json.Marshal(args)
		f.calls = append(f.calls, method+" "+string(b))
		if err := f.errs[method]; err != nil {
			return nil, err
		}
		return []byte(f.answers[method]), nil
	}
	t.Cleanup(func() { rexCall = prev })
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/rex", []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const (
	rexView    = `{"windows":[{"window_id":"window:w1","layers":[{"blocks":[{"block_id":"block:other"},{"block_id":"block:b1"}]}]}]}`
	rexInspect = `{"client":{},"actions":[{"name":"pane.split"},{"name":"session.select"}]}`
)

func rexClients(ids ...string) string {
	var cs []string
	for _, id := range ids {
		cs = append(cs, `{"client_id":"`+id+`","info":{"kind":"app","name":"Rex","label":"`+id+`"},"connection_state":"connected"}`)
	}
	cs = append(cs, `{"client_id":"client:cli","info":{"kind":"cli"},"connection_state":"connected"}`)
	return `{"clients":[` + strings.Join(cs, ",") + `]}`
}

func TestUIRevealTerminal(t *testing.T) {
	root, _ := controlFixture(t)
	t.Setenv("REX_SESSION", "session:s1")
	t.Setenv("REX_BLOCK", "block:b1")
	t.Setenv("REX_CLIENT", "")
	ownForTest(t, root)
	gen := ownerGeneration(root, "moe", "change-auth")
	a := ui.Action{Kind: ui.ActReveal, Project: "moe", Run: "change-auth", Generation: gen}

	f := &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:mac"), "client.inspect": rexInspect, "session.focus_block": `{"revision":2}`, "client.perform": `{"performed_by":"client:mac"}`}}
	f.install(t)
	r := revealTerminal(context.Background(), root, a)
	if !r.OK {
		t.Fatalf("reveal = %+v", r)
	}
	want := []string{
		`session.view {"session_id":"session:s1"}`,
		`client.list {}`,
		`client.inspect {"client_id":"client:mac"}`,
		`session.focus_block {"block_id":"block:b1","session_id":"session:s1"}`,
		`client.perform {"action":"session.select","args":{"session_id":"session:s1","window_id":"window:w1"},"client_id":"client:mac","origin":"api","timeout_ms":5000}`,
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}

	// Several apps: ask, don't guess; the choice is then honoured.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:a", "client:b"), "client.inspect": rexInspect}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); len(r.Choices) != 2 || r.OK {
		t.Fatalf("ambiguous = %+v", r)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "session.focus_block") }) {
		t.Fatal("focused before a client was chosen")
	}
	a.Client = "client:b"
	f.answers["client.perform"] = `{}`
	if r := revealTerminal(context.Background(), root, a); !r.OK || !strings.Contains(f.calls[len(f.calls)-1], `"client_id":"client:b"`) {
		t.Fatalf("chosen = %+v %v", r, f.calls)
	}
	a.Client = ""

	// A detached block has no window: reported, nothing focused.
	f = &fakeRex{answers: map[string]string{"session.view": `{"windows":[]}`}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); r.OK || !strings.Contains(r.Message, "detached") || len(f.calls) != 1 {
		t.Fatalf("detached = %+v %v", r, f.calls)
	}

	// The app timing out is an unknown outcome, said as such.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients("client:mac"), "client.inspect": rexInspect}, errs: map[string]error{"client.perform": errors.New("rex client.perform: timeout")}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); !r.Uncertain || !strings.Contains(r.Message, "may still") {
		t.Fatalf("timeout = %+v", r)
	}

	// No app: unavailable. An owner other than the one shown: refused
	// before any rex call.
	f = &fakeRex{answers: map[string]string{"session.view": rexView, "client.list": rexClients()}}
	f.install(t)
	if r := revealTerminal(context.Background(), root, a); r.OK || !strings.Contains(r.Message, "no connected Rex app") {
		t.Fatalf("no app = %+v", r)
	}
	f = &fakeRex{}
	f.install(t)
	a.Generation = "someone-else"
	if r := revealTerminal(context.Background(), root, a); r.OK || len(f.calls) != 0 || !strings.Contains(r.Message, "owner changed") {
		t.Fatalf("replaced owner = %+v %v", r, f.calls)
	}
}

// A resume whose acknowledgement was lost, resent whole after a new
// hold replaced the one it released, reads back what it did and leaves
// the new hold alone. Resent with the new revision, it would conflict.
func TestUIDoResumeRetryAfterNewHold(t *testing.T) {
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	if r := do(ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p1"}); !r.OK {
		t.Fatalf("pause = %+v", r)
	}
	original := ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "resume",
		RequestID: "r1", HoldRevision: controlRecord(t, root).Revision}
	if r := do(original); !r.OK { // acknowledgement "lost"
		t.Fatalf("resume = %+v", r)
	}
	if r := do(ui.Action{Kind: ui.ActControl, Project: "moe", Run: "change-auth", Control: "pause", RequestID: "p2"}); !r.OK {
		t.Fatalf("second pause = %+v", r)
	}
	if r := do(original); !r.OK || !strings.Contains(r.Message, "already recorded") || controlRecord(t, root).Hold == nil {
		t.Fatalf("retry = %+v, hold %+v", r, controlRecord(t, root).Hold)
	}
	changed := original
	changed.HoldRevision = controlRecord(t, root).Revision
	if r := do(changed); r.OK {
		t.Fatalf("retry rebuilt against the new hold succeeded: %+v", r)
	}
}

// Closed intents remain inspectable from the canonical scan, without a
// draft or a previously running source. Ordinary dash membership stays intact.
func TestUIClosedIntentsInspectable(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root, _ := controlFixture(t)
	do := func(a ui.Action) ui.Result { return uiDo(context.Background(), root, a) }
	body := "# A goal\n" + strings.Repeat("standing direction\n", 30) + strings.Repeat("x", 164) + "END_OF_INTENT\n"
	for _, a := range []ui.Action{
		{Kind: ui.ActCreate, Project: "moe", Run: "b-goal", Workflow: "intent", Text: "# Another goal\n"},
		{Kind: ui.ActCreate, Project: "moe", Run: "a-goal", Workflow: "intent", Text: body},
		{Kind: ui.ActCreate, Project: "moe", Run: "an-idea", Workflow: "idea", Text: "# an-idea\n"},
	} {
		if r := do(a); !r.OK {
			t.Fatalf("%+v: %+v", a, r)
		}
	}
	s := newUISource(root, DashFilter{}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	if it := uiItem(t, b, "moe", "a-goal", ui.ItemIntent); !it.Editable {
		t.Fatalf("open intent = %+v", it)
	}
	for _, slug := range []string{"a-goal", "b-goal", "an-idea"} {
		if r := do(ui.Action{Kind: ui.ActClose, Project: "moe", Run: slug}); !r.OK {
			t.Fatalf("close %s: %+v", slug, r)
		}
	}
	for _, source := range []*uiSource{s, newUISource(root, DashFilter{}, os.Stdout)} {
		if err := source.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		b = takeBoard(t, source)
		it := uiItem(t, b, "moe", "a-goal", ui.ItemIntent)
		if it.Body != body || it.Editable || it.Notes || it.Controls || it.Terminal || it.Question != nil || !strings.Contains(strings.Join(it.Detail, "\n"), "closed") {
			t.Fatalf("closed intent = %+v", it)
		}
		other := uiItem(t, b, "moe", "b-goal", ui.ItemIntent)
		if it.Line >= other.Line || !strings.Contains(b.Lines[it.Line], it.Label()) || !strings.Contains(strings.Join(b.Lines[:it.Line], "\n"), "CLOSED INTENTS") {
			t.Fatalf("closed intent lines = %v", b.Lines)
		}
		if idea := uiItem(t, b, "moe", "an-idea", ui.ItemIdea); idea.Editable {
			t.Fatalf("closed idea = %+v", idea)
		}
		for _, row := range source.snap.Rows {
			if row.Run == "a-goal" || row.Run == "b-goal" {
				t.Fatalf("UI history changed dash membership: %+v", row)
			}
		}
	}
	// The scan includes every project/workflow; the UI-only section must
	// apply the same filters as the ordinary dashboard rows.
	for _, filter := range []DashFilter{{ProjectFilter: "elsewhere"}, {WorkflowFilter: "idea"}, {WorkflowFilter: "intent"}} {
		source := newUISource(root, filter, os.Stdout)
		if err := source.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		b := takeBoard(t, source)
		var intents []string
		for _, it := range b.Items {
			if it.Kind == ui.ItemIntent {
				intents = append(intents, it.Run)
			}
		}
		want := []string(nil)
		if filter.WorkflowFilter == "intent" {
			want = []string{"a-goal", "b-goal"}
		}
		if !slices.Equal(intents, want) {
			t.Fatalf("filter %+v: intents %v, want %v", filter, intents, want)
		}
	}
	// Inspection grants no write authority, even if a stale editor sends one.
	if r := do(ui.Action{Kind: ui.ActEdit, Project: "moe", Run: "a-goal", Expected: body, Text: "replacement"}); r.OK || r.Uncertain {
		t.Fatalf("closed edit = %+v", r)
	}
	got, err := os.ReadFile(root + "/" + run.ContentPath("moe", "a-goal", "intent"))
	if err != nil || string(got) != body {
		t.Fatalf("closed canvas changed: %q, %v", got, err)
	}
}
