package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// stubEditor points EDITOR at `true` — a no-op that exits 0 — so
// launchEditor spawns something real but non-interactive. Satisfies
// the editor-available gate without dropping the test into vi.
func stubEditor(t *testing.T) {
	t.Helper()
	t.Setenv("EDITOR", "true")
	t.Setenv("VISUAL", "")
}

// noEditor clears both EDITOR and VISUAL so the editor-available gate
// trips. Used by the handful of tests that explicitly exercise the
// "no editor configured" failure path.
func noEditor(t *testing.T) {
	t.Helper()
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "")
}

// ideaCanvas is the canvas path for an idea run.
func ideaCanvas(root, projectID, slug string) string {
	return filepath.Join(root, "projects", projectID, "runs", slug, "documents", "idea", "content.md")
}

func TestIdeaRegistered(t *testing.T) {
	cmd, ok := commands["idea"]
	if !ok {
		t.Fatal(`expected top-level command "idea" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("idea command summary should not be empty")
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "edit", "close", "list", "cat", "move"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("idea usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestBuildIdeaChatPromptSectionsEndWithNewline pins the same
// trailing-newline contract as TestBuildSystemPromptSectionsEndWithNewline,
// but for buildIdeaChatPrompt's three-section join (soul, idea stage
// fragment, inline operational core). The idea builder is the odd
// one of the five — no *wiki.Config and an inline core literal — so
// a regression in the literal would silently drop the trailing
// newline; this test is the tripwire.
func TestBuildIdeaChatPromptSectionsEndWithNewline(t *testing.T) {
	got := buildIdeaChatPrompt("/tmp/canvas.md", "capture")
	assertPromptSectionsEndWithNewline(t, got, 3)
}

func TestIdeaNewCreatesRunAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "tele", "Faster dash load"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "captured idea tele faster-dash-load") {
		t.Fatalf("missing capture confirmation: %q", out.String())
	}

	// Canvas lands at the run's documents/idea path.
	body, err := os.ReadFile(ideaCanvas(root, "tele", "faster-dash-load"))
	if err != nil {
		t.Fatalf("idea canvas not written: %v", err)
	}
	if string(body) != "# Faster dash load\n" {
		t.Fatalf("unexpected canvas body: %q", body)
	}

	// The open-run commit is HEAD with the expected subject + trailers.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Open run tele faster-dash-load: Faster dash load") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{"MoE-Run: faster-dash-load", "MoE-Project: tele"} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	// run.json should exist and name the idea workflow.
	mdBody, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "faster-dash-load", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(mdBody), `"workflow": "idea"`) {
		t.Fatalf("run.json does not name idea workflow:\n%s", mdBody)
	}
}

func TestIdeaNewCommitsEditorEdits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Fake editor: append a line so we can tell the capture commit
	// reflects the post-edit file, not the stub.
	script := filepath.Join(t.TempDir(), "fake-editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'added by editor\\n' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "new", "tele", "With body"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	body, err := os.ReadFile(ideaCanvas(root, "tele", "with-body"))
	if err != nil {
		t.Fatalf("idea canvas not written: %v", err)
	}
	if !strings.Contains(string(body), "added by editor") {
		t.Fatalf("editor edit not on disk: %q", body)
	}

	// The edit rides along on the open commit — single commit, clean tree.
	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("working tree should be clean after capture, got:\n%v", entries)
	}
	shown := gittest.Output(t, root, "show", "HEAD:projects/tele/runs/with-body/documents/idea/content.md")
	if !strings.Contains(shown, "added by editor") {
		t.Fatalf("HEAD version missing editor edit:\n%s", shown)
	}
}

func TestIdeaNewAutoSuffixesOnCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	for _, want := range []string{"tele foo", "tele foo-2", "tele foo-3"} {
		var out, errb bytes.Buffer
		code := Run([]string{"idea", "new", "tele", "foo"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
		if !strings.Contains(out.String(), "captured idea "+want) {
			t.Fatalf("expected capture of %s, got: %q", want, out.String())
		}
	}
}

func TestIdeaNewIDOverrideErrorsOnCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "--id=mine", "tele", "first"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first new failed: code=%d", code)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "--id=mine", "tele", "second"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on explicit-id collision, got 0; stderr=%q", errb.String())
	}
	// run.New formats the collision error with "already used in project".
	if !strings.Contains(errb.String(), "already used") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
}

// Regression: --id placed after the project positional should still
// be honored; reorderFlags hoists it to the front.
func TestIdeaNewTolerantToFlagAfterPositional(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "tele", "something", "--id=custom-slug"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "captured idea tele custom-slug") {
		t.Fatalf("expected slug from --id override, got: %q", out.String())
	}
	if _, err := os.Stat(ideaCanvas(root, "tele", "custom-slug")); err != nil {
		t.Fatalf("idea canvas should exist under override slug: %v", err)
	}
}

// The editor gate: without $EDITOR or $VISUAL, idea new must refuse
// up front and leave the tree untouched.
func TestIdeaNewRequiresEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "tele", "needs an editor"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit with no editor set, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "EDITOR") || !strings.Contains(errb.String(), "VISUAL") {
		t.Fatalf("expected error naming $EDITOR/$VISUAL, got: %q", errb.String())
	}
	// No run dir should have been written.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "needs-an-editor")); !os.IsNotExist(err) {
		t.Fatalf("run dir should not exist on editor-gate failure, stat err=%v", err)
	}
	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tree should be clean after editor-gate failure, got:\n%v", entries)
	}
}

func TestIdeaNewRefusesUnregisteredProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "ghost", "anything"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing project, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Fatalf("expected unregistered-project error, got: %q", errb.String())
	}
}

func TestIdeaNewRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	// Drop a stray untracked file so the dirty-tree gate fires.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "tele", "x"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
}

func TestIdeaListPrintsSlugsAndTitles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	for _, title := range []string{"Cross-project search", "Faster dash load", "Zzz last"} {
		if code := Run([]string{"idea", "new", "tele", title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("setup capture failed for %q", title)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "list", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	wantOrder := []string{"cross-project-search", "faster-dash-load", "zzz-last"}
	last := -1
	for _, slug := range wantOrder {
		idx := strings.Index(got, slug)
		if idx < 0 {
			t.Fatalf("output missing slug %q:\n%s", slug, got)
		}
		if idx <= last {
			t.Fatalf("output not slug-sorted (looking for %q after offset %d):\n%s", slug, last, got)
		}
		last = idx
	}
	if !strings.Contains(got, "cross-project-search\tCross-project search") {
		t.Fatalf("expected slug<TAB>title format, got:\n%s", got)
	}
}

func TestIdeaListHidesClosedAndPromoted(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Three ideas, then close one and promote another.
	for _, title := range []string{"still open", "will be closed", "will be promoted"} {
		if code := Run([]string{"idea", "new", "tele", title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("setup capture failed for %q", title)
		}
	}
	if code := Run([]string{"idea", "close", "tele", "will-be-closed"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("close failed")
	}
	if code := runNew("sdlc", []string{"--from-idea=will-be-promoted", "tele"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("promote failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "list", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "still-open") {
		t.Fatalf("open idea missing from list:\n%s", got)
	}
	if strings.Contains(got, "will-be-closed") {
		t.Fatalf("closed idea should not be listed:\n%s", got)
	}
	if strings.Contains(got, "will-be-promoted") {
		t.Fatalf("promoted idea should not be listed:\n%s", got)
	}
}

func TestIdeaListEmptyProjectIsZero(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "list", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "" {
		t.Fatalf("expected empty stdout for project with no ideas, got: %q", out.String())
	}
}

// Editor edits to an existing idea land as a single `work: update idea`
// commit. Trailers include MoE-Run, MoE-Project, MoE-Workflow, MoE-Document.
func TestIdeaEditCommitsEditorEdits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Starter"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	// Fake editor: rewrite the canvas.
	script := filepath.Join(t.TempDir(), "refine-editor.sh")
	body := "#!/bin/sh\ncat > \"$1\" <<BODY\n# Starter refined\n\n- a bullet\nBODY\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "starter"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "refined idea tele starter") {
		t.Fatalf("missing refine confirmation: %q", out.String())
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "work: update idea") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: starter",
		"MoE-Project: tele",
		"MoE-Workflow: idea",
		"MoE-Document: idea",
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
		t.Fatalf("tree should be clean after refine, got:\n%v", entries)
	}
}

// A no-op save must not produce an empty `work: update idea` commit.
func TestIdeaEditNoChangeDoesNotCommit(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Leave it"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "leave-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Fatalf("expected 'unchanged' message on no-op, got: %q", out.String())
	}

	afterHead := gitLog(t, root, "-1", "--format=%H")
	if beforeHead != afterHead {
		t.Fatalf("no-op edit created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
	}
}

func TestIdeaEditMissingSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("expected missing-idea error, got: %q", errb.String())
	}
}

func TestIdeaEditRequiresEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	stubEditor(t)
	if code := Run([]string{"idea", "new", "tele", "Ed gate"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	noEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "ed-gate"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero with no editor, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "EDITOR") || !strings.Contains(errb.String(), "VISUAL") {
		t.Fatalf("expected error naming $EDITOR/$VISUAL, got: %q", errb.String())
	}
}

func TestIdeaEditRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Busy"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "busy"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
}

func TestIdeaEditRefusesNonIdeaRun(t *testing.T) {
	// Guard that `moe idea edit` doesn't operate on an sdlc/kb
	// run even if the slug matches — the workflow check is load-bearing
	// since ideas and other runs share one slug namespace per project.
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele", "Real run"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup run failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "tele", "real-run"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero editing a non-idea run, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not an idea") {
		t.Fatalf("expected wrong-workflow error, got: %q", errb.String())
	}
}

func TestIdeaCloseBumpsStatusAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Close me"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele", "close-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed idea tele close-me") {
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
	if !strings.Contains(head, "Close idea tele close-me") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: close-me",
		"MoE-Project: tele",
		"MoE-Workflow: idea",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

func TestIdeaCloseMissingSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("expected missing-idea error, got: %q", errb.String())
	}
}

func TestIdeaCloseRejectsAlreadyClosed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "One shot"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if code := Run([]string{"idea", "close", "tele", "one-shot"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first close failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele", "one-shot"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on already-closed idea, got 0")
	}
	if !strings.Contains(errb.String(), "already") {
		t.Fatalf("expected already-closed error, got: %q", errb.String())
	}
}

func TestIdeaCloseUsageErrorsOnMissingArgs(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on missing slug, got %d; stderr=%q", code, errb.String())
	}
}

// TestIdeaCatPrintsCanvas: dump an idea's canvas verbatim to stdout.
func TestIdeaCatPrintsCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Read me back"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "cat", "tele", "read-me-back"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# Read me back\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %q", errb.String())
	}
}

// TestIdeaCatUnknownSlug: missing slug exits 1, names the missing slug.
func TestIdeaCatUnknownSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "cat", "tele", "ghost"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on missing slug, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such run: tele/ghost") {
		t.Fatalf("expected missing-run error, got: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got: %q", out.String())
	}
}

// TestIdeaCatRefusesNonIdeaRun: pointing cat at a non-idea run errors
// loud — same loadIdeaRun guard idea edit relies on.
func TestIdeaCatRefusesNonIdeaRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele", "Real run"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup run failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "cat", "tele", "real-run"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on wrong-workflow slug, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "real-run is a sdlc run, use 'moe sdlc cat'") {
		t.Fatalf("expected wrong-workflow error pointing at sdlc, got: %q", errb.String())
	}
}

// TestIdeaCatStatusAgnostic: cat works on closed ideas too — recall is
// useful precisely after an idea has moved on.
func TestIdeaCatStatusAgnostic(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Closed but cat-able"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if code := Run([]string{"idea", "close", "tele", "closed-but-cat-able"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup close failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "cat", "tele", "closed-but-cat-able"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "Closed but cat-able") {
		t.Fatalf("expected canvas body in stdout, got: %q", out.String())
	}
}

// TestIdeaCatUsageErrors: wrong arity exits 2.
func TestIdeaCatUsageErrors(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "cat", "tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on missing slug, got %d; stderr=%q", code, errb.String())
	}
}

// buildIdeaChatPrompt should include soul.md, the stages/idea/<mode>.md
// fragment, and the operational core naming the canvas file.
func TestBuildIdeaChatPromptHasAllSections(t *testing.T) {
	got := buildIdeaChatPrompt("/tmp/ideas/foo.md", "capture")
	for _, want := range []string{
		"/tmp/ideas/foo.md",
		"Stage: idea capture",
		"pre-design shelf",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("capture prompt missing %q:\n%s", want, got)
		}
	}
	got = buildIdeaChatPrompt("/tmp/ideas/bar.md", "refine")
	if !strings.Contains(got, "Stage: idea refine") {
		t.Fatalf("refine prompt missing refine fragment:\n%s", got)
	}
}

// --chat bypasses the $EDITOR gate. The fake claude writes to the path
// passed in on its command line (the tempfile canvas, before run.New
// moves the body into place).
func TestIdeaNewChatSkipsEditorGate(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	// The kickoff passes the canvas path as the last arg. The fake
	// pulls it out of --append-system-prompt instead: the prompt
	// contains a line `  <abs path>` right under "Your canvas is the
	// single file:". Extracting it this way keeps the fake independent
	// of where os.MkdirTemp actually lands the tempfile.
	fakeClaudeOnPath(t, `#!/bin/sh
next_is_prompt=0
prompt=
for a in "$@"; do
  if [ "$next_is_prompt" = "1" ]; then
    prompt=$a
    next_is_prompt=0
  fi
  case "$a" in
    --append-system-prompt) next_is_prompt=1 ;;
  esac
done
# Grab the line after "Your canvas is the single file:" and trim it.
f=$(printf '%s' "$prompt" | awk '/Your canvas is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$f" ] && [ -f "$f" ]; then
  printf 'written by fake claude\n' >> "$f"
fi
exit 0
`)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "--chat", "tele", "Chat capture"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "captured idea tele chat-capture") {
		t.Fatalf("missing capture confirmation: stdout=%q stderr=%q", out.String(), errb.String())
	}
	body, err := os.ReadFile(ideaCanvas(root, "tele", "chat-capture"))
	if err != nil {
		t.Fatalf("idea canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "written by fake claude") {
		t.Fatalf("expected fake-claude edit on disk: %q", body)
	}
}

func TestIdeaEditChatSkipsEditorGate(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	stubEditor(t)
	if code := Run([]string{"idea", "new", "tele", "Chat refine"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	noEditor(t)

	// On edit the canvas is at its final path, so glob under runs/.
	fakeClaudeOnPath(t, `#!/bin/sh
for f in "$PWD"/projects/*/runs/*/documents/idea/content.md; do
  [ -f "$f" ] && printf '\nrefined by fake claude\n' >> "$f"
done
exit 0
`)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "edit", "--chat", "tele", "chat-refine"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "refined idea tele chat-refine") {
		t.Fatalf("missing refine confirmation: %q", out.String())
	}
	body, err := os.ReadFile(ideaCanvas(root, "tele", "chat-refine"))
	if err != nil {
		t.Fatalf("idea canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "refined by fake claude") {
		t.Fatalf("expected fake-claude edit on disk: %q", body)
	}
}

// fakeClaudeOnPath drops an executable named `claude` into a tempdir
// and prepends that dir to $PATH so exec.LookPath("claude") finds it.
// Script contents are the argument to the shebang — the caller writes
// the shell body.
func fakeClaudeOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// gitLog runs `git -C root log <args>` and returns its stdout.
func gitLog(t *testing.T, root string, args ...string) string {
	t.Helper()
	return gittest.Output(t, root, append([]string{"log"}, args...)...)
}

// TestIdeaMoveRehomesRunAndCommits is the happy path: capture an idea
// in project A, move it to project B, and assert the on-disk run dir
// relocated, run.json's project field rewrote, the source dir is gone,
// and HEAD carries the move subject + canonical trailers including
// MoE-Idea-Moved-From.
func TestIdeaMoveRehomesRunAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Belongs to moe"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "belongs-to-moe", "moe"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "moved idea tele/belongs-to-moe to moe/belongs-to-moe") {
		t.Fatalf("missing move confirmation: %q", out.String())
	}

	// Source dir gone, destination dir holds the canvas.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "belongs-to-moe")); !os.IsNotExist(err) {
		t.Fatalf("source dir should be gone, stat err=%v", err)
	}
	body, err := os.ReadFile(ideaCanvas(root, "moe", "belongs-to-moe"))
	if err != nil {
		t.Fatalf("destination canvas missing: %v", err)
	}
	if string(body) != "# Belongs to moe\n" {
		t.Fatalf("canvas body changed by move: %q", body)
	}

	// run.json under the destination names the new project.
	mdBody, err := os.ReadFile(filepath.Join(root, "projects", "moe", "runs", "belongs-to-moe", "run.json"))
	if err != nil {
		t.Fatalf("destination run.json missing: %v", err)
	}
	if !strings.Contains(string(mdBody), `"project": "moe"`) {
		t.Fatalf("run.json project not rewritten:\n%s", mdBody)
	}
	if !strings.Contains(string(mdBody), `"status": "in_progress"`) {
		t.Fatalf("run.json status should stay in_progress:\n%s", mdBody)
	}

	// HEAD subject + trailers.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Move idea tele belongs-to-moe to moe") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: belongs-to-moe",
		"MoE-Project: moe",
		"MoE-Workflow: idea",
		"MoE-Idea-Moved-From: tele/belongs-to-moe",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	// Clean tree afterwards.
	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tree should be clean after move, got:\n%v", entries)
	}
}

// TestIdeaMoveRefusesUnknownDestProject: dest project must be registered.
func TestIdeaMoveRefusesUnknownDestProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Stuck here"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "stuck-here", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing dest project, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Fatalf("expected unregistered-project error, got: %q", errb.String())
	}
	// Source untouched.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "stuck-here")); err != nil {
		t.Fatalf("source dir should be untouched on refusal: %v", err)
	}
}

// TestIdeaMoveRefusesSameProject: src == dest is a no-op, refused.
func TestIdeaMoveRefusesSameProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Stays put"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "stays-put", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on same-project move, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "same") {
		t.Fatalf("expected same-project error, got: %q", errb.String())
	}
	// No commit created.
	afterHead := gitLog(t, root, "-1", "--format=%H")
	if beforeHead != afterHead {
		t.Fatalf("same-project move should not commit:\nbefore=%safter=%s", beforeHead, afterHead)
	}
}

// TestIdeaMoveRefusesSlugCollision: a run already at the destination
// slug forces a refusal so the operator picks (close or rename) instead
// of having two runs silently fight for the path.
func TestIdeaMoveRefusesSlugCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Twin"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture A failed")
	}
	if code := Run([]string{"idea", "new", "moe", "Twin"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture B failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "twin", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on slug collision, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
	// Both run dirs intact.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "twin")); err != nil {
		t.Fatalf("source dir should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "moe", "runs", "twin")); err != nil {
		t.Fatalf("dest dir should still exist: %v", err)
	}
}

// TestIdeaMoveRefusesClosedIdea: closed status is terminal — moving it
// would update its home without changing what it represents, so refuse.
func TestIdeaMoveRefusesClosedIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Done"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if code := Run([]string{"idea", "close", "tele", "done"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup close failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "done", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on closed idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not open") {
		t.Fatalf("expected wrong-status error, got: %q", errb.String())
	}
}

// TestIdeaMoveRefusesPromotedIdea: promoted ideas carry a provenance
// pointer on their downstream sdlc run; moving the source after promote
// would silently invalidate that link, so refuse.
func TestIdeaMoveRefusesPromotedIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := Run([]string{"idea", "new", "tele", "Promote me"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if code := runNew("sdlc", []string{"--from-idea=promote-me", "tele"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup promote failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "promote-me", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on promoted idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not open") {
		t.Fatalf("expected wrong-status error, got: %q", errb.String())
	}
}

// TestIdeaMoveRefusesNonIdeaRun: a slug that names a non-idea run
// (sdlc, kb, …) is not an idea move target, even when the slug shape
// matches. Guard the workflow check the same way idea edit does.
func TestIdeaMoveRefusesNonIdeaRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele", "Real run"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup sdlc run failed")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "real-run", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on non-idea run, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not an idea") {
		t.Fatalf("expected wrong-workflow error, got: %q", errb.String())
	}
}

// TestIdeaMoveMissingSourceSlug: source slug must resolve to an idea
// run that exists.
func TestIdeaMoveMissingSourceSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "ghost", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing source, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("expected missing-idea error, got: %q", errb.String())
	}
}

// TestIdeaMoveRefusesDirtyWorkingTree: a stray edit would ride along on
// the move commit. The clean-tree gate must trip before any move work.
func TestIdeaMoveRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Dirty"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "dirty", "moe"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
}

// TestIdeaMoveUsageErrors: wrong arity exits 2.
func TestIdeaMoveUsageErrors(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "move", "tele", "slug"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on missing args, got %d; stderr=%q", code, errb.String())
	}
}
