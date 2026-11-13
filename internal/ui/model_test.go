package ui

import (
	"fmt"
	"strings"
	"testing"
)

func testModel(t *testing.T) (*model, chan job) {
	t.Helper()
	q := make(chan job, 8)
	m := newModel(q)
	m.resize(30, 100)
	return m, q
}

func board(seq uint64, items ...Item) Board {
	b := Board{Seq: seq}
	for i := range items {
		items[i].Line = i
		b.Lines = append(b.Lines, "  "+items[i].Label())
	}
	b.Items = items
	return b
}

func press(m *model, ks ...Key) {
	for _, k := range ks {
		m.key(k)
	}
}

func typed(m *model, s string) { press(m, runes(s)...) }

func run(p, r string) Item {
	return Item{Project: p, Run: r, Kind: ItemRun, Notes: true, Controls: true, Generation: "g1", HoldRevision: 3}
}

func idea(p, r string, bucket int) Item {
	return Item{Project: p, Run: r, Bucket: bucket, Kind: ItemIdea, Editable: true, Notes: true, Body: "# " + r + "\n"}
}

// Selection follows the occurrence, then the run, then the nearest
// survivor — and says what happened.
func TestSelectionFollowsIdentity(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a"), idea("p", "x", 1), idea("p", "x", 3), run("p", "b")))
	press(m, Key{Code: KeyDown}, Key{Code: KeyDown})
	if m.sel != (itemKey{"p", "x", 3}) {
		t.Fatalf("sel = %+v", m.sel)
	}
	// Reordered: the same occurrence stays selected.
	m.setBoard(board(2, idea("p", "x", 3), run("p", "b"), idea("p", "x", 1), run("p", "a")))
	if it, _ := m.selected(); it.Line != 0 || it.Bucket != 3 {
		t.Fatalf("after reorder = %+v", it)
	}
	// The occurrence's bucket went: follow the same run.
	m.setBoard(board(3, run("p", "b"), idea("p", "x", 1), run("p", "a")))
	if m.sel != (itemKey{"p", "x", 1}) {
		t.Fatalf("after bucket change = %+v", m.sel)
	}
	// The run went: nearest survivor, said.
	m.setBoard(board(4, run("p", "b"), run("p", "a")))
	if m.sel != (itemKey{"p", "a", 0}) || !strings.Contains(consoleText(m), "p/x left the board") {
		t.Fatalf("after removal = %+v, %q", m.sel, consoleText(m))
	}
	// A late, older board is ignored.
	m.setBoard(board(3, run("p", "z")))
	if m.board.Seq != 4 {
		t.Fatal("older board replaced newer")
	}
}

// A refresh while editing never moves the editor's target or its text.
func TestRefreshKeepsDraft(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a"), run("p", "b")))
	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "hello")
	m.setBoard(board(2, run("p", "b"), run("p", "a"), run("p", "c")))
	typed(m, " there")
	if m.mode != modeEdit || m.active.ed.String() != "hello there" || m.active.key.run != "a" {
		t.Fatalf("draft after refresh = %+v", m.active.key)
	}
	press(m, Key{Code: KeyCtrlS})
	j := <-q
	if a := j.steps[0]; a.Kind != ActNote || a.Run != "a" || a.Text != "hello there" {
		t.Fatalf("note action = %+v", a)
	}
	// A failed save keeps the draft; a successful one clears it.
	m.finished(jobResult{job: j, results: []Result{{Message: "refused"}}})
	if m.drafts[*j.draft] == nil || m.dirtyDrafts() != 1 {
		t.Fatal("failed save dropped the draft")
	}
	press(m, Key{Code: KeyRune, Rune: 'n'})
	if m.active.ed.String() != "hello there" {
		t.Fatal("reopened draft lost text")
	}
	press(m, Key{Code: KeyCtrlS})
	j = <-q
	m.finished(jobResult{job: j, results: []Result{{OK: true, Message: "noted"}}})
	if m.dirtyDrafts() != 0 {
		t.Fatal("saved draft kept")
	}
}

