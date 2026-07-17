package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// todayDateSuffix returns the current local date in YYYY-MM-DD form —
// the same suffix run.nextFreeDatedID appends on an IDBase collision.
func todayDateSuffix() string {
	return time.Now().Local().Format("2006-01-02")
}

// captureIdea is a small wrapper around `moe idea new` for tests that
// need a pre-existing idea to promote. slug is the operator-typed
// kebab slug — moe idea new takes `<project>/<slug>` as a single
// positional now.
func captureIdea(t *testing.T, projectID, slug string) {
	t.Helper()
	stubEditor(t)
	if code := Run([]string{"idea", "new", projectID + "/" + slug}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup capture failed for %q", slug)
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
// target run date-suffixes (YYYY-MM-DD) because IDBase collided. The
// idea canvas gets copied into the new run's first-stage doc, and the
// source idea is bumped to StatusPromoted with a MoE-Promoted-To
// trailer — two commits, not one.
func TestRunNewFromIdeaSeedsFirstStageAndPromotesSource(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "cross-project-search")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=tele/cross-project-search"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "cross-project-search-" + todayDateSuffix()
	// Namespace is shared — the sdlc run date-suffixes because the idea
	// took "cross-project-search" first.
	if !strings.Contains(out.String(), "opened run tele/"+dated) {
		t.Fatalf("expected slug %q in output, got: %q", dated, out.String())
	}

	// First-stage (design) doc is seeded with the idea canvas. The
	// idea was captured by its slug — the H1 echoes the slug.
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", dated, "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("seeded design doc missing: %v", err)
	}
	if string(body) != "# cross-project-search\n" {
		t.Fatalf("unexpected seeded body: %q", body)
	}

	// HEAD is the idea-promote commit (status bump). Its predecessor
	// is the sdlc open commit.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Promote idea tele/cross-project-search → tele/"+dated) {
		t.Fatalf("expected promote commit at HEAD, got:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: cross-project-search",
		"MoE-Project: tele",
		"MoE-Workflow: idea",
		"MoE-Promoted-To: tele/" + dated,
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("promote commit missing %q:\n%s", want, head)
		}
	}

	// HEAD~1 is the sdlc run-open commit.
	prev := gitLog(t, root, "-1", "HEAD~1", "--format=%s%n%b")
	if !strings.Contains(prev, "Open run tele/"+dated+" from idea cross-project-search") {
		t.Fatalf("expected sdlc open commit below promote, got:\n%s", prev)
	}
	for _, want := range []string{
		"MoE-Run: " + dated,
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

func TestRunNewFromIdeaWorksForKBFirstStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "dns-basics")

	var out, errb bytes.Buffer
	code := runNew("kb", []string{"--from-idea=tele/dns-basics"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// kb's first stage is "research"; slug date-suffixes on collision.
	dated := "dns-basics-" + todayDateSuffix()
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", dated, "documents", "research", "content.md")); err != nil {
		t.Fatalf("kb's first-stage doc not seeded under %s: %v", dated, err)
	}
}

func TestRunNewFromIdeaErrorsOnMissingIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=tele/nope"}, &out, &errb)
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
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "twice-over")
	if code := runNew("sdlc", []string{"--from-idea=tele/twice-over"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("first promote failed")
	}
	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=tele/twice-over"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on second promote, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "already") {
		t.Fatalf("expected already-promoted error, got: %q", errb.String())
	}
}

// Smoke test of the flag-only invocation: --from-idea takes a full
// `<project>/<slug>` value and no positional. (Originally a regression
// for stdlib `flag` stopping at the first positional, when the shape
// included a trailing `<project>` positional; the shape collapsed when
// the run reference moved onto the flag value.)
func TestRunNewFromIdeaFlagOnlyInvocation(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "flag-ordering")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=tele/flag-ordering"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	want := "opened run tele/flag-ordering-" + todayDateSuffix()
	if !strings.Contains(out.String(), want) {
		t.Fatalf("missing open confirmation %q: %q", want, out.String())
	}
}

// After reorderFlags hoists every --foo to the front, stdlib `flag`
// rejects unknown ones outright rather than silently taking them as
// slugs. That's the belt-and-braces behavior the design called for.
func TestRunNewRejectsUnknownFlagLookalike(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele/some-slug", "--bogus"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Fatalf("expected error naming --bogus, got: %q", errb.String())
	}
}

// Without --from-idea the positional must be `<project>/<slug>`; a
// bare project token is the old shape and now rejected.
func TestRunNewRequiresSlugPositional(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "<project>/<run>") {
		t.Fatalf("expected slug-shape hint, got: %q", errb.String())
	}
}

// A non-canonical slug (uppercase, spaces, …) must be rejected at the
// verb boundary so silent slugify can't paper over operator typos.
func TestRunNewRejectsNonCanonicalSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele/Foo_Bar"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "lowercase kebab") {
		t.Fatalf("expected slug-shape error, got: %q", errb.String())
	}
}

