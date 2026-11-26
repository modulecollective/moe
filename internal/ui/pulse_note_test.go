package ui

import (
	"strings"
	"testing"
)

// `p` opens a note for the pulse with no selection at all, and saving it
// emits ActPulseNote naming no run. The `:pulse-note` command opens the
// same draft, so a kept one comes back from either door.
func TestPulseNoteDraft(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1)) // an empty board: nothing selected
	press(m, Key{Code: KeyRune, Rune: 'p'})
	if m.mode != modeEdit || m.active == nil || m.active.key != pulseNoteKey {
		t.Fatalf("p did not open the pulse-note draft: mode %v, active %+v", m.mode, m.active)
	}
	typed(m, "do the lightpool fix next")
	press(m, Key{Code: KeyEsc}) // kept, not saved
	if len(q) != 0 || m.dirtyDrafts() != 1 {
		t.Fatal("Esc saved or dropped the draft")
	}
	keyboard(m, ":pulse-note\r")
	if m.active == nil || m.active.ed.String() != "do the lightpool fix next" {
		t.Fatalf(":pulse-note did not reopen the kept draft: %+v", m.active)
	}
	press(m, Key{Code: KeyCtrlS})
	j := <-q
	if a := j.steps[0]; a.Kind != ActPulseNote || a.Project != "" || a.Run != "" || a.Text != "do the lightpool fix next" {
		t.Fatalf("pulse-note action = %+v", a)
	}
	if !strings.Contains(j.label, "note for the pulse") || j.steps[0].describe() != "note to the pulse" {
		t.Fatalf("label %q / describe %q", j.label, j.steps[0].describe())
	}
	if !strings.Contains(helpText, "p              write a note to the next pulse") || !strings.Contains(helpText, "pulse-note") {
		t.Fatal("help does not list the pulse note")
	}
}