// An uncertain save keeps the draft with its evidence and needs a second
// Ctrl-S before anything is resubmitted.
func TestUncertainSaveNeedsConfirmation(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "x")
	press(m, Key{Code: KeyCtrlS})
	j := <-q
	m.finished(jobResult{job: j, results: []Result{{Uncertain: true, Message: "commit outcome unknown", Lines: []string{"readback: not recorded"}}}})
	press(m, Key{Code: KeyRune, Rune: 'n'}, Key{Code: KeyCtrlS})
	select {
	case <-q:
		t.Fatal("resubmitted without confirmation")
	default:
	}
	if v := strings.Join(m.detailLinesForTest(), "\n"); !strings.Contains(v, "readback: not recorded") {
		t.Fatalf("evidence not shown:\n%s", v)
	}
	press(m, Key{Code: KeyCtrlS})
	if j := <-q; j.steps[0].Text != "x" {
		t.Fatal("confirmed resubmission lost text")
	}
}

func (m *model) detailLinesForTest() []string {
	it, ok := m.selected()
	lines, _ := m.detailLines(it, ok, m.width-1, 100)
	return lines
}

// An answer is bound to the question displayed when it was opened.
func TestAnswerBindsQuestion(t *testing.T) {
	m, q := testModel(t)
	a := run("p", "a")
	a.Question = &Question{ID: 4, Text: "which?"}
	m.setBoard(board(1, a))
	press(m, Key{Code: KeyRune, Rune: 'a'})
	typed(m, "that one")
	a.Question = &Question{ID: 5, Text: "and now?"}
	m.setBoard(board(2, a))
	press(m, Key{Code: KeyCtrlS})
	if got := (<-q).steps[0]; got.Kind != ActAnswer || got.QuestionID != 4 {
		t.Fatalf("answer = %+v", got)
	}
}

// Idea drafts offer save only (the default), save+tag sdlc and
// save+tag design-only; the tag rides the same captured identity.
func TestIdeaSaveChoices(t *testing.T) {
	for _, tc := range []struct {
		key    Key
		tag    string
		design bool
	}{
		{Key{Code: KeyEnter}, "", false},
		{Key{Code: KeyRune, Rune: 'g'}, "sdlc", false},
		{Key{Code: KeyRune, Rune: 'd'}, "sdlc", true},
	} {
		m, q := testModel(t)
		m.setBoard(board(1, idea("p", "i", 1)))
		press(m, Key{Code: KeyRune, Rune: 'e'}, Key{Code: KeyEnd})
		typed(m, "more")
		press(m, Key{Code: KeyCtrlS})
		if m.mode != modeSave {
			t.Fatal("no save choice for an idea")
		}
		press(m, tc.key)
		j := <-q
		if j.steps[0].Kind != ActEdit || j.steps[0].Expected != "# i\n" || j.steps[0].Text != "# i\nmore" {
			t.Fatalf("save step = %+v", j.steps[0])
		}
		if tc.tag == "" {
			if len(j.steps) != 1 {
				t.Fatalf("save only tagged: %+v", j.steps)
			}
			continue
		}
		if len(j.steps) != 2 || j.steps[1].Kind != ActTag || j.steps[1].Run != "i" || j.steps[1].Workflow != tc.tag || j.steps[1].DesignOnly != tc.design {
			t.Fatalf("steps = %+v", j.steps)
		}
		// Saved but tag failed: both said, retry offered, draft gone.
		m.finished(jobResult{job: j, results: []Result{{OK: true, Message: "saved"}, {Message: "tag refused"}}})
		if !strings.Contains(consoleText(m), "saved") || !strings.Contains(consoleText(m), "tag refused") || !strings.Contains(consoleText(m), "retry with :tag") {
			t.Fatalf("message = %q", consoleText(m))
		}
		if m.dirtyDrafts() != 0 {
			t.Fatal("saved draft kept")
		}
	}
}

// The worker never tags after a failed save.
func TestWorkerStopsAfterFailedSave(t *testing.T) {
	jobs, results := make(chan job, 1), make(chan jobResult, 1)
	var did []ActionKind
	go worker(func(a Action) Result { did = append(did, a.Kind); return Result{} }, jobs, results)
	jobs <- job{steps: []Action{{Kind: ActEdit}, {Kind: ActTag}}}
	r := <-results
	close(jobs)
	if len(r.results) != 1 || len(did) != 1 || did[0] != ActEdit {
		t.Fatalf("ran %v", did)
	}
}

