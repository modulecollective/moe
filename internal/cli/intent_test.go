package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// intentCanvas is the canvas path for an intent run.
func intentCanvas(root, projectID, slug string) string {
	return filepath.Join(root, "projects", projectID, "runs", slug, "documents", "intent", "content.md")
}

func TestIntentRegistered(t *testing.T) {
	cmd, ok := commands["intent"]
	if !ok {
		t.Fatal(`expected top-level command "intent" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("intent command summary should not be empty")
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "edit", "close", "list", "cat"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("intent usage missing subcommand %q: %q", want, out.String())
		}
	}
	// Intent is deliberately narrower than idea: no move/reopen/log in v1.
	for _, deny := range []string{"move", "reopen", "log"} {
		if strings.Contains(out.String(), deny) {
			t.Fatalf("intent usage should not carry %q in v1: %q", deny, out.String())
		}
	}
}

func TestIntentNewCreatesRunAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "tele/faster-everything"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "parked intent tele/faster-everything") {
		t.Fatalf("missing park confirmation: %q", out.String())
	}

	body, err := os.ReadFile(intentCanvas(root, "tele", "faster-everything"))
	if err != nil {
		t.Fatalf("intent canvas not written: %v", err)
	}
	if string(body) != "# faster-everything\n" {
		t.Fatalf("unexpected canvas body: %q", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Open run tele/faster-everything") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{"MoE-Run: faster-everything", "MoE-Project: tele"} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	mdBody, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "faster-everything", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(mdBody), `"workflow": "intent"`) {
		t.Fatalf("run.json does not name intent workflow:\n%s", mdBody)
	}
}

func TestIntentNewSlugCollisionFailsLoud(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/north-star"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first new failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "tele/north-star"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on slug collision, got 0; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "already used") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
	if !strings.Contains(errb.String(), `"north-star-2"`) {
		t.Fatalf("expected suggested free slug, got: %q", errb.String())
	}
}

func TestIntentNewRejectsNonCanonicalSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "tele/Big Theme"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
}

func TestIntentNewRequiresEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "tele/needs-an-editor"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit with no editor set, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "EDITOR") || !strings.Contains(errb.String(), "VISUAL") {
		t.Fatalf("expected error naming $EDITOR/$VISUAL, got: %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "needs-an-editor")); !os.IsNotExist(err) {
		t.Fatalf("run dir should not exist on editor-gate failure, stat err=%v", err)
	}
}

func TestIntentNewRefusesUnregisteredProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "ghost/anything"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing project, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Fatalf("expected unregistered-project error, got: %q", errb.String())
	}
}

func TestIntentNewRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "new", "tele/x"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
}

func TestIntentListPrintsOpenSlugsSorted(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	for _, slug := range []string{"a-theme", "m-theme", "z-theme"} {
		if code := Run([]string{"intent", "new", "tele/" + slug}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("setup park failed for %q", slug)
		}
	}
	// Close one — closed intents must drop out of the list.
	if code := Run([]string{"intent", "close", "tele/m-theme"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup close failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "list", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, "m-theme") {
		t.Fatalf("closed intent should not be listed:\n%s", got)
	}
	aIdx, zIdx := strings.Index(got, "a-theme"), strings.Index(got, "z-theme")
	if aIdx < 0 || zIdx < 0 || aIdx > zIdx {
		t.Fatalf("expected a-theme before z-theme, both present:\n%s", got)
	}
}

func TestIntentEditCommitsEditorEdits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/starter"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup park failed")
	}

	script := filepath.Join(t.TempDir(), "refine-editor.sh")
	body := "#!/bin/sh\ncat > \"$1\" <<BODY\n# Starter sharpened\n\n- a bullet\nBODY\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "edit", "tele/starter"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sharpened intent tele/starter") {
		t.Fatalf("missing sharpen confirmation: %q", out.String())
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "work: update intent") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: starter",
		"MoE-Project: tele",
		"MoE-Workflow: intent",
		"MoE-Document: intent",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tree should be clean after sharpen, got:\n%v", entries)
	}
}

func TestIntentEditRefusesNonIntentRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele/real-run"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup run failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "edit", "tele/real-run"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero editing a non-intent run, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not an intent") {
		t.Fatalf("expected wrong-workflow error, got: %q", errb.String())
	}
}

func TestIntentCloseBumpsStatusAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/close-me"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup park failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "close", "tele/close-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed intent tele/close-me") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	mdBody, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "close-me", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing after close: %v", err)
	}
	if !strings.Contains(string(mdBody), `"status": "closed"`) {
		t.Fatalf("run.json status not closed:\n%s", mdBody)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close intent tele/close-me") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{"MoE-Run: close-me", "MoE-Project: tele", "MoE-Workflow: intent"} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

// TestIntentCloseExemptFromCanvasSeal: a capture-workflow close must not
// trip the non-empty-canvas seal — an empty intent on close is operator
// intent, not a missed write. Emptying the canvas and committing it, then
// closing, must succeed.
func TestIntentCloseExemptFromCanvasSeal(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/emptied"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup park failed")
	}
	// Truncate the canvas to zero bytes and commit it clean, so the close
	// gate sees an empty canvas on disk.
	canvas := intentCanvas(root, "tele", "emptied")
	if err := os.WriteFile(canvas, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(root, "commit", "-am", "empty the canvas"); err != nil {
		t.Fatalf("commit truncation: %v", err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"intent", "close", "tele/emptied"}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected close to succeed on empty capture canvas, got %d stderr=%q", code, errb.String())
	}
}

func TestIntentCloseRejectsAlreadyClosed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/one-shot"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup park failed")
	}
	if code := Run([]string{"intent", "close", "tele/one-shot"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first close failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "close", "tele/one-shot"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on already-closed intent, got 0")
	}
	if !strings.Contains(errb.String(), "already") {
		t.Fatalf("expected already-closed error, got: %q", errb.String())
	}
}

func TestIntentCatPrintsCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"intent", "new", "tele/read-me-back"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup park failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "cat", "tele/read-me-back"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# read-me-back\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
}

func TestIntentCatRefusesNonIntentRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele/real-run"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup run failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"intent", "cat", "tele/real-run"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on wrong-workflow slug, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "real-run is a sdlc run, use 'moe sdlc cat'") {
		t.Fatalf("expected wrong-workflow error pointing at sdlc, got: %q", errb.String())
	}
}

func TestIsCaptureWorkflow(t *testing.T) {
	for _, wf := range []string{"idea", "intent"} {
		if !isCaptureWorkflow(wf) {
			t.Errorf("expected %q to be a capture workflow", wf)
		}
	}
	for _, wf := range []string{"sdlc", "kb", "twin", "chat", "pulse", "hooks", "chores"} {
		if isCaptureWorkflow(wf) {
			t.Errorf("expected %q not to be a capture workflow", wf)
		}
	}
}
