package ui

import (
	"strings"
	"testing"
)

const choreText = "trigger:\ncadence: 90d\ncooldown: 90d\nwhen:\nworkflow:\n---\n# audit\n"

func editableChore(p, r string) Item {
	return Item{Project: p, Run: r, Bucket: 2, Kind: ItemChore, Editable: true, Body: choreText, Detail: []string{"chore " + p + "/" + r}}
}

func lastSaid(m *model) string {
	if len(m.console.events) == 0 {
		return ""
	}
	return m.console.events[len(m.console.events)-1].text
}

func TestChoreEditQueuesCheckedSave(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, editableChore("p", "audit")))
	keyboard(m, "e")
	if m.mode != modeEdit || m.active == nil || m.active.key.purpose != "chore" || m.active.ed.String() != choreText {
		t.Fatalf("e did not open the chore's text form: mode=%v active=%+v", m.mode, m.active)
	}
	keyboard(m, "more\x13") // Ctrl-S
	if m.mode != modeNormal {
		t.Fatalf("a chore save opened mode %v (the idea save/tag dialogue?)", m.mode)
	}
	j := <-q
	a := j.steps[0]
	if len(j.steps) != 1 || a.Kind != ActChoreEdit || a.Expected != choreText || a.Text != choreText+"more" || a.Project != "p" || a.Run != "audit" {
		t.Fatalf("queued %+v", j.steps)
	}

	// A refusal keeps the draft with its reason.
	m.finished(jobResult{job: j, results: []Result{{Message: "line 2: cadence is set twice"}}})
	d := m.drafts[draftKey{project: "p", run: "audit", purpose: "chore"}]
	if d == nil || d.ed.String() != choreText+"more" {
		t.Fatal("refused save dropped the draft")
	}
	it, _ := m.selected()
	if detail := strings.Join(m.itemDetail(it, true), "\n"); !strings.Contains(detail, "cadence is set twice") || !strings.Contains(detail, "e to continue it") {
		t.Fatalf("detail lacks the reason or the reopen key:\n%s", detail)
	}
	// e reopens the kept draft, not a fresh copy of the body.
	keyboard(m, "e")
	if m.active != d {
		t.Fatal("e did not reopen the kept draft")
	}
	keyboard(m, "\x13")
	j = <-q
	m.finished(jobResult{job: j, results: []Result{{OK: true, Message: "saved chore p/audit"}}})
	if len(m.drafts) != 0 {
		t.Fatal("a saved draft was kept")
	}
}

func TestNewChoreCreatesOnSaveAndSelectsIt(t *testing.T) {
	m, q := testModel(t)
	// A same-name run is on the board; selection must land on the chore.
	m.setBoard(board(1, run("p", "lint")))
	keyboard(m, ":new-chore p/lint\r")
	if m.active == nil || m.active.key.purpose != "new-chore" {
		t.Fatalf("no new-chore draft: %+v", m.active)
	}
	if got := m.active.ed.String(); got != "trigger:\ncadence:\ncooldown:\nwhen:\nworkflow:\n---\n# lint\n" {
		t.Fatalf("seed %q", got)
	}
	keyboard(m, "\x13")
	j := <-q
	if j.steps[0].Kind != ActChoreCreate || j.steps[0].Run != "lint" {
		t.Fatalf("queued %+v", j.steps)
	}
	m.finished(jobResult{job: j, results: []Result{{OK: true}}})
	m.setBoard(board(2, run("p", "lint"), editableChore("p", "lint")))
	if it, _ := m.selected(); it.Kind != ItemChore || it.Run != "lint" {
		t.Fatalf("selected %+v, want the new chore", it)
	}

	for _, bad := range []string{":new-chore lint\r", ":new-chore p/Bad_Name\r", ":new-chore\r"} {
		keyboard(m, bad)
		if m.mode != modeNormal || len(q) != 0 {
			t.Fatalf("%q: mode %v, queued %d", bad, m.mode, len(q))
		}
	}
}

func TestChoreDeleteAsksThenQueues(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, editableChore("p", "audit")))
	keyboard(m, ":delete\r")
	if m.mode != modeConfirm || len(q) != 0 {
		t.Fatalf("delete did not ask: mode %v", m.mode)
	}
	keyboard(m, "n")
	if len(q) != 0 {
		t.Fatal("declined delete queued")
	}
	keyboard(m, ":delete\ry")
	j := <-q
	if j.steps[0].Kind != ActChoreDelete || j.steps[0].Run != "audit" {
		t.Fatalf("queued %+v", j.steps)
	}
}

func TestChoreOpenQueuesAndSelectsTheRun(t *testing.T) {
	m, q := testModel(t)
	// The same name as a run, to prove the selection moves to the run
	// the open created and not back to the chore.
	m.setBoard(board(1, editableChore("p", "audit")))
	keyboard(m, "o")
	j := <-q
	if j.steps[0].Kind != ActChoreOpen || j.steps[0].Run != "audit" {
		t.Fatalf("queued %+v", j.steps)
	}
	m.finished(jobResult{job: j, results: []Result{{OK: true, Opened: "audit"}}})
	m.setBoard(board(2, editableChore("p", "audit"), run("p", "audit")))
	if it, _ := m.selected(); it.Kind != ItemRun || it.Run != "audit" {
		t.Fatalf("selected %+v, want the opened run", it)
	}
	keyboard(m, ":open\r") // on the run now
	if len(q) != 0 || !strings.Contains(lastSaid(m), "select a chore") {
		t.Fatalf("open on a run: queued %d, said %q", len(q), lastSaid(m))
	}
}

// The capture verbs stay inert on a chore and say why; e on an idea
// still edits the idea.
func TestCaptureVerbsInertOnChore(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, editableChore("p", "x"), idea("p", "x", 1)))
	for _, keys := range []string{":close\r", "g", "d", "u", "n", ":tag-sdlc\r"} {
		keyboard(m, keys)
		if len(q) != 0 || m.mode != modeNormal {
			t.Fatalf("%q on a chore: queued %d, mode %v", keys, len(q), m.mode)
		}
		if lastSaid(m) == "" {
			t.Fatalf("%q said nothing", keys)
		}
	}
	// Same label, separate drafts: the idea's body draft and the chore's.
	keyboard(m, "eCHORE")
	press(m, Key{Code: KeyEsc}, Key{Code: KeyDown})
	keyboard(m, "eIDEA")
	press(m, Key{Code: KeyEsc})
	chore := m.drafts[draftKey{project: "p", run: "x", purpose: "chore"}]
	body := m.drafts[draftKey{project: "p", run: "x", purpose: "body"}]
	if chore == nil || body == nil || !strings.Contains(chore.ed.String(), "CHORE") || strings.Contains(chore.ed.String(), "IDEA") || !strings.Contains(body.ed.String(), "IDEA") || strings.Contains(body.ed.String(), "CHORE") {
		t.Fatalf("drafts mixed: %+v", m.drafts)
	}
	it, _ := m.selected()
	if detail := strings.Join(m.itemDetail(it, true), "\n"); strings.Contains(detail, "edit of chore") {
		t.Fatal("the idea's details list the chore's draft")
	}
}

// Keeping a chore draft out of the idea save dialogue must not take a
// note on an idea out of it: that save has always offered the tag.
func TestNoteOnIdeaStillOffersTheTag(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1), editableChore("p", "i")))
	press(m, Key{Code: KeyRune, Rune: 'n'})
	typed(m, "a thought")
	press(m, Key{Code: KeyCtrlS})
	if m.mode != modeSave {
		t.Fatalf("a note on an idea saved without the save/tag choice: mode=%v", m.mode)
	}
}