func TestCommands(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a"), idea("p", "i", 1)))

	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "finish")
	if row, _ := m.footerRow(200); !strings.Contains(row, "p/a: ships this run with !!") {
		t.Fatalf("finish hint = %q", row)
	}
	// A refresh while typing does not retarget the command.
	m.setBoard(board(2, idea("p", "i", 1), run("p", "a")))
	press(m, Key{Code: KeyDown}) // no effect in the command line
	press(m, Key{Code: KeyEnter})
	if a := (<-q).steps[0]; a.Kind != ActControl || a.Control != "finish" || a.Run != "a" {
		t.Fatalf("finish = %+v", a)
	}
	m.busy = nil

	// park carries the owner generation; resume the hold revision; an
	// uncertain outcome's id is reused by the retry.
	m.sel = itemKey{"p", "a", 0}
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "park")
	press(m, Key{Code: KeyEnter})
	j := <-q
	if a := j.steps[0]; a.Generation != "g1" {
		t.Fatalf("park = %+v", a)
	}
	m.finished(jobResult{job: j, results: []Result{{Uncertain: true, RequestID: "req-1"}}})
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "park")
	press(m, Key{Code: KeyEnter})
	if a := (<-q).steps[0]; a.RequestID != "req-1" {
		t.Fatalf("retry id = %q", a.RequestID)
	}
	m.busy = nil
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "resume")
	press(m, Key{Code: KeyEnter})
	if a := (<-q).steps[0]; a.HoldRevision != 3 || a.RequestID != "" {
		t.Fatalf("resume = %+v", a)
	}
	m.busy = nil

	// Controls refuse a non-run; close asks, naming the item.
	m.sel = itemKey{"p", "i", 1}
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "pause")
	press(m, Key{Code: KeyEnter})
	if !strings.Contains(consoleText(m), "select a live operator-paced run") {
		t.Fatalf("pause on idea = %q", consoleText(m))
	}
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "close")
	press(m, Key{Code: KeyEnter})
	if m.mode != modeConfirm || !strings.Contains(m.confirm.prompt, "Close idea p/i") {
		t.Fatalf("close prompt = %+v", m.confirm)
	}
	press(m, Key{Code: KeyRune, Rune: 'n'})
	select {
	case <-q:
		t.Fatal("declined close ran")
	default:
	}
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "close")
	press(m, Key{Code: KeyEnter}, Key{Code: KeyRune, Rune: 'y'})
	if a := (<-q).steps[0]; a.Kind != ActClose || a.Run != "i" {
		t.Fatalf("close = %+v", a)
	}
	m.busy = nil

	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "tag design")
	press(m, Key{Code: KeyEnter})
	if a := (<-q).steps[0]; a.Kind != ActTag || a.Workflow != "sdlc" || !a.DesignOnly {
		t.Fatalf("tag design = %+v", a)
	}
	m.busy = nil
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "untag")
	press(m, Key{Code: KeyEnter})
	if a := (<-q).steps[0]; a.Kind != ActTag || a.Workflow != "" || a.DesignOnly {
		t.Fatalf("untag = %+v", a)
	}
	m.busy = nil

	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "rm -rf /")
	press(m, Key{Code: KeyEnter})
	if !strings.Contains(consoleText(m), `unknown command "rm"`) {
		t.Fatalf("unknown = %q", consoleText(m))
	}
}

