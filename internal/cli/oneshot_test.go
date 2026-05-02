package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedSdlcOneShotProject sets up a registered project with a real
// submodule on disk so the code-stage sandbox.Ensure call can clone it.
// Mirrors newPushFixture's submodule wiring without the run / branch
// scaffolding push tests need — one-shot starts from a fresh title and
// builds the run as it goes.
func seedSdlcOneShotProject(t *testing.T, root, projectID string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), projectID+".git")
	mustGit(t, "", "init", "--bare", "-b", "main", origin)
	seed := t.TempDir()
	mustGit(t, "", "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	mustGit(t, seed, "add", "README.md")
	mustGit(t, seed, "commit", "-m", "seed")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "origin", "main")

	subPath := filepath.Join("projects", projectID, "src")
	mustGit(t, root, "-c", "protocol.file.allow=always",
		"submodule", "add", "-b", "main", origin, subPath)
	writeFile(t, filepath.Join(root, "projects", projectID, "project.json"),
		`{"id":"`+projectID+`","submodule":"`+subPath+`","remote":"`+origin+`","default_branch":"main"}`+"\n")
	// `git add -A` so bureaucracy.conf (laid down by markBureaucracy
	// before this helper runs) and any other pending files come along
	// — runNew refuses on a dirty tree, and `submodule add` plus the
	// markBureaucracy file together leave several pending paths.
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-m", "Register project "+projectID)
}

// fakeOneShotClaude installs a `claude` stub that, on every -p
// invocation, parses --append-system-prompt to find the canvas path
// (the line under "Your canvas for this document is the single
// file:") and appends a tagged line to it. Appending — not overwriting
// — lets the from-idea test verify the seed survives the agent turn.
//
// failOn lets a test inject a deterministic failure for a specific
// docID's stage call: if the canvas path matches `*/<failOn>/content.md`,
// the script writes nothing and exits with the supplied code (use 0
// to silently refuse instead of crashing). Empty failOn never fails.
func fakeOneShotClaude(t *testing.T, failOn string, failExit int, marker string) {
	t.Helper()
	script := `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
` + failOnSnippet(failOn, failExit) + `
if [ -n "$canvas" ]; then printf '` + marker + `\n' >> "$canvas"; fi
exit 0
`
	fakeClaudeOnPath(t, script)
}

// failOnSnippet emits the shell case that exits early when the canvas
// is the failed stage's. Empty failOn returns "" so the script just
// always writes and exits 0.
func failOnSnippet(failOn string, failExit int) string {
	if failOn == "" {
		return ""
	}
	return `case "$canvas" in
  */documents/` + failOn + `/content.md) exit ` + strconv.Itoa(failExit) + ` ;;
esac
`
}

func TestRunNewOneShotChainsDesignAndCode(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeOneShotClaude(t, "", 0, "written by fake claude")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--one-shot", "tele", "Test feature"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	// Both stage canvases exist with the fake claude marker.
	for _, doc := range []string{"design", "code"} {
		body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "test-feature", "documents", doc, "content.md"))
		if err != nil {
			t.Fatalf("%s canvas missing: %v", doc, err)
		}
		if !strings.Contains(string(body), "written by fake claude") {
			t.Fatalf("%s canvas missing fake-claude marker: %q", doc, body)
		}
	}

	// Two `work: update` commits land — one per stage — each with the
	// MoE-Document trailer keyed to the right doc.
	log := gitLog(t, root, "--format=%s%n%b", "--grep=^work: update")
	for _, want := range []string{
		"work: update design",
		"work: update code",
		"MoE-Document: design",
		"MoE-Document: code",
		"MoE-Run: test-feature",
		"MoE-Workflow: sdlc",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("commit log missing %q:\n%s", want, log)
		}
	}

	// Chain hands off to promptNextStage; suppressNextStagePrompt
	// pins stdin to a non-tty so it falls through to the `next: …`
	// hint instead of the interactive [N/m/p] ship prompt.
	if !strings.Contains(out.String(), "next: moe workflow sdlc push tele test-feature") {
		t.Fatalf("expected post-chain next-stage hint in stdout, got: %q", out.String())
	}
}

func TestRunNewOneShotComposesWithFromIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	captureIdea(t, "tele", "Cross-project search")
	fakeOneShotClaude(t, "", 0, "fake-claude refined")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--one-shot", "--from-idea=cross-project-search", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	dated := "cross-project-search-" + todayDateSuffix()
	designCanvas := filepath.Join(root, "projects", "tele", "runs", dated, "documents", "design", "content.md")
	body, err := os.ReadFile(designCanvas)
	if err != nil {
		t.Fatalf("design canvas missing: %v", err)
	}
	got := string(body)
	// Idea seed + one-shot agent's append both land on disk — the
	// design's "code stage runs against the seeded design" guarantee
	// is the agent saw the seed.
	if !strings.Contains(got, "# Cross-project search") {
		t.Fatalf("design canvas should retain idea seed:\n%s", got)
	}
	if !strings.Contains(got, "fake-claude refined") {
		t.Fatalf("design canvas should carry agent edit:\n%s", got)
	}
	// Code stage ran (chain proceeded past design).
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", dated, "documents", "code", "content.md")); err != nil {
		t.Fatalf("code canvas missing — chain did not advance: %v", err)
	}
}

func TestRunNewOneShotStopsWhenDesignWritesNoCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Fake claude exits 0 without writing the canvas. commitTurn's
	// canvas-existence assertion catches it; the chain never advances
	// to code.
	fakeClaudeOnPath(t, `#!/bin/sh
exit 0
`)

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--one-shot", "tele", "Empty turn"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit when design canvas is missing; stdout=%q stderr=%q", out.String(), errb.String())
	}

	// Design canvas absent.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "empty-turn", "documents", "design", "content.md")); !os.IsNotExist(err) {
		t.Fatalf("design canvas should not exist: err=%v", err)
	}
	// Code stage was never invoked — no code dir, no code commit.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "empty-turn", "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist: err=%v", err)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^work: update code"); strings.TrimSpace(log) != "" {
		t.Fatalf("code stage should not have committed; got log:\n%s", log)
	}
}

func TestRunNewOneShotCodeFailureLeavesRunWithoutPush(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Design succeeds (writes its canvas, exit 0); code exits non-zero
	// without writing. The chain returns the code stage's exit status,
	// the run is left where it is, and push is not invoked from the
	// chain (SkipNextStage suppresses the next-stage prompt).
	fakeOneShotClaude(t, "code", 7, "design only")

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--one-shot", "tele", "Half done"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit when code stage fails; stdout=%q stderr=%q", out.String(), errb.String())
	}

	// Design canvas + commit landed.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "half-done", "documents", "design", "content.md")); err != nil {
		t.Fatalf("design canvas should exist: %v", err)
	}
	// Code canvas absent (claude exited before writing).
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "half-done", "documents", "code", "content.md")); !os.IsNotExist(err) {
		t.Fatalf("code canvas should not exist: err=%v", err)
	}
	// No push commit / merge / PR — the chain stops at code.
	for _, forbidden := range []string{"sdlc: ship", "sdlc: open PR for", "Merge branch"} {
		if log := gitLog(t, root, "--format=%s", "-1"); strings.Contains(log, forbidden) {
			t.Fatalf("did not expect post-code action %q in HEAD: %q", forbidden, log)
		}
	}
}

func TestRunNewOneShotRejectsNonSdlcWorkflow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := runNew("kb", []string{"--one-shot", "tele", "DNS basics"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 for --one-shot on kb, got %d (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "--one-shot: only sdlc supports") {
		t.Fatalf("expected sdlc-only error, got stderr=%q", errb.String())
	}
}

func TestRunNewOneShotAppendsOneShotFragmentToSystemPrompt(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Capture --append-system-prompt to a tempfile so the test can
	// assert the one-shot addendum landed in the assembled prompt.
	captureFile := filepath.Join(t.TempDir(), "prompts.txt")
	t.Setenv("MOE_TEST_PROMPT_DUMP", captureFile)
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
printf '%s\n--END-PROMPT--\n' "$prompt" >> "$MOE_TEST_PROMPT_DUMP"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'fake-claude wrote canvas\n' >> "$canvas"; fi
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"--one-shot", "tele", "Has fragment"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dump, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("prompt dump missing: %v", err)
	}
	prompts := strings.Split(string(dump), "--END-PROMPT--")
	// design + code = two non-empty prompt captures.
	got := 0
	for _, p := range prompts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		got++
		if !strings.Contains(p, "# One-shot") {
			t.Fatalf("captured prompt is missing one-shot fragment header:\n%s", p)
		}
		if !strings.Contains(p, "you only get one turn") {
			t.Fatalf("captured prompt is missing one-shot body:\n%s", p)
		}
	}
	if got != 2 {
		t.Fatalf("expected 2 prompts captured (design + code), got %d", got)
	}
}
