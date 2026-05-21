package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// markRunStatus rewrites run.json's status field directly. Test helper
// for tests that need a "merged" or "closed" run without driving the
// full close path.
func markRunStatus(t *testing.T, root, projectID, runID, status string) {
	t.Helper()
	path := filepath.Join(root, "projects", projectID, "runs", runID, "run.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var md map[string]any
	if err := json.Unmarshal(b, &md); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	md["status"] = status
	out, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
	code := runResume([]string{"tele/will-be-merged"}, &out, &errb)
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
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runResume([]string{"tele/no-such-run"}, &out, &errb)
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
	if code := runResume([]string{"tele/interactive-resume"}, &out, &errb); code != 0 {
		t.Fatalf("interactive resume exit=%d stderr=%q", code, errb.String())
	}
	if len(gotArgs) != 1 || gotArgs[0] != "tele/interactive-resume" {
		t.Fatalf("interactive resume should invoke design with [project/run]; got %v", gotArgs)
	}
}