// new idea opens a seeded draft and creates nothing until it is saved —
// untagged unless the operator chooses a tag.
func TestNewCapture(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "new idea p/fresh-thing")
	press(m, Key{Code: KeyEnter})
	if m.mode != modeEdit || m.active.ed.String() != "# fresh-thing\n" {
		t.Fatalf("new idea draft = %v %q", m.mode, m.active.ed.String())
	}
	select {
	case <-q:
		t.Fatal("created before save")
	default:
	}
	typed(m, "body")
	press(m, Key{Code: KeyCtrlS}, Key{Code: KeyEnter})
	j := <-q
	if a := j.steps[0]; len(j.steps) != 1 || a.Kind != ActCreate || a.Workflow != "idea" || a.Run != "fresh-thing" || a.Text != "# fresh-thing\nbody" {
		t.Fatalf("create = %+v", j.steps)
	}
	m.finished(jobResult{job: j, results: []Result{{OK: true}}})
	m.setBoard(board(2, run("p", "a"), idea("p", "fresh-thing", 3)))
	if m.sel.run != "fresh-thing" {
		t.Fatalf("created idea not selected: %+v", m.sel)
	}

	press(m, Key{Code: KeyRune, Rune: ':'})
	typed(m, "new intent p/Bad Slug")
	press(m, Key{Code: KeyEnter})
	if m.mode != modeNormal || !strings.Contains(consoleText(m), "usage") && !strings.Contains(consoleText(m), "kebab") {
		t.Fatalf("bad slug = %v %q", m.mode, consoleText(m))
	}
}

func TestQuitAsksAboutDrafts(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	if m.key(Key{Code: KeyRune, Rune: 'q'}) != effQuit {
		t.Fatal("clean quit asked")
	}
	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "unsaved")
	press(m, Key{Code: KeyEsc})
	if m.key(Key{Code: KeyRune, Rune: 'q'}) == effQuit || !strings.Contains(m.confirm.prompt, "Discard 1 unsaved draft") {
		t.Fatal("dirty quit did not ask")
	}
	if m.key(Key{Code: KeyRune, Rune: 'n'}) == effQuit || m.dirtyDrafts() != 1 {
		t.Fatal("declined quit lost the draft")
	}
	m.key(Key{Code: KeyRune, Rune: 'q'})
	if m.key(Key{Code: KeyRune, Rune: 'y'}) != effQuit {
		t.Fatal("confirmed quit did not quit")
	}
}

// What the view paints: a colourless selection marker, external control
// bytes made visible, SGR kept, and a too-small terminal that loses
// nothing.
func TestViewIsSafe(t *testing.T) {
	m, _ := testModel(t)
	a := run("p", "a")
	a.Detail = []string{"title \x1b]0;pwned\x07 here"}
	b := board(1, a)
	b.Lines[0] = "  \x1b[1mp/a\x1b[0m \x1b[2J\x9b"
	m.setBoard(b)
	f := m.view()
	got := strings.Join(f.rows, "\n")
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x9b"} {
		if strings.Contains(got, bad) {
			t.Fatalf("painted raw %q:\n%q", bad, got)
		}
	}
	if !strings.HasPrefix(f.rows[m.layout().boardRow], "> \x1b[1mp/a") || !strings.Contains(got, "^[]0;pwned^G") {
		t.Fatalf("rows = %q", f.rows[:3])
	}
	for i, r := range f.rows {
		if w := textWidth(stripSGR(r)); w > m.width-1 {
			t.Fatalf("row %d is %d cells wide", i, w)
		}
	}

	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "draft")
	m.resize(5, 20)
	if f := m.view(); !strings.Contains(f.rows[0], "too small") {
		t.Fatalf("small = %q", f.rows[0])
	}
	m.resize(30, 100)
	if m.active == nil || m.active.ed.String() != "draft" || m.sel.run != "a" {
		t.Fatal("resize lost state")
	}
}

func stripSGR(s string) string {
	var b strings.Builder
	p := []byte(s)
	for len(p) > 0 {
		if n := cliCSILen(p); n > 0 {
			p = p[n:]
			continue
		}
		b.WriteByte(p[0])
		p = p[1:]
	}
	return b.String()
}

// Keys the save dialogue does not name leave it open with its draft;
// Esc and Ctrl-C go back to the editor. Each step paints, as the loop
// does after every key.
func TestIdeaSaveDialogueKeepsState(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1)))
	press(m, Key{Code: KeyRune, Rune: 'e'})
	for _, k := range []Key{{Code: KeyUp}, {Code: KeyTab}, {Code: KeyCtrlS}, {Code: KeyRune, Rune: 'x'}, {Code: KeyPaste, Text: "y"}} {
		press(m, Key{Code: KeyCtrlS})
		press(m, k)
		m.view()
		if m.mode != modeSave || m.saveDraft == nil {
			t.Fatalf("after %+v: mode %d, draft %v", k, m.mode, m.saveDraft)
		}
		press(m, Key{Code: KeyEsc})
		m.view()
		if m.mode != modeEdit || m.active == nil {
			t.Fatalf("Esc did not return to the editor: mode %d", m.mode)
		}
	}
	press(m, Key{Code: KeyCtrlS}, Key{Code: KeyCtrlC})
	m.view()
	if m.mode != modeEdit || m.active == nil {
		t.Fatalf("Ctrl-C did not return to the editor: mode %d", m.mode)
	}
	press(m, Key{Code: KeyCtrlS}, Key{Code: KeyRune, Rune: 's'})
	if j := <-q; j.steps[0].Kind != ActEdit {
		t.Fatalf("save = %+v", j.steps[0])
	}
}

