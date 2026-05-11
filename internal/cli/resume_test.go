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

	// Design ran headlessly; the chain prompt-per-stage stops here so
	// the operator can spot-check before code runs.
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-design", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("design canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "fake claude resume") {
		t.Fatalf("design canvas missing fake-claude marker: %q", body)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "resume-from-design", "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist after one-shot design — chain stops at prompt: err=%v", err)
	}
	if !strings.Contains(out.String(), "next: moe sdlc code tele resume-from-design") {
		t.Fatalf("expected post-design code hint, got: %q", out.String())
	}
}

// TestSdlcResumeRerunsParkedDesign: under the forward-walking rule a
// committed design turn with no later code turn parks the run at
// design. resume re-enters design (re-running it on the parked
// canvas) and then stops at the chain prompt for code — same shape
// as TestSdlcResumeFromDesign but starting from a populated canvas
// rather than a blank one.
func TestSdlcResumeRerunsParkedDesign(t *testing.T) {
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
	// Land design first; the run is now parked at design (no code
	// turn yet, so design's only successor has no later turn).
	runOneShotStageDirect(t, "tele", "resume-from-code", "design", false)

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"--one-shot", "tele", "resume-from-code"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	// Banner names design only — chain runs the first applicable
	// stage and stops at the prompt before code.
	if !strings.Contains(out.String(), "one-shot: design (headless)") {
		t.Fatalf("expected one-shot: design banner, got: %q", out.String())
	}
	if strings.Contains(out.String(), "design → code") {
		t.Fatalf("did not expect design→code banner — chain runs one stage at a time now: %q", out.String())
	}
	// Code dir absent — chain stopped at the prompt before code ran.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "resume-from-code", "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist on chain-prompt stop: err=%v", err)
	}
	// Code hint surfaces via the chain prompt's non-tty fallback.
	if !strings.Contains(out.String(), "next: moe sdlc code tele resume-from-code") {
		t.Fatalf("expected code hint, got: %q", out.String())
	}
}

// TestSdlcResumeRerunsParkedCode: design + code committed, no push
// yet. Push has no work-turn commit shape, so under the
// forward-walking rule code stays parked until the run hits a
// terminal status. resume re-enters code and surfaces the push hint
// at the end via the successor lookup. Design is not re-run because
// it satisfies — code's later turn is the successor it needs.
func TestSdlcResumeRerunsParkedCode(t *testing.T) {
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

	out.Reset()
	errb.Reset()
	if code := runResume([]string{"--one-shot", "tele", "resume-from-push"}, &out, &errb); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	// Banner names code only — design satisfies (its successor's turn
	// is later), so the chain skips it.
	if !strings.Contains(out.String(), "one-shot: code") {
		t.Fatalf("expected one-shot: code banner, got: %q", out.String())
	}
	if strings.Contains(out.String(), "design → code") {
		t.Fatalf("did not expect design→code banner on parked-code resume: %q", out.String())
	}
	// Push hint surfaces at the end.
	if !strings.Contains(out.String(), "next: moe sdlc push tele resume-from-push") {
		t.Fatalf("expected push hint, got: %q", out.String())
	}
	// Design canvas was not touched — its successor is satisfied.
	designAfter, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "resume-from-push", "documents", "design", "content.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(designAfter, designSnap) {
		t.Fatalf("design canvas mutated unexpectedly:\nbefore: %q\nafter:  %q", designSnap, designAfter)
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