// Collision on an operator-typed slug fails loud with a suggestion
// (no silent -2 auto-suffix).
func TestRunNewSlugCollisionFailsLoudWithSuggestion(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele/fix-the-thing"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("first runNew failed")
	}
	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"tele/fix-the-thing"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on slug collision, got 0; stdout=%q", out.String())
	}
	msg := errb.String()
	for _, want := range []string{`"fix-the-thing"`, "tele", "fix-the-thing-2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}

// writingEditor points EDITOR at a script that appends content to the
// file it's handed, so --seed's editor capture produces a body that
// differs from the stub (unlike stubEditor's no-op `true`).
func writingEditor(t *testing.T, content string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "seed-editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+content+"\\n' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")
}

// --park opens the run and prints the next-stage hint without prompting.
// The assertion doubles as the "no chain prompt" check: the interactive
// prompt's "run now?" text must never appear.
func TestRunNewParkPrintsHintAndExits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--park", "tele/park-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "opened run tele/park-me") {
		t.Fatalf("missing open confirmation: %q", out.String())
	}
	if !strings.Contains(out.String(), "next: moe sdlc design tele/park-me") {
		t.Fatalf("missing next-stage hint: %q", out.String())
	}
	if strings.Contains(out.String(), "run now?") {
		t.Fatalf("--park must not print the chain prompt: %q", out.String())
	}
}

// --park composes with --from-idea: promote, print the hint, stop.
func TestRunNewParkWithFromIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	captureIdea(t, "tele", "parked-promote")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--from-idea=tele/parked-promote", "--park"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "parked-promote-" + todayDateSuffix()
	if !strings.Contains(out.String(), "next: moe sdlc design tele/"+dated) {
		t.Fatalf("missing next-stage hint for promoted run: %q", out.String())
	}
	if strings.Contains(out.String(), "run now?") {
		t.Fatalf("--park must not print the chain prompt: %q", out.String())
	}
}

// --seed pops the editor and opens the run with the edited body as its
// first-stage (design) seed — the same SeedDocs mechanism promote uses.
func TestRunNewSeedOpensWithEditedBody(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	writingEditor(t, "Design the thing")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "tele/seeded"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "opened run tele/seeded") {
		t.Fatalf("missing open confirmation: %q", out.String())
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "seeded", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("seeded design doc missing: %v", err)
	}
	if !strings.Contains(string(body), "Design the thing") {
		t.Fatalf("edited seed body not on the first-stage canvas: %q", body)
	}
	// The stub H1 (# slug) rides along above the operator's addition.
	if !strings.Contains(string(body), "# seeded") {
		t.Fatalf("stub heading missing from seeded canvas: %q", body)
	}
}

// --seed --park is the operator's stated common case: mint the run from
// an editor seed and walk away, no chain prompt.
func TestRunNewSeedParkComposition(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	writingEditor(t, "walk away seed")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "--park", "tele/seed-park"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "next: moe sdlc design tele/seed-park") {
		t.Fatalf("missing next-stage hint: %q", out.String())
	}
	if strings.Contains(out.String(), "run now?") {
		t.Fatalf("--park must not print the chain prompt: %q", out.String())
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "seed-park", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("seeded design doc missing: %v", err)
	}
	if !strings.Contains(string(body), "walk away seed") {
		t.Fatalf("edited seed body not on the first-stage canvas: %q", body)
	}
}

// An unchanged stub (editor is a no-op) mints nothing — an accidental
// sdlc run is a dashboard entry that needs an explicit close, so --seed
// aborts rather than capturing a bare heading the way `idea new` does.
func TestRunNewSeedAbortsOnUnchangedStub(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t) // EDITOR=true: leaves the stub untouched

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "tele/untouched"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on unchanged stub, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "seed unchanged") {
		t.Fatalf("expected 'seed unchanged' abort, got: %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "untouched")); !os.IsNotExist(err) {
		t.Fatalf("aborted --seed must mint nothing; run dir exists (err=%v)", err)
	}
}

// --seed and --from-idea both claim the first-stage seed — reject the
// combination at the verb boundary.
func TestRunNewSeedMutuallyExclusiveWithFromIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "--from-idea=tele/whatever"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got: %q", errb.String())
	}
}

// --seed needs an editor; with neither $EDITOR nor $VISUAL set it fails
// before touching disk.
func TestRunNewSeedRequiresEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "tele/no-ed"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero with no editor, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "needs an editor") {
		t.Fatalf("expected editor-required error, got: %q", errb.String())
	}
}

// --seed pre-flights the slug before the editor pops: a collision fails
// fast and the editor never runs (so the operator never types into a
// tempfile we'd throw away). The fake editor bumps a counter; after the
// colliding second --seed it must still read zero.
func TestRunNewSeedSlugPreflightFiresBeforeEditor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	suppressNextStagePrompt(t)

	// First run takes the slug via a plain (non-seed) open.
	if code := runNew("sdlc", []string{"tele/taken"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup open failed")
	}

	editorDir := t.TempDir()
	marker := filepath.Join(editorDir, "ran")
	script := filepath.Join(editorDir, "counting-editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ran\\n' >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--seed", "tele/taken"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on slug collision, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "already used") {
		t.Fatalf("expected collision error, got: %q", errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("editor ran before the slug pre-flight (marker exists, err=%v)", err)
	}
}

func TestRunNewAgentHelpNamesPersistenceBoundary(t *testing.T) {
	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--help"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected help exit (2), got %d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	got := errb.String()
	if !strings.Contains(got, "Explicit values persist to run.json; omitted values resolve at stage time") {
		t.Fatalf("help missing agent persistence boundary:\n%s", got)
	}
	if !strings.Contains(got, "$MOE_AGENT, then claude") {
		t.Fatalf("help missing fallback step:\n%s", got)
	}
}