// A draft whose save is pending cannot be reopened, so the save's
// acknowledgement never clears text it did not carry.
func TestPendingSaveLocksItsDraft(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a"), idea("p", "i", 1)))
	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "first")
	press(m, Key{Code: KeyCtrlS})
	j := <-q
	press(m, Key{Code: KeyRune, Rune: 'n'})
	if m.mode != modeEdit {
		typed(m, " second") // lands nowhere: normal mode ignores it
	}
	if m.mode == modeEdit || !strings.Contains(consoleText(m), "wait for its outcome") {
		t.Fatalf("reopened a pending draft: mode %d, %q", m.mode, consoleText(m))
	}
	// Other drafts stay open to edit.
	press(m, Key{Code: KeyDown}, Key{Code: KeyRune, Rune: 'e'})
	if m.mode != modeEdit {
		t.Fatal("an unrelated draft was locked")
	}
	press(m, Key{Code: KeyEsc}, Key{Code: KeyUp})
	m.finished(jobResult{job: j, results: []Result{{OK: true}}})
	press(m, Key{Code: KeyRune, Rune: 'n'})
	if m.mode != modeEdit || m.active.ed.String() != "" {
		t.Fatalf("after the save: mode %d, text %q", m.mode, m.active.ed.String())
	}
}

// An uncertain control is retried as the request it was: same id, same
// owner generation and hold revision, even after a refresh shows a new
// owner or hold.
func TestUncertainControlRetriesOriginal(t *testing.T) {
	for _, verb := range []string{"park", "resume"} {
		m, q := testModel(t)
		m.setBoard(board(1, run("p", "a")))
		press(m, Key{Code: KeyRune, Rune: ':'})
		typed(m, verb)
		press(m, Key{Code: KeyEnter})
		j := <-q
		m.finished(jobResult{job: j, results: []Result{{Uncertain: true, RequestID: "req-1"}}})
		next := run("p", "a")
		next.Generation, next.HoldRevision = "g2", 4
		m.setBoard(board(2, next))
		press(m, Key{Code: KeyRune, Rune: ':'})
		typed(m, verb)
		press(m, Key{Code: KeyEnter})
		got := (<-q).steps[0]
		want := j.steps[0]
		want.RequestID = "req-1"
		if got != want {
			t.Fatalf("%s retry = %+v, want %+v", verb, got, want)
		}
		// Settled: the next command is a new request against what is shown.
		m.finished(jobResult{job: job{steps: []Action{got}}, results: []Result{{OK: true, RequestID: "req-1"}}})
		press(m, Key{Code: KeyRune, Rune: ':'})
		typed(m, verb)
		press(m, Key{Code: KeyEnter})
		if a := (<-q).steps[0]; a.RequestID != "" || a.Generation != "g2" || a.HoldRevision != 4 {
			t.Fatalf("%s after settling = %+v", verb, a)
		}
	}
}

