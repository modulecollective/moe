package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestStageDropsTrackedEditOutsideWriteSet: an sdlc design turn whose
// agent also edits another run's canvas closes clean. The stray edit is
// reset after the turn's commit and named on stderr, so the close's
// rebase never sees a dirty tree and no resolve agent is launched.
func TestStageDropsTrackedEditOutsideWriteSet(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	seedIdea(t, root, "tele", "bystander")
	ideaRel := run.ContentPath("tele", "bystander", dash.IdeaDocID)
	ideaBefore, _ := os.ReadFile(filepath.Join(root, ideaRel))

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
printf 'design body\n' >> "$canvas"
printf 'stray\n' >> "$wt/`+ideaRel+`"
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/strays"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := openSdlcDesign("tele", "strays", true, "", &out, &errb); code != 0 {
		t.Fatalf("design exit=%d stderr=%s", code, errb.String())
	}
	if want := "dropped edit outside this turn's write set: " + ideaRel; !strings.Contains(errb.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, errb.String())
	}
	if strings.Contains(errb.String(), "launching an agent") {
		t.Errorf("a resolve agent was launched:\n%s", errb.String())
	}
	if after, _ := os.ReadFile(filepath.Join(root, ideaRel)); !bytes.Equal(after, ideaBefore) {
		t.Errorf("idea canvas changed on main: %q", after)
	}
	body, _ := os.ReadFile(filepath.Join(root, run.ContentPath("tele", "strays", "design")))
	if !strings.Contains(string(body), "design body") {
		t.Errorf("design canvas did not land: %q", body)
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("tree dirty after the turn:\n%s", st)
	}
}

// TestRefusedCommitKeepsStrayEdits: a turn whose commit is refused for
// a missing canvas returns before the drop, so the worktree keeps its
// tracked dirt as evidence.
func TestRefusedCommitKeepsStrayEdits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedIdea(t, root, "tele", "bystander")
	if _, err := run.New(root, "tele", run.Options{ID: "strays", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	md := mustLoad(t, root, "tele", "strays")
	ideaRel := run.ContentPath("tele", "bystander", dash.IdeaDocID)
	if err := os.WriteFile(filepath.Join(root, ideaRel), []byte("stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	s := &stageSession{root: root, projectID: "tele", runID: "strays", docID: "design", md: md, agentName: "claude", stderr: &errb}
	err := s.commitStager(root, nil)
	if _, ok := err.(*canvasRefusalError); !ok {
		t.Fatalf("commitStager = %v, want a canvas refusal", err)
	}
	if st := gittest.Output(t, root, "status", "--porcelain", "--", ideaRel); !strings.Contains(st, ideaRel) {
		t.Errorf("refused turn dropped the stray edit; status %q", st)
	}
	if strings.Contains(errb.String(), "dropped edit") {
		t.Errorf("refused turn printed a drop:\n%s", errb.String())
	}
}
