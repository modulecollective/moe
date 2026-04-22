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

// noEditor unsets EDITOR/VISUAL so launchEditor takes the print-hint
// path instead of trying to spawn an interactive program in the test.
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
	for _, want := range []string{"new", "list"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("idea usage missing subcommand %q: %q", want, out.String())
		}
	}
}

func TestIdeaNewCreatesFileAndCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "tele", "Faster dash load"}, &out, &errb)
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

func TestIdeaNewAutoSuffixesOnCollision(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	for _, want := range []string{"tele/foo", "tele/foo-2", "tele/foo-3"} {
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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	if code := Run([]string{"idea", "new", "--id=mine", "tele", "first"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first new failed: code=%d", code)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"idea", "new", "--id=mine", "tele", "second"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on explicit-id collision, got 0; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
}

func TestIdeaNewRefusesUnregisteredProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	// Drop a stray untracked file so `git status --porcelain` reports it.
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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

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