// A long logical line wraps in the details, so every character of a
// capture is reachable by scrolling — closed captures too, which have no
// editor to fall back on — and no row is wider than the pane.
func TestDetailWrapsLongLines(t *testing.T) {
	long := strings.Repeat("x", 150) + "END_OF_CAPTURE"
	for _, cols := range []int{100, 40} {
		m, _ := testModel(t)
		m.resize(14, cols)
		closed := idea("p", "i", 1)
		closed.Editable = false
		closed.Body = "# i\n" + strings.Repeat("filler\n", 20) + long + "\n"
		m.setBoard(board(1, closed))
		press(m, Key{Code: KeyTab})
		seen := map[int]string{} // painted detail rows by absolute row
		for range 60 {
			f := m.view()
			for i := range m.detailHeight() {
				r := f.rows[m.layout().detailRow+i]
				if w := textWidth(r); w > cols-1 {
					t.Fatalf("%d cols: row %q is %d cells", cols, r, w)
				}
				seen[m.detailTop+i] = r
			}
			press(m, Key{Code: KeyDown})
		}
		var all []string
		for i := range len(seen) {
			all = append(all, seen[i])
		}
		if !strings.Contains(strings.Join(all, ""), long) {
			t.Fatalf("%d cols: %q never painted whole:\n%s", cols, long, strings.Join(all, "\n"))
		}
	}
}

// paintedDetail scrolls the details from the top to the bottom and
// returns every painted row in order, joined without separators so a
// wrapped line reads whole.
func paintedDetail(t *testing.T, m *model) string {
	t.Helper()
	m.focus, m.detailTop = paneDetail, 0
	seen := map[int]string{}
	for range 200 {
		f := m.view()
		for i := range m.detailHeight() {
			r := f.rows[m.layout().detailRow+i]
			if w := textWidth(r); w > m.width-1 {
				t.Fatalf("row %q is %d cells wide", r, w)
			}
			seen[m.detailTop+i] = r
		}
		press(m, Key{Code: KeyDown})
	}
	var all []string
	for i := range len(seen) {
		all = append(all, seen[i])
	}
	return strings.Join(all, "")
}

// An outcome — a refusal with its recovery, or a save whose tag failed —
// stays reachable in the console and target details at every width.
func TestOutcomeReachable(t *testing.T) {
	const refusal = "the canvas changed since you opened it; nothing saved — Ctrl-X discards your draft, then e opens the current text"
	for _, cols := range []int{40, 80, 100} {
		m, q := testModel(t)
		m.resize(14, cols)
		b := board(1, idea("p", "i", 1))
		b.Header = "all projects · gathered 15:04:05 · 0 live"
		m.setBoard(b)
		press(m, Key{Code: KeyRune, Rune: 'e'})
		typed(m, "more")
		press(m, Key{Code: KeyCtrlS}, Key{Code: KeyRune, Rune: 's'})
		m.finished(jobResult{job: <-q, results: []Result{{Changed: true, Message: refusal}}})
		if got := strings.Join(m.consoleRows(), ""); !strings.Contains(got, refusal) {
			t.Fatalf("%d cols: console missing refusal: %q", cols, got)
		}
		if got := paintedDetail(t, m); !strings.Contains(got, refusal) {
			t.Fatalf("%d cols: refusal not reachable in details:\n%s", cols, got)
		}
		// Back in the editor, the refusal heads the draft.
		press(m, Key{Code: KeyRune, Rune: 'e'})
		if head := strings.Join(m.detailLinesForTest()[:3], ""); !strings.Contains(head, "! the canvas changed") {
			t.Fatalf("%d cols: editor head %q", cols, head)
		}
		press(m, Key{Code: KeyCtrlS}, Key{Code: KeyRune, Rune: 'g'})
		m.finished(jobResult{job: <-q, results: []Result{{OK: true, Message: "saved p/i"}, {Message: "tag: idea p/i is promoted, not in progress"}}})
		if got := strings.Join(m.consoleRows(), ""); !strings.Contains(got, "saved p/i") || !strings.Contains(got, "tag: idea p/i") {
			t.Fatalf("console missing partial outcome: %q", got)
		}
		got := paintedDetail(t, m)
		for _, want := range []string{"saved p/i", "tag: idea p/i is promoted, not in progress", "retry with :tag sdlc"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%d cols: %q not reachable in details:\n%s", cols, want, got)
			}
		}
	}
}

