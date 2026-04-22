package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureIdea is a small wrapper around `moe idea new` for tests that
// need a pre-existing idea to promote.
func captureIdea(t *testing.T, projectID, title string) {
	t.Helper()
	stubEditor(t)
	if code := Run([]string{"idea", "new", projectID, title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
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

// Ideas and runs share a single slug namespace per project. The idea
// run captured first occupies "cross-project-search"; the promoted
// target run auto-suffixes to "cross-project-search-2". The idea
// canvas gets copied into the new run's first-stage doc, and the
// source idea is bumped to StatusPromoted with a MoE-Promoted-To
// trailer — two commits, not one.
func TestRunNewFromIdeaSeedsFirstStageAndPromotesSource(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Cross-project search")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=cross-project-search", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Namespace is shared — the sdlc run auto-suffixes because the idea
	// took "cross-project-search" first.
	if !strings.Contains(out.String(), "opened run tele/cross-project-search-2") {
		t.Fatalf("expected slug auto-suffix on shared namespace, got: %q", out.String())
	}

	// First-stage (design) doc is seeded with the idea canvas.
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "cross-project-search-2", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("seeded design doc missing: %v", err)
	}
	if string(body) != "# Cross-project search\n" {
		t.Fatalf("unexpected seeded body: %q", body)
	}

	// HEAD is the idea-promote commit (status bump). Its predecessor
	// is the sdlc open commit.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Promote idea tele/cross-project-search → tele/cross-project-search-2") {
		t.Fatalf("expected promote commit at HEAD, got:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: cross-project-search",
		"MoE-Project: tele",
		"MoE-Workflow: idea",
		"MoE-Promoted-To: tele/cross-project-search-2",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("promote commit missing %q:\n%s", want, head)
		}
	}

	// HEAD~1 is the sdlc run-open commit.
	prev := gitLog(t, root, "-1", "HEAD~1", "--format=%s%n%b")
	if !strings.Contains(prev, "Open run tele/cross-project-search-2 from idea cross-project-search:") {
		t.Fatalf("expected sdlc open commit below promote, got:\n%s", prev)
	}
	for _, want := range []string{
		"MoE-Run: cross-project-search-2",
		"MoE-Project: tele",
		"MoE-Idea: cross-project-search",
	} {
		if !strings.Contains(prev, want) {
			t.Fatalf("sdlc open commit missing %q:\n%s", want, prev)
		}
	}

	// Idea canvas is still on disk (ideas are now runs; we don't delete
	// their files on promotion — the status bump is the lifecycle event).
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "cross-project-search", "documents", "idea", "content.md")); err != nil {
		t.Fatalf("source idea canvas should remain on disk after promotion: %v", err)
	}

	// Source run.json now has status=promoted.
	srcJSON, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "cross-project-search", "run.json"))
	if err != nil {
		t.Fatalf("source run.json missing: %v", err)
	}
	if !strings.Contains(string(srcJSON), `"status": "promoted"`) {
		t.Fatalf("source run.json status not promoted:\n%s", srcJSON)
	}
}

func TestRunNewFromIdeaExplicitTitleOverridesIdeaTitle(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Original title")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=original-title", "tele", "Renamed at promote"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Slug is derived from the explicit title (run.New behaviour).
	if !strings.Contains(out.String(), "opened run tele/renamed-at-promote") {
		t.Fatalf("expected slug from explicit title, got: %q", out.String())
	}
	// HEAD is the promote commit; sdlc open commit is HEAD~1.
	prev := gitLog(t, root, "-1", "HEAD~1", "--format=%s")
	if !strings.Contains(prev, "Renamed at promote") {
		t.Fatalf("sdlc open subject should carry explicit title, got: %q", prev)
	}
}

func TestRunNewFromIdeaWorksForKBFirstStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "DNS basics")

	var out, errb bytes.Buffer
	code := runNew("kb", []string{"--from-idea=dns-basics", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// kb's first stage is "research"; slug auto-suffixes on collision.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "dns-basics-2", "documents", "research", "content.md")); err != nil {
		t.Fatalf("kb's first-stage doc not seeded: %v", err)
	}
}

func TestRunNewFromIdeaErrorsOnMissingIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=nope", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing idea, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "--from-idea") {
		t.Fatalf("expected missing-idea error to name the flag, got: %q", errb.String())
	}
}

func TestRunNewFromIdeaRefusesPromotedIdea(t *testing.T) {
	// Promoting the same idea twice would split a single intent
	// across two runs — refuse the second attempt.
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Twice over")
	if code := runNew("sdlc", []string{"--from-idea=twice-over", "tele"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("first promote failed")
	}
	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=twice-over", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on second promote, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "already") {
		t.Fatalf("expected already-promoted error, got: %q", errb.String())
	}
}

// Regression: the operator typed `moe sdlc new tele --from-idea=x` and
// stdlib `flag` stopped at the first positional, so `--from-idea=x`
// became part of the title. After reorderFlags this shape seeds the
// first-stage doc just like the --from-idea-first form does.
func TestRunNewTolerantToFlagsAfterPositional(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Flag ordering")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele", "--from-idea=flag-ordering"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "opened run tele/flag-ordering-2") {
		t.Fatalf("missing open confirmation: %q", out.String())
	}
}

// After reorderFlags hoists every --foo to the front, stdlib `flag`
// rejects unknown ones outright rather than silently taking them as
// titles. That's the belt-and-braces behavior the design called for.
func TestRunNewRejectsUnknownFlagLookalike(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele", "Some title", "--bogus"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Fatalf("expected error naming --bogus, got: %q", errb.String())
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
