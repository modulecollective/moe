package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureIdea is a small wrapper around `moe idea add` for tests that
// need a pre-existing idea to promote.
func captureIdea(t *testing.T, projectID, title string) {
	t.Helper()
	noEditor(t)
	if code := Run([]string{"idea", "add", projectID, title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed for %q", title)
	}
}

// suppressNextStagePrompt redirects os.Stdin to a regular file so
// promptNextStage's stdinIsTerminal probe returns false and it just
// prints the next-stage hint instead of chaining into the next stage's
// command. Without this the test inherits the operator's terminal,
// promptNextStage sees a tty, and a default-yes design prompt fires
// runDesign — which tries to launch the real `claude` binary.
func suppressNextStagePrompt(t *testing.T) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = oldStdin
		f.Close()
	})
}

func TestRunNewFromIdeaSeedsFirstStageAndDeletesIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Cross-project search")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=cross-project-search", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "opened run tele/cross-project-search") {
		t.Fatalf("missing open confirmation: %q", out.String())
	}

	// Idea file is gone.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "ideas", "cross-project-search.md")); !os.IsNotExist(err) {
		t.Fatalf("expected idea file deleted, stat err=%v", err)
	}
	// First-stage doc is seeded with the idea body.
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "cross-project-search", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("seeded design doc missing: %v", err)
	}
	if string(body) != "# Cross-project search\n" {
		t.Fatalf("unexpected seeded body: %q", body)
	}
	// One commit covers both the run open, the seed, and the idea removal.
	// --no-renames keeps git from collapsing the seeded-doc/idea pair into
	// a single R-line — which it does when the bodies happen to match — so
	// we can assert on the A and D operations independently.
	head := gitLog(t, root, "-1", "--name-status", "--no-renames", "--format=%s%n%b")
	if !strings.Contains(head, "Open run tele/cross-project-search from idea cross-project-search:") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: cross-project-search",
		"MoE-Project: tele",
		"MoE-Idea: cross-project-search",
		"A\tprojects/tele/runs/cross-project-search/run.json",
		"A\tprojects/tele/runs/cross-project-search/documents/design/content.md",
		"D\tprojects/tele/ideas/cross-project-search.md",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing %q:\n%s", want, head)
		}
	}
}

func TestRunNewFromIdeaExplicitTitleOverridesH1(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Original title")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=original-title", "tele", "Renamed at promote"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Slug is derived from the explicit title (run.New behaviour), so the
	// run id changes too.
	if !strings.Contains(out.String(), "opened run tele/renamed-at-promote") {
		t.Fatalf("expected slug from explicit title, got: %q", out.String())
	}
	head := gitLog(t, root, "-1", "--format=%s")
	if !strings.Contains(head, "Renamed at promote") {
		t.Fatalf("commit subject should carry explicit title, got: %q", head)
	}
}

func TestRunNewFromIdeaWorksForKBFirstStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "DNS basics")

	var out, errb bytes.Buffer
	code := runNew("kb", []string{"--from-idea=dns-basics", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// kb's first stage is "research".
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "dns-basics", "documents", "research", "content.md")); err != nil {
		t.Fatalf("kb's first-stage doc not seeded: %v", err)
	}
}

func TestRunNewFromIdeaErrorsOnMissingIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=nope", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "--from-idea") || !strings.Contains(errb.String(), "nope.md") {
		t.Fatalf("expected missing-idea error to name the path, got: %q", errb.String())
	}
}

func TestRunNewRequiresTitleWithoutFromIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("expected usage hint, got: %q", errb.String())
	}
}