// An answer kept from a question that closed or was replaced comes back
// under a, still bound to its own question, says so, and can be
// discarded; then a reaches the open question, if there is one.
func TestStaleAnswerRecoverable(t *testing.T) {
	for _, replaced := range []bool{true, false} {
		m, q := testModel(t)
		a := run("p", "a")
		a.Question = &Question{ID: 1, Text: "which?"}
		m.setBoard(board(1, a))
		press(m, Key{Code: KeyRune, Rune: 'a'})
		typed(m, "precious operator answer")
		a.Question = nil
		if replaced {
			a.Question = &Question{ID: 2, Text: "and now?"}
		}
		m.setBoard(board(2, a))
		press(m, Key{Code: KeyCtrlS})
		j := <-q
		if j.steps[0].QuestionID != 1 {
			t.Fatalf("submitted against #%d", j.steps[0].QuestionID)
		}
		m.finished(jobResult{job: j, results: []Result{{Message: "question #1 is no longer open"}}})
		if row, _ := m.footerRow(200); !strings.Contains(row, "a answer") {
			t.Fatalf("footer = %q", row)
		}
		press(m, Key{Code: KeyRune, Rune: 'a'})
		if m.mode != modeEdit || m.active.key.question != 1 || m.active.ed.String() != "precious operator answer" {
			t.Fatalf("replaced=%v: reopened %+v", replaced, m.active)
		}
		if head := strings.Join(m.detailLinesForTest(), "\n"); !strings.Contains(head, "question #1 is no longer open: saving will be refused") {
			t.Fatalf("head = %s", head)
		}
		press(m, Key{Code: KeyCtrlX}, Key{Code: KeyRune, Rune: 'y'})
		if m.dirtyDrafts() != 0 {
			t.Fatal("discard kept the answer")
		}
		press(m, Key{Code: KeyRune, Rune: 'a'})
		if replaced {
			if m.mode != modeEdit || m.active.key.question != 2 || m.active.ed.String() != "" {
				t.Fatalf("after discard: %+v", m.active)
			}
		} else if m.mode == modeEdit {
			t.Fatal("opened an answer with no open question")
		}
	}
}

// A draft outlives its target's eligibility: once a capture closes or a
// run finishes, the key that opened its draft still reopens it to read
// or discard, the details say so, and the save stays the writer's to
// refuse. A target missing from the source keeps a read-only draft row;
// a closed intent still on the source stays listed after draft discard.
func TestDraftReachableAfterTargetCloses(t *testing.T) {
	intentBody := "# y\nintent canvas\n" + strings.Repeat("standing direction\n", 30) + strings.Repeat("x", 164) + "END_OF_INTENT\n"
	cases := []struct {
		name  string
		item  Item
		key   rune
		close func(Item) []Item // the target's items on the next board
	}{
		{"idea body", idea("p", "x", 1), 'e', func(it Item) []Item { it.Editable, it.Notes = false, false; return []Item{it} }},
		{"intent body", Item{Project: "p", Run: "y", Kind: ItemIntent, Editable: true, Body: intentBody}, 'e', func(it Item) []Item { it.Editable = false; it.Bucket = 4; return []Item{it} }},
		{"off-board intent body", Item{Project: "p", Run: "y", Kind: ItemIntent, Editable: true, Body: "# y\nintent canvas\n"}, 'e', func(Item) []Item { return nil }},
		{"run note", run("p", "a"), 'n', func(it Item) []Item { it.Notes, it.Controls = false, false; return []Item{it} }},
	}
	for _, c := range cases {
		for _, submit := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/submit=%v", c.name, submit), func(t *testing.T) {
				m, q := testModel(t)
				other := idea("p", "z", 1)
				m.setBoard(board(1, c.item, other))
				press(m, Key{Code: KeyRune, Rune: c.key})
				typed(m, "precious operator text")
				m.setBoard(board(2, append(c.close(c.item), other)...))
				if submit {
					press(m, Key{Code: KeyCtrlS})
					j := <-q
					if j.steps[0].Run != c.item.Run {
						t.Fatalf("submitted to %s", j.steps[0].Run)
					}
					m.finished(jobResult{job: j, results: []Result{{Message: "no longer open"}}})
				}
				press(m, Key{Code: KeyEsc})
				if m.mode != modeNormal || m.dirtyDrafts() != 1 {
					t.Fatalf("mode=%v dirty=%d", m.mode, m.dirtyDrafts())
				}
				if it, ok := m.selected(); !ok || it.Run != c.item.Run {
					t.Fatalf("selected %+v, message %q", it, consoleText(m))
				}
				detail := strings.Join(m.detailLinesForTest(), "\n")
				if !strings.Contains(detail, string(c.key)+" to continue it") || !strings.Contains(detail, "saving will be refused; Ctrl-X discards it") {
					t.Fatalf("detail = %s", detail)
				}
				if c.item.Kind == ItemIntent && !strings.Contains(detail, "intent canvas") {
					t.Fatalf("closed intent's body not shown: %s", detail)
				}
				press(m, Key{Code: KeyRune, Rune: c.key})
				if m.mode != modeEdit || m.active.key.run != c.item.Run || !strings.Contains(m.active.ed.String(), "precious operator text") {
					t.Fatalf("reopen: mode=%v message=%q", m.mode, consoleText(m))
				}
				press(m, Key{Code: KeyCtrlX}, Key{Code: KeyRune, Rune: 'y'})
				if m.dirtyDrafts() != 0 || m.mode != modeNormal {
					t.Fatalf("discard: dirty=%d mode=%v", m.dirtyDrafts(), m.mode)
				}
				if c.close(c.item) == nil {
					for _, it := range m.board.Items {
						if it.Run == c.item.Run {
							t.Fatal("kept row outlived its discarded draft")
						}
					}
					if strings.Contains(strings.Join(m.board.Lines, "\n"), keptHeading) {
						t.Fatal("kept heading outlived its last draft")
					}
				} else if it, ok := m.selected(); !ok || it.Run != c.item.Run || it.Body != c.item.Body {
					t.Fatalf("canonical target lost after discard: %+v", it)
				}
				if c.name == "intent body" {
					got := paintedDetail(t, m)
					if !strings.Contains(got, strings.ReplaceAll(strings.TrimRight(intentBody, "\n"), "\n", "")) || strings.Contains(got, "precious operator text") {
						t.Fatalf("canonical body after draft discard: %s", got)
					}
				}
				press(m, Key{Code: KeyRune, Rune: c.key})
				if m.mode == modeEdit && m.active.key.run == c.item.Run {
					t.Fatal("opened a fresh draft on a closed target")
				}
			})
		}
	}
}

