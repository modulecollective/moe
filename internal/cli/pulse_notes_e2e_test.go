package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
)

// pulseNotesFake is a fake agent that dumps its argv to $NOTE_DUMP and,
// unless $NOTE_GATE is "none", writes a concluded gate. With $NOTE_LATE
// set it also lands a second pulse note on the canonical root mid-turn,
// the way an operator typing while the sweep runs would.
const pulseNotesFake = `#!/bin/sh
printf '%s\n' "$@" > "$NOTE_DUMP"
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
[ -n "$canvas" ] || exit 1
wt=${canvas%%/projects/*}
root=${wt%%/.moe/worktrees/*}
if [ -n "$NOTE_LATE" ]; then
  printf '%s\n' '{"notes":[{"id":1,"text":"bump Go everywhere\n\nstart with moe"},{"id":2,"text":"late note"}]}' > "$root/projects/_cabinet/pulse-notes.json"
  git -C "$root" add projects/_cabinet/pulse-notes.json
  git -C "$root" commit -q -m "input: pulse note #2" >/dev/null
fi
[ "$NOTE_GATE" = "none" ] && exit 0
ticks=$(printf '\140\140\140')
printf '%s\n' '# Pulse' '' '## Operator notes' '' '- #1 filed.' '' '## Gate' '' "${ticks}json" '{"status":"ok"}' "$ticks" > "$canvas"
exit 0
`

func pulseNotesBureaucracy(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	fakeClaudeOnPath(t, pulseNotesFake)
	return root
}

func sweepDump(t *testing.T, root string) (string, int, string) {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "argv")
	t.Setenv("NOTE_DUMP", dump)
	var out, errb bytes.Buffer
	code := runPulseSurvey(root, project.RootID, "", nil, &out, &errb)
	argv, _ := os.ReadFile(dump)
	return string(argv), code, errb.String()
}

// The whole loop on a real sweep: the note reaches the prompt in full,
// a concluded sweep stamps it with its own run, a later note landed
// mid-turn stays pending and reaches the next sweep, and a sweep with
// nothing pending renders no block.
func TestPulseNotesReachTheSurveyOnceEndToEnd(t *testing.T) {
	root := pulseNotesBureaucracy(t)
	if _, err := input.AddPulse(root, "bump Go everywhere\n\nstart with moe"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NOTE_LATE", "1")
	argv, code, errs := sweepDump(t, root)
	if code != 0 {
		t.Fatalf("sweep exit=%d stderr=%s", code, errs)
	}
	for _, want := range []string{"\n# Notes from the operator\n", "- #1: bump Go everywhere\n\n  start with moe", "`## Operator notes`"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "late note") {
		t.Fatalf("the mid-turn note reached a prompt rendered before it:\n%s", argv)
	}
	f, err := input.LoadPulse(root)
	if err != nil {
		t.Fatal(err)
	}
	first := f.Notes[0].DeliveredTo
	if !strings.HasPrefix(first, project.RootID+"/pulse") || !f.Notes[1].Pending() {
		t.Fatalf("notes after the first sweep = %+v; want #1 read by the pulse, #2 pending", f.Notes)
	}
	// The stamp precedes the close, so the close stays the project tip.
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); !strings.HasPrefix(subj, "Close pulse run") {
		t.Fatalf("tip after the sweep = %q, want the pulse close", subj)
	}
	if stamp := gittest.Output(t, root, "log", "-1", "--format=%B", "--grep=^Read pulse notes #1 in "); !strings.Contains(stamp, "MoE-Workflow: pulse") {
		t.Fatalf("stamp commit = %q", stamp)
	}

	t.Setenv("NOTE_LATE", "")
	argv, code, errs = sweepDump(t, root)
	if code != 0 {
		t.Fatalf("second sweep exit=%d stderr=%s", code, errs)
	}
	if !strings.Contains(argv, "- #2: late note") || strings.Contains(argv, "#1: bump Go") {
		t.Fatalf("second prompt should carry only the late note:\n%s", argv)
	}

	argv, code, errs = sweepDump(t, root)
	if code != 0 {
		t.Fatalf("third sweep exit=%d stderr=%s", code, errs)
	}
	if strings.Contains(argv, "\n# Notes from the operator\n") {
		t.Fatalf("a sweep with nothing pending rendered a notes block:\n%s", argv)
	}
}

// A sweep that leaves its gate unfilled concluded nothing, so the note
// stays pending and the next sweep renders it again.
func TestPulseNoteSurvivesAnUnfilledGate(t *testing.T) {
	root := pulseNotesBureaucracy(t)
	if _, err := input.AddPulse(root, "do the lightpool fix next"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOTE_GATE", "none")
	argv, code, _ := sweepDump(t, root)
	if code == 0 {
		t.Fatal("an unfilled gate reported a clean sweep")
	}
	if !strings.Contains(argv, "- #1: do the lightpool fix next") {
		t.Fatalf("prompt lacks the note:\n%s", argv)
	}
	if f, _ := input.LoadPulse(root); !f.Notes[0].Pending() {
		t.Fatalf("note stamped by a failed sweep: %+v", f.Notes[0])
	}

	t.Setenv("NOTE_GATE", "")
	argv, code, errs := sweepDump(t, root)
	if code != 0 {
		t.Fatalf("retry exit=%d stderr=%s", code, errs)
	}
	if !strings.Contains(argv, "- #1: do the lightpool fix next") {
		t.Fatalf("retry prompt lacks the redelivered note:\n%s", argv)
	}
	if f, _ := input.LoadPulse(root); f.Notes[0].Pending() {
		t.Fatalf("note still pending after a concluded sweep: %+v", f.Notes[0])
	}
}
