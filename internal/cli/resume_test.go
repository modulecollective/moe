package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// runOneShotStageDirect drives one stage via the same path the chain
// uses, so test fixtures can stand up a "design done, code pending"
// state without typing through promptNextStage. Goes through the
// typed Command's Run with --one-shot, mirroring runOneShotChain's
// production dispatch (and the post-stage `o` prompt's), so the
// fixture reflects production semantics.
func runOneShotStageDirect(t *testing.T, projectID, runID, docID string, needsSandbox bool) {
	t.Helper()
	_ = needsSandbox // sandbox-need is now derived by the typed handler.
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatalf("lookup sdlc group: %v", err)
	}
	cmd := g.Lookup(docID)
	if cmd == nil {
		t.Fatalf("sdlc has no command for stage %q", docID)
	}
	var out, errb bytes.Buffer
	if code := cmd.Run([]string{"--one-shot", projectID, runID}, &out, &errb); code != 0 {
		t.Fatalf("one-shot %s: exit=%d stderr=%q", docID, code, errb.String())
	}
}

func TestSdlcResumeRefusesTerminalRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Will be merged"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	markRunStatus(t, root, "tele", "will-be-merged", run.StatusMerged)

	out.Reset()
	errb.Reset()
	code := runResume([]string{"tele", "will-be-merged"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal on merged run; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "merged") {
		t.Fatalf("expected merged in stderr: %q", errb.String())
	}
	// The session must not have been opened — no design dir written
	// (the stage session creates it lazily, so absence proves we never
	// reached the chain).
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "will-be-merged", "documents", "design")); !os.IsNotExist(err) {
		t.Fatalf("design dir should not exist on terminal-run refusal: err=%v", err)
	}
}

func TestSdlcResumeRefusesMissingRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runResume([]string{"tele", "no-such-run"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal on missing run; stdout=%q", out.String())
	}
}

// TestSdlcResumeInteractiveInvokesNextStage: resume invokes the next
// pending stage's interactive Run. Stub the stage's Run function so
// the test doesn't try to open a real Claude session.
func TestSdlcResumeInteractiveInvokesNextStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Interactive resume"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	// Swap the design stage's Run for a recorder. The group's
	// commands map is package-private state; mutate directly and
	// restore in cleanup. (Workflow no longer holds *Command pointers
	// after the split — dispatch lives on CommandGroup.)
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	original := g.commands["design"]
	t.Cleanup(func() { g.commands["design"] = original })

	var gotArgs []string
	g.commands["design"] = &Command{
		Name: "design",
		Run: func(args []string, _, _ io.Writer) int {
			gotArgs = append([]string(nil), args...)
			return 0
		},
	}

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"tele", "interactive-resume"}, &out, &errb); code != 0 {
		t.Fatalf("interactive resume exit=%d stderr=%q", code, errb.String())
	}
	if len(gotArgs) != 2 || gotArgs[0] != "tele" || gotArgs[1] != "interactive-resume" {
		t.Fatalf("interactive resume should invoke design with [project, run]; got %v", gotArgs)
	}
}
