package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedProject writes a minimal project.json so the project-registered
// check in idea/runNew passes. Commits everything currently pending
// (including the bureaucracy.conf marker laid down by markBureaucracy)
// so the tree is clean for commands that refuse to run on a dirty
// working tree.
func seedProject(t *testing.T, root, projectID string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "-C", root, "add", "-A")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commit := exec.Command("git", "-C", root, "commit", "-m", "register project "+projectID)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

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
	for _, want := range []string{"add", "remove", "list"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("idea usage missing subcommand %q: %q", want, out.String())
		}
	}
}

func TestIdeaAddCreatesFileAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "tele", "Faster dash load"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "captured idea tele/faster-dash-load") {
		t.Fatalf("missing capture confirmation: %q", out.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "ideas", "faster-dash-load.md"))
	if err != nil {
		t.Fatalf("idea file not written: %v", err)
	}
	if string(body) != "# Faster dash load\n" {
		t.Fatalf("unexpected stub body: %q", body)
	}

	// The capture commit should be the new HEAD with the right subject + trailers.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Capture idea tele/faster-dash-load: Faster dash load") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{"MoE-Idea: faster-dash-load", "MoE-Project: tele"} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

func TestIdeaAddCommitsEditorEdits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
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
	if code := Run([]string{"idea", "add", "tele", "With body"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "ideas", "with-body.md"))
	if err != nil {
		t.Fatalf("idea file not written: %v", err)
	}
	if !strings.Contains(string(body), "added by editor") {
		t.Fatalf("editor edit not on disk: %q", body)
	}

	// The edit must be in the commit, not left as an uncommitted change.
	status := exec.Command("git", "-C", root, "status", "--porcelain")
	st, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, st)
	}
	if len(bytes.TrimSpace(st)) != 0 {
		t.Fatalf("working tree should be clean after capture, got:\n%s", st)
	}
	show := exec.Command("git", "-C", root, "show", "HEAD:projects/tele/ideas/with-body.md")
	shown, err := show.CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v\n%s", err, shown)
	}
	if !strings.Contains(string(shown), "added by editor") {
		t.Fatalf("HEAD version missing editor edit:\n%s", shown)
	}
}

func TestIdeaAddAutoSuffixesOnCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	for _, want := range []string{"tele/foo", "tele/foo-2", "tele/foo-3"} {
		var out, errb bytes.Buffer
		code := Run([]string{"idea", "add", "tele", "foo"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
		if !strings.Contains(out.String(), "captured idea "+want) {
			t.Fatalf("expected capture of %s, got: %q", want, out.String())
		}
	}
}

func TestIdeaAddIDOverrideErrorsOnCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "add", "--id=mine", "tele", "first"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first new failed: code=%d", code)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "--id=mine", "tele", "second"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on explicit-id collision, got 0; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
}

// Regression: --id placed after the project positional should still
// be honored; reorderFlags hoists it to the front.
func TestIdeaAddTolerantToFlagAfterPositional(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "tele", "something", "--id=custom-slug"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "captured idea tele/custom-slug") {
		t.Fatalf("expected slug from --id override, got: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "ideas", "custom-slug.md")); err != nil {
		t.Fatalf("idea file should exist under override slug: %v", err)
	}
}

// The editor gate: without $EDITOR or $VISUAL, idea add must refuse
// up front and leave the tree untouched. The previous behavior wrote
// the stub, printed a hint, and committed the title-only file anyway.
func TestIdeaAddRequiresEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "tele", "needs an editor"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit with no editor set, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "EDITOR") || !strings.Contains(errb.String(), "VISUAL") {
		t.Fatalf("expected error naming $EDITOR/$VISUAL, got: %q", errb.String())
	}
	// No file should have been written.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "ideas", "needs-an-editor.md")); !os.IsNotExist(err) {
		t.Fatalf("idea file should not exist on editor-gate failure, stat err=%v", err)
	}
	// Tree must still be clean (no orphan commit, no untracked stub).
	status := exec.Command("git", "-C", root, "status", "--porcelain")
	st, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, st)
	}
	if len(bytes.TrimSpace(st)) != 0 {
		t.Fatalf("tree should be clean after editor-gate failure, got:\n%s", st)
	}
}

func TestIdeaAddRefusesUnregisteredProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "ghost", "anything"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing project, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Fatalf("expected unregistered-project error, got: %q", errb.String())
	}
}

func TestIdeaAddRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	// Drop a stray untracked file so `git status --porcelain` reports it.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "add", "tele", "x"}, &out, &errb)
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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	for _, title := range []string{"Cross-project search", "Faster dash load", "Zzz last"} {
		if code := Run([]string{"idea", "add", "tele", title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
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

func TestIdeaListEmptyProjectIsZero(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
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

func TestIdeaRemoveDeletesFileAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "add", "tele", "Faster dash load"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "remove", "tele", "faster-dash-load"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "removed idea tele/faster-dash-load") {
		t.Fatalf("missing removal confirmation: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "ideas", "faster-dash-load.md")); !os.IsNotExist(err) {
		t.Fatalf("idea file should be gone, stat err=%v", err)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Remove idea tele/faster-dash-load: Faster dash load") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{"MoE-Idea: faster-dash-load", "MoE-Project: tele"} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}

	// Tree should be clean after the removal commit.
	status := exec.Command("git", "-C", root, "status", "--porcelain")
	st, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, st)
	}
	if len(bytes.TrimSpace(st)) != 0 {
		t.Fatalf("working tree should be clean after remove, got:\n%s", st)
	}
}

func TestIdeaRemoveMissingSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "remove", "tele", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("expected missing-idea error, got: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "moe idea list") {
		t.Fatalf("expected hint pointing at `moe idea list`, got: %q", errb.String())
	}
}

func TestIdeaRemoveRefusesUnregisteredProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "remove", "ghost", "anything"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing project, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Fatalf("expected unregistered-project error, got: %q", errb.String())
	}
}

func TestIdeaRemoveRefusesDirtyWorkingTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "add", "tele", "A thing"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed")
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "remove", "tele", "a-thing"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on dirty tree, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree error, got: %q", errb.String())
	}
	// Idea file must still be on disk — the dirty-tree check runs first.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "ideas", "a-thing.md")); err != nil {
		t.Fatalf("idea file should still exist after aborted remove: %v", err)
	}
}

func TestIdeaRemoveUsageErrorsOnMissingArgs(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "remove", "tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on missing slug, got %d; stderr=%q", code, errb.String())
	}
}

// gitLog runs `git -C root log <args>` and returns its stdout.
func gitLog(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "log"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, out)
	}
	return string(out)
}
