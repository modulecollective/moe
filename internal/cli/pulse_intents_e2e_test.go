package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// TestPulseSurveyKeepsIntentsEndToEnd drives a real sweep with a fake
// agent on a two-project bureaucracy. The agent writes one intent's
// status.md, strays onto an idea canvas and an intent canvas, and writes
// a gate that links, opens and closes. The turn commit carries status.md
// and neither canvas; both strays are dropped and named, with no resolve
// agent; the commits after it hold the link, the new intent and the
// close; nothing is left dirty.
func TestPulseSurveyKeepsIntentsEndToEnd(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	seedIntent(t, root, "moe", "goal", "# The goal\n\nDone when: the thing ships.\n")
	seedIntent(t, root, "tele", "finished", "# Finished\n\nDone when: tele/x merged.\n")
	seedIdea(t, root, "moe", "some-idea")
	seedIdea(t, root, "tele", "incident-fix")
	ideaRel := run.ContentPath("moe", "some-idea", dash.IdeaDocID)
	intentRel := run.ContentPath("moe", "goal", dash.IntentDocID)
	statusRel := run.StatusPath("moe", "goal")
	ideaBefore, _ := os.ReadFile(filepath.Join(root, ideaRel))
	intentBefore, _ := os.ReadFile(filepath.Join(root, intentRel))

	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
wt=${canvas%%/projects/*}
ticks=$(printf '\140\140\140')
printf '%s\n' '2026-11-24, pulse' '- Landed: the first half.' > "$wt/`+statusRel+`"
printf '%s\n' '' '## Status' '' '- stray' >> "$wt/`+intentRel+`"
printf '%s\n' 'stray' >> "$wt/`+ideaRel+`"
printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","serves":[{"run":"moe/some-idea","intent":"moe/goal"},{"run":"tele/incident-fix","intent":"_cabinet/incident"}],"intents":[{"open":"_cabinet/incident","title":"Incident closed out","body":"Done when: no recurrence.","why":"two runs serve it"},{"close":"tele/finished","why":"tele/x merged"}]}' "$ticks" > "$canvas"
exit 0
`)

	var out, errb bytes.Buffer
	code := runPulseSurvey(root, project.RootID, "", nil, &out, &errb)
	if code != 0 {
		t.Fatalf("pulse exit=%d stderr=%s", code, errb.String())
	}
	for _, rel := range []string{intentRel, ideaRel} {
		if want := "dropped edit outside this turn's write set: " + rel; !strings.Contains(errb.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errb.String())
		}
	}
	if strings.Contains(errb.String(), "launching an agent") {
		t.Errorf("a resolve agent was launched:\n%s", errb.String())
	}

	turn := strings.TrimSpace(gittest.Output(t, root, "log", "-1", "--format=%H", "--grep=^Update pulse for "))
	files := gittest.Output(t, root, "show", "--name-only", "--format=", turn)
	if !strings.Contains(files, statusRel) {
		t.Errorf("turn commit lacks the intent's status.md:\n%s", files)
	}
	if strings.Contains(files, ideaRel) || strings.Contains(files, intentRel) {
		t.Errorf("turn commit staged a non-whitelisted run document:\n%s", files)
	}
	status, _ := os.ReadFile(filepath.Join(root, statusRel))
	if string(status) != "2026-11-24, pulse\n- Landed: the first half.\n" {
		t.Errorf("status.md after the sweep:\n%s", status)
	}
	if after, _ := os.ReadFile(filepath.Join(root, intentRel)); !bytes.Equal(after, intentBefore) {
		t.Errorf("intent canvas changed: %q", after)
	}
	if after, _ := os.ReadFile(filepath.Join(root, ideaRel)); !bytes.Equal(after, ideaBefore) {
		t.Errorf("idea canvas changed: %q", after)
	}

	subjects := gittest.Output(t, root, "log", "--format=%s", turn+"..HEAD")
	for _, want := range []string{"Open run _cabinet/incident", "Link 2 runs to intents", "Close intent tele/finished"} {
		if !strings.Contains(subjects, want) {
			t.Errorf("after the turn, missing %q in:\n%s", want, subjects)
		}
	}
	if got := mustLoad(t, root, "moe", "some-idea").Intent; got != "moe/goal" {
		t.Errorf("idea link = %q", got)
	}
	if got := mustLoad(t, root, "tele", "incident-fix").Intent; got != "_cabinet/incident" {
		t.Errorf("link to the opened intent = %q", got)
	}
	if md := mustLoad(t, root, "tele", "finished"); md.Status != run.StatusClosed || !strings.HasPrefix(md.ClosedBy, project.RootID+"/pulse") {
		t.Errorf("closed intent = %+v", md)
	}
	if md := mustLoad(t, root, project.RootID, "incident"); md.SpawnedBy == "" {
		t.Errorf("opened intent lacks spawned_by: %+v", md)
	}
	for _, md := range mustScan(t, root) {
		if md.Workflow == pulseWorkflow && md.Status != run.StatusClosed {
			t.Errorf("pulse run %s left %s", md.ID, md.Status)
		}
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("tree dirty after the sweep:\n%s", st)
	}
}

// TestPulseStatusMeetsAnOperatorEdit: while the sweep runs, the operator
// commits to the intent canvas — on the very lines the old in-canvas
// Status sat beside. The pulse writes only status.md, a file the
// operator never touches, so the close rebases clean and both land.
func TestPulseStatusMeetsAnOperatorEdit(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	seedIntent(t, root, "moe", "goal", "# The goal\n\nDirection.\n\nDone when: the thing ships.\n")
	seedIdea(t, root, "moe", "some-idea")
	intentRel := run.ContentPath("moe", "goal", dash.IntentDocID)
	statusRel := run.StatusPath("moe", "goal")

	fakeClaudeOnPath(t, `#!/bin/sh
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
sed -i 's/^Done when: the thing ships.$/Done when: the thing ships, twice./' "$root/`+intentRel+`"
git -C "$root" commit -q -am "operator edits the intent" >/dev/null
ticks=$(printf '\140\140\140')
printf '%s\n' '2026-11-24, pulse' '- Landed: half.' > "$wt/`+statusRel+`"
printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","serves":[{"run":"moe/some-idea","intent":"moe/goal"}]}' "$ticks" > "$canvas"
exit 0
`)
	var out, errb bytes.Buffer
	if code := runPulseSurvey(root, project.RootID, "", nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	canvas, _ := os.ReadFile(filepath.Join(root, intentRel))
	if !strings.Contains(string(canvas), "twice.") {
		t.Errorf("operator's edit lost:\n%s", canvas)
	}
	if status, _ := os.ReadFile(filepath.Join(root, statusRel)); !strings.Contains(string(status), "- Landed: half.") {
		t.Errorf("status.md did not land:\n%s", status)
	}
	if mustLoad(t, root, "moe", "some-idea").Intent != "moe/goal" {
		t.Error("gate not applied")
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("root left dirty:\n%s", st)
	}
}