// With no draft at all, both a newly closed intent and one already
// closed at startup remain selectable with every body character reachable.
func TestClosedIntentInspectable(t *testing.T) {
	long := strings.Repeat("x", 164) + "END_OF_INTENT"
	body := "# Direction\n" + strings.Repeat("standing direction\n", 30) + long + "\n"
	for _, startup := range []bool{false, true} {
		for _, width := range []int{40, 100} {
			t.Run(fmt.Sprintf("startup=%v/width=%d", startup, width), func(t *testing.T) {
				m, q := testModel(t)
				m.resize(14, width)
				closed := Item{Project: "p", Run: "intent", Kind: ItemIntent, Bucket: 4, Body: body}
				other := idea("p", "neighbour", 3)
				if !startup {
					open := closed
					open.Editable, open.Bucket = true, 1
					m.setBoard(board(1, other, open))
					press(m, Key{Code: KeyDown})
				}
				m.setBoard(board(2, other, closed))
				if startup {
					press(m, Key{Code: KeyDown})
				}
				if it, ok := m.selected(); !ok || it.Run != closed.Run || len(m.drafts) != 0 {
					t.Fatalf("closed selection = %+v, drafts=%d", it, len(m.drafts))
				}
				if got := paintedDetail(t, m); !strings.Contains(got, strings.ReplaceAll(strings.TrimRight(body, "\n"), "\n", "")) {
					t.Fatalf("closed body not fully reachable: %s", got)
				}
				press(m, Key{Code: KeyRune, Rune: 'e'}, Key{Code: KeyRune, Rune: 'n'})
				for _, command := range []string{"edit", "close", "pause", "tag sdlc"} {
					press(m, Key{Code: KeyRune, Rune: ':'})
					typed(m, command)
					press(m, Key{Code: KeyEnter})
					if m.mode != modeNormal {
						t.Fatalf("closed intent accepted %q: mode %v", command, m.mode)
					}
				}
				if len(q) != 0 || len(m.drafts) != 0 {
					t.Fatal("inspection enabled a write or fresh draft")
				}
			})
		}
	}
}

func consoleText(m *model) string { return strings.Join(m.consoleRows(), "") }
