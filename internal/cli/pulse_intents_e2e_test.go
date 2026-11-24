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
// agent on a two-project bureaucracy. The agent rewrites one intent's
// Status and writes a gate that links, opens and closes. The turn commit
// carries the intent canvas and no other run's document; the commits
// after it hold the link, the new intent and the close; nothing is left
// dirty.
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
	ideaBefore, _ := os.ReadFile(filepath.Join(root, ideaRel))

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
printf '%s\n' '' '## Status' '' '2026-11-24, pulse' '- Landed: the first half.' >> "$wt/projects/moe/runs/goal/documents/intent/content.md"
printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","serves":[{"run":"moe/some-idea","intent":"moe/goal"},{"run":"tele/incident-fix","intent":"_cabinet/incident"}],"intents":[{"open":"_cabinet/incident","title":"Incident closed out","body":"Done when: no recurrence.","why":"two runs serve it"},{"close":"tele/finished","why":"tele/x merged"}]}' "$ticks" > "$canvas"
exit 0
`)

	var out, errb bytes.Buffer
	code := runPulseSurvey(root, project.RootID, "", nil, &out, &errb)
	if code != 0 {
		t.Fatalf("pulse exit=%d stderr=%s", code, errb.String())
	}

	turn := strings.TrimSpace(gittest.Output(t, root, "log", "-1", "--format=%H", "--grep=^work: update pulse"))
	files := gittest.Output(t, root, "show", "--name-only", "--format=", turn)
	if !strings.Contains(files, run.ContentPath("moe", "goal", dash.IntentDocID)) {
		t.Errorf("turn commit lacks the intent canvas:\n%s", files)
	}
	if strings.Contains(files, ideaRel) {
		t.Errorf("turn commit staged a non-whitelisted run document:\n%s", files)
	}
	intent, _ := os.ReadFile(filepath.Join(root, run.ContentPath("moe", "goal", dash.IntentDocID)))
	if !strings.Contains(string(intent), "## Status\n\n2026-11-24, pulse\n- Landed: the first half.") {
		t.Errorf("intent canvas after the sweep:\n%s", intent)
	}
	if ideaAfter, _ := os.ReadFile(filepath.Join(root, ideaRel)); !bytes.Equal(ideaAfter, ideaBefore) {
		t.Errorf("idea canvas changed: %q", ideaAfter)
	}

	subjects := gittest.Output(t, root, "log", "--format=%s", turn+"..HEAD")
	for _, want := range []string{"Open run _cabinet/incident", "Link 2 run(s) to intents", "Close intent tele/finished"} {
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

// TestPulseStatusEditMeetsAnOperatorEdit is the peer-edit collision the
// design accepted. While the sweep runs, the operator commits to the
// same intent canvas. An edit to another part of the file rebases clean
// and both land; an edit to the same lines fails the sweep, leaves the
// pulse run open, applies nothing from the gate, and keeps the
// operator's edit.
func TestPulseStatusEditMeetsAnOperatorEdit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator string // sed expression the operator applies to the canvas
		clean    bool
	}{
		{"different lines", "s/^# The goal$/# The sharper goal/", true},
		{"same lines", "s/^Done when: the thing ships.$/Done when: the thing ships, twice./", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			// The pulse rewrites the Done-when line's neighbourhood by
			// appending Status straight after it; the operator's "same
			// lines" edit touches that last line.
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
sed -i '`+tc.operator+`' "$root/`+intentRel+`"
git -C "$root" commit -q -am "operator edits the intent" >/dev/null
ticks=$(printf '\140\140\140')
printf '%s\n' '' '## Status' '' '2026-11-24, pulse' '- Landed: half.' >> "$wt/`+intentRel+`"
printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","serves":[{"run":"moe/some-idea","intent":"moe/goal"}]}' "$ticks" > "$canvas"
exit 0
`)
			var out, errb bytes.Buffer
			code := runPulseSurvey(root, project.RootID, "", nil, &out, &errb)
			canvas, _ := os.ReadFile(filepath.Join(root, intentRel))
			if tc.clean {
				if code != 0 {
					t.Fatalf("exit=%d stderr=%s", code, errb.String())
				}
				if !strings.Contains(string(canvas), "# The sharper goal") || !strings.Contains(string(canvas), "- Landed: half.") {
					t.Fatalf("both edits should land:\n%s", canvas)
				}
				if mustLoad(t, root, "moe", "some-idea").Intent != "moe/goal" {
					t.Fatal("gate not applied")
				}
				return
			}
			if code == 0 {
				t.Fatalf("an overlapping edit should fail the sweep; stderr=%s", errb.String())
			}
			if !strings.Contains(errb.String(), "rebase") {
				t.Errorf("stderr does not name the rebase:\n%s", errb.String())
			}
			if !strings.Contains(string(canvas), "twice.") || strings.Contains(string(canvas), "Landed: half.") {
				t.Errorf("root canvas should hold the operator's edit only:\n%s", canvas)
			}
			if mustLoad(t, root, "moe", "some-idea").Intent != "" {
				t.Error("a failed sweep applied its gate")
			}
			open := false
			for _, md := range mustScan(t, root) {
				if md.Workflow == pulseWorkflow && md.Status == run.StatusInProgress {
					open = true
				}
			}
			if !open {
				t.Error("the failed sweep's run should stay open on the dash")
			}
			if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
				t.Errorf("root left dirty:\n%s", st)
			}
		})
	}
}
