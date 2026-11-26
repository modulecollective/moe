package input

import (
	"errors"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// A pulse note round-trips: added as the operator's own commit under the
// root project, then stamped with the pulse run that read it, under that
// run's consent.
func TestPulseNoteRoundTrip(t *testing.T) {
	root := seedRoot(t)

	e, err := AddPulse(root, "  bump Go everywhere\n\nstart with moe  ")
	if err != nil {
		t.Fatalf("AddPulse: %v", err)
	}
	if e.ID != 1 || !e.Pending() || e.Text != "bump Go everywhere\n\nstart with moe" {
		t.Fatalf("entry = %+v, want a trimmed pending note at id 1", e)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Project: _cabinet") {
		t.Fatalf("note commit missing its project trailer:\n%s", msg)
	}
	for _, banned := range []string{"MoE-Consent:", "MoE-Input-", "MoE-Run:"} {
		if strings.Contains(msg, banned) {
			t.Fatalf("note commit carries %s:\n%s", banned, msg)
		}
	}
	if files := gittest.Output(t, root, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(files) != "projects/_cabinet/pulse-notes.json" {
		t.Fatalf("note commit touched %q, want only the pulse record", files)
	}

	if err := MarkPulseDelivered(root, "_cabinet/pulse-2026-11-26-12", []int{1}, "dynamic"); err != nil {
		t.Fatalf("MarkPulseDelivered: %v", err)
	}
	f, err := LoadPulse(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Notes[0].DeliveredTo != "_cabinet/pulse-2026-11-26-12" || f.Notes[0].Pending() {
		t.Fatalf("note = %+v, want delivered to the pulse run", f.Notes[0])
	}
	msg = gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, want := range []string{"MoE-Run: pulse-2026-11-26-12", "MoE-Project: _cabinet", "MoE-Workflow: pulse", "MoE-Consent: dynamic"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stamp commit missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "MoE-Document:") {
		t.Fatalf("stamp commit reads as a doc turn:\n%s", msg)
	}

	// Re-stamping finds nothing pending and commits nothing.
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	if err := MarkPulseDelivered(root, "_cabinet/pulse-2026-11-26-13", []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	if got := gittest.Output(t, root, "rev-parse", "HEAD"); got != head {
		t.Fatalf("a repeat stamp committed")
	}
}

// A note the operator adds after the kickoff rendered — between the
// render and the stamp — is not in the stamped id list and stays pending
// for the next sweep, even when it lands inside the stamp's own lock
// window.
func TestMarkPulseDeliveredKeepsANoteAddedMidWindow(t *testing.T) {
	root := seedRoot(t)
	if _, err := AddPulse(root, "do the lightpool fix next"); err != nil {
		t.Fatal(err)
	}
	onceLocked(t, func() {
		if err := writeFile(root+"/"+PulsePath(), File{Notes: []Entry{
			{ID: 1, Text: "do the lightpool fix next"},
			{ID: 2, Text: "and bump Go"},
		}}); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "add", "-A")
		gittest.Run(t, root, "commit", "-m", "input: pulse note #2")
	})
	if err := MarkPulseDelivered(root, "_cabinet/pulse-a", []int{1}, ""); err != nil {
		t.Fatalf("MarkPulseDelivered: %v", err)
	}
	f, err := LoadPulse(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Notes) != 2 || f.Notes[0].DeliveredTo != "_cabinet/pulse-a" || !f.Notes[1].Pending() {
		t.Fatalf("notes = %+v, want #1 read and #2 still pending", f.Notes)
	}
}

func TestPulseNoteRefusals(t *testing.T) {
	root := seedRoot(t)
	if _, err := AddPulse(root, "  \n "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("blank note err = %v, want ErrEmpty", err)
	}
	if f, err := LoadPulse(root); err != nil || len(f.Notes) != 0 {
		t.Fatalf("missing record = %+v, %v; want empty and no error", f, err)
	}
	if err := MarkPulseDelivered(root, "pulse-a", []int{1}, ""); err == nil {
		t.Fatal("an unqualified pulse run was accepted")
	}
	write(t, root, PulsePath(), `{"notes":[{"id":1,"question":"which?","asked_by":"moe/x"}]}`)
	if _, err := LoadPulse(root); !errors.Is(err, ErrMalformed) {
		t.Fatalf("a ping in the pulse record err = %v, want ErrMalformed", err)
	}
}
