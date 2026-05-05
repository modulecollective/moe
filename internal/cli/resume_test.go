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
// state without typing through promptNextStage. Reuses runOneShotStage
// directly (the same call runOneShotChain makes), so the test fixture
// reflects production semantics.
func runOneShotStageDirect(t *testing.T, projectID, runID, docID string, needsSandbox bool) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runOneShotStage(projectID, runID, docID, needsSandbox, &out, &errb); code != 0 {
		t.Fatalf("runOneShotStage %s: exit=%d stderr=%q", docID, code, errb.String())
	}
}

func TestSdlcResumeFromDesign(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeOneShotClaude(t, "", 0, "fake claude resume")

	// Open a fresh sdlc run with no design canvas yet.
	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Resume from design"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"--one-shot", "tele", "resume-from-design"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	for _, doc := range []string{"design", "code"} {
		body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-design", "documents", doc, "content.md"))
		if err != nil {
			t.Fatalf("%s canvas missing: %v", doc, err)
		}
		if !strings.Contains(string(body), "fake claude resume") {
			t.Fatalf("%s canvas missing fake-claude marker: %q", doc, body)
		}
	}
	if !strings.Contains(out.String(), "next: moe sdlc push tele resume-from-design") {
		t.Fatalf("expected push hint at end, got: %q", out.String())
	}
}

func TestSdlcResumeFromCode(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeOneShotClaude(t, "", 0, "fake claude resume")

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Resume from code"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	// Land design first; resume should not re-run it.
	runOneShotStageDirect(t, "tele", "resume-from-code", "design", false)

	designBefore, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-code", "documents", "design", "content.md"))
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"--one-shot", "tele", "resume-from-code"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	// Code canvas now exists.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "resume-from-code", "documents", "code", "content.md")); err != nil {
		t.Fatalf("code canvas should exist: %v", err)
	}
	// Design canvas was not re-run (no second append).
	designAfter, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-code", "documents", "design", "content.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(designBefore, designAfter) {
		t.Fatalf("design canvas changed during code-resume:\nbefore: %q\nafter:  %q", designBefore, designAfter)
	}
	// Banner should mention only code, not design.
	if !strings.Contains(out.String(), "one-shot: code") {
		t.Fatalf("expected one-shot: code banner, got: %q", out.String())
	}
	if strings.Contains(out.String(), "design → code") {
		t.Fatalf("did not expect design→code banner on resume-from-code: %q", out.String())
	}
}

func TestSdlcResumeFromPush(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeOneShotClaude(t, "", 0, "fake claude resume")

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Resume from push"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	runOneShotStageDirect(t, "tele", "resume-from-push", "design", false)
	runOneShotStageDirect(t, "tele", "resume-from-push", "code", true)

	designSnap, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-push", "documents", "design", "content.md"))
	if err != nil {
		t.Fatal(err)
	}
	codeSnap, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-push", "documents", "code", "content.md"))
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"--one-shot", "tele", "resume-from-push"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	// No "one-shot:" banner — both stages were already done.
	if strings.Contains(out.String(), "one-shot:") {
		t.Fatalf("did not expect one-shot banner on resume-from-push: %q", out.String())
	}
	// Push hint surfaces.
	if !strings.Contains(out.String(), "next: moe sdlc push tele resume-from-push") {
		t.Fatalf("expected push hint, got: %q", out.String())
	}
	// Neither canvas was touched.
	for _, want := range []struct {
		path string
		want []byte
	}{
		{filepath.Join(root, "projects", "tele", "runs", "resume-from-push", "documents", "design", "content.md"), designSnap},
		{filepath.Join(root, "projects", "tele", "runs", "resume-from-push", "documents", "code", "content.md"), codeSnap},
	} {
		got, err := os.ReadFile(want.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want.want) {
			t.Fatalf("%s mutated unexpectedly", want.path)
		}
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
	code := runResume([]string{"--one-shot", "tele", "will-be-merged"}, &out, &errb)
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
	code := runResume([]string{"--one-shot", "tele", "no-such-run"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal on missing run; stdout=%q", out.String())
	}
}

// TestSdlcResumeInteractiveInvokesNextStage: without --one-shot,
// resume should invoke the next pending stage's interactive Run.
// Stub the stage's Run function so the test doesn't try to open a
// real Claude session.
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

	// Swap the design stage's Run for a recorder. workflow.commands[]
	// is package-private state, so we mutate it directly and restore
	// in cleanup.
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	original := wf.commands["design"]
	t.Cleanup(func() { wf.commands["design"] = original })

	var gotArgs []string
	wf.commands["design"] = &Command{
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
