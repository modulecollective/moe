package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// seedSdlcOneShotProject sets up a registered project with a real
// submodule on disk so the code-stage sandbox.Ensure call can clone it.
// Mirrors newPushFixture's submodule wiring without the run / branch
// scaffolding push tests need — one-shot starts from a fresh title and
// builds the run as it goes.
func seedSdlcOneShotProject(t *testing.T, root, projectID string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), projectID+".git")
	gittest.Run(t, "", "init", "--bare", "-b", "main", origin)
	seed := t.TempDir()
	gittest.Run(t, "", "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	gittest.Run(t, seed, "add", "README.md")
	gittest.Run(t, seed, "commit", "-m", "seed")
	gittest.Run(t, seed, "remote", "add", "origin", origin)
	gittest.Run(t, seed, "push", "origin", "main")

	subPath := filepath.Join("projects", projectID, "src")
	gittest.Run(t, root, "-c", "protocol.file.allow=always",
		"submodule", "add", "-b", "main", origin, subPath)
	writeFile(t, filepath.Join(root, "projects", projectID, "project.json"),
		`{"id":"`+projectID+`","submodule":"`+subPath+`","remote":"`+origin+`","default_branch":"main"}`+"\n")
	// `git add -A` so bureaucracy.conf (laid down by markBureaucracy
	// before this helper runs) and any other pending files come along
	// — runNew refuses on a dirty tree, and `submodule add` plus the
	// markBureaucracy file together leave several pending paths.
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "Register project "+projectID)
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

// TestRunNewOneShotRunsDesignAndPromptsForCode pins the post-tweak
// behaviour: --one-shot drives the *first* pending stage (design)
// headlessly, lands the canvas + commit, then hands off to the
// chain-prompt-per-stage `[Y/n/o]` between design and code. Without
// the prompt the operator never gets a checkpoint to spot-check the
// design before code runs against it. Non-tty stdin (the suppress
// helper) collapses the prompt to its `next: …` hint, so the test
// asserts on the hint rather than driving the prompt.
func TestRunNewOneShotRunsDesignAndPromptsForCode(t *testing.T) {
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

	// Design canvas landed.
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "test-feature", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("design canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "written by fake claude") {
		t.Fatalf("design canvas missing fake-claude marker: %q", body)
	}
	// Code stage was *not* auto-run — the chain prompt now stops here
	// for the operator to type `o` (or to walk away and review with
	// `moe review`).
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "test-feature", "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist on chain-prompt stop: err=%v", err)
	}

	log := gitLog(t, root, "--format=%s%n%b", "--grep=^work: update")
	for _, want := range []string{
		"work: update design",
		"MoE-Document: design",
		"MoE-Run: test-feature",
		"MoE-Workflow: sdlc",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("commit log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "work: update code") {
		t.Fatalf("code stage should not have committed:\n%s", log)
	}

	// Chain prompt's non-tty fallback prints the next-stage hint —
	// pointing at the code stage, not push, because the chain stopped
	// after design.
	if !strings.Contains(out.String(), "next: moe sdlc code tele test-feature") {
		t.Fatalf("expected post-design next-stage hint in stdout, got: %q", out.String())
	}
}

// TestRunNewOneShotComposesWithFromIdea: --from-idea seeds the design
// canvas and --one-shot lands the agent's edit on top of that seed.
// The design-then-code chain stops at the chain prompt now — same
// shape as TestRunNewOneShotRunsDesignAndPromptsForCode — so we
// assert the seed survived plus the next-stage hint, not that code
// auto-ran.
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
	if !strings.Contains(got, "# Cross-project search") {
		t.Fatalf("design canvas should retain idea seed:\n%s", got)
	}
	if !strings.Contains(got, "fake-claude refined") {
		t.Fatalf("design canvas should carry agent edit:\n%s", got)
	}
	// Code dir should not exist — chain stops at the prompt.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", dated, "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist on chain-prompt stop: err=%v", err)
	}
	if !strings.Contains(out.String(), "next: moe sdlc code tele "+dated) {
		t.Fatalf("expected post-design next-stage hint in stdout, got: %q", out.String())
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
	// design = one non-empty prompt capture; the chain prompts the
	// operator before code rather than auto-running it.
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
	if got != 1 {
		t.Fatalf("expected 1 prompt captured (design only — chain stops here), got %d", got)
	}
}

// One-shot has no operator on stdin, so claude has to be invoked with
// --permission-mode bypassPermissions or write/edit/bash tool calls
// silently deny. Belt-and-suspenders against the flag being dropped in
// a future refactor: capture argv from a fake claude and assert the
// pair appears for both stages.
func TestRunNewOneShotPassesBypassPermissionsFlag(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	argvFile := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("MOE_TEST_ARGV_DUMP", argvFile)
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOE_TEST_ARGV_DUMP"
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
printf -- '--END-ARGV--\n' >> "$MOE_TEST_ARGV_DUMP"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'fake-claude wrote canvas\n' >> "$canvas"; fi
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"--one-shot", "tele", "Bypass flag"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dump, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("argv dump missing: %v", err)
	}
	invocations := strings.Split(string(dump), "--END-ARGV--")
	// design = one non-empty argv capture; chain stops at the prompt
	// before code, so we only see the design invocation here.
	got := 0
	for _, inv := range invocations {
		if strings.TrimSpace(inv) == "" {
			continue
		}
		got++
		args := strings.Split(strings.TrimSpace(inv), "\n")
		// Find --permission-mode and assert its value is bypassPermissions.
		// Fail loudly with full argv on mismatch — this is the exact flag
		// the bug fix turns on, so a regression should be obvious.
		idx := -1
		for i, a := range args {
			if a == "--permission-mode" {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("invocation missing --permission-mode flag:\n%s", inv)
		}
		if idx+1 >= len(args) || args[idx+1] != "bypassPermissions" {
			t.Fatalf("--permission-mode value should be bypassPermissions, got %q in:\n%s", args[idx+1:], inv)
		}
	}
	if got != 1 {
		t.Fatalf("expected 1 argv capture (design only — chain stops here), got %d", got)
	}
}

// Per-stage --one-shot on design lands the canvas headlessly. The run
// title flows through as the user prompt (not the interactive kickoff
// string), so the agent gets the same context the chained one-shot
// already exercises.
func TestRunDesignOneShot(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	argvFile := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("MOE_TEST_ARGV_DUMP", argvFile)
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOE_TEST_ARGV_DUMP"
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
printf -- '--END-ARGV--\n' >> "$MOE_TEST_ARGV_DUMP"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'design via per-stage one-shot\n' >> "$canvas"; fi
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Per-stage design"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runDesign([]string{"--one-shot", "tele", "per-stage-design"}, &out, &errb); code != 0 {
		t.Fatalf("runDesign --one-shot exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "per-stage-design", "documents", "design", "content.md"))
	if err != nil {
		t.Fatalf("design canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "design via per-stage one-shot") {
		t.Fatalf("design canvas missing fake-claude marker: %q", body)
	}

	dump, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("argv dump missing: %v", err)
	}
	args := string(dump)
	// Run title is the user prompt under -p; the interactive kickoff
	// must not leak in.
	if !strings.Contains(args, "Per-stage design") {
		t.Fatalf("expected run title as user prompt, got argv:\n%s", args)
	}
	if strings.Contains(args, "greet the operator") {
		t.Fatalf("interactive kickoff string leaked into headless argv:\n%s", args)
	}
	if !strings.Contains(args, "-p\n") {
		t.Fatalf("expected -p invocation, got argv:\n%s", args)
	}
}

// Per-stage --one-shot on code runs under the sandbox clone with the
// design canvas pre-seeded. Canvas lands; design canvas keeps the
// content the design turn wrote.
func TestRunCodeOneShot(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeOneShotClaude(t, "", 0, "written by fake claude")

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "Per-stage code"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	// Land the design canvas first so the precheck passes and code has
	// something to work against.
	out.Reset()
	errb.Reset()
	if code := runDesign([]string{"--one-shot", "tele", "per-stage-code"}, &out, &errb); code != 0 {
		t.Fatalf("runDesign --one-shot exit=%d stderr=%q", code, errb.String())
	}
	designCanvas := filepath.Join(root, "projects", "tele", "runs", "per-stage-code", "documents", "design", "content.md")
	beforeDesign, err := os.ReadFile(designCanvas)
	if err != nil {
		t.Fatalf("design canvas missing after design stage: %v", err)
	}

	out.Reset()
	errb.Reset()
	if code := runCode([]string{"--one-shot", "tele", "per-stage-code"}, &out, &errb); code != 0 {
		t.Fatalf("runCode --one-shot exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "per-stage-code", "documents", "code", "content.md"))
	if err != nil {
		t.Fatalf("code canvas missing: %v", err)
	}
	if !strings.Contains(string(body), "written by fake claude") {
		t.Fatalf("code canvas missing fake-claude marker: %q", body)
	}

	// Design canvas is unchanged by the code stage.
	afterDesign, err := os.ReadFile(designCanvas)
	if err != nil {
		t.Fatalf("design canvas missing after code stage: %v", err)
	}
	if string(beforeDesign) != string(afterDesign) {
		t.Fatalf("code stage mutated design canvas:\nbefore: %q\nafter: %q", beforeDesign, afterDesign)
	}

	log := gitLog(t, root, "--format=%s%n%b", "--grep=^work: update code")
	for _, want := range []string{
		"work: update code",
		"MoE-Document: code",
		"MoE-Run: per-stage-code",
		"MoE-Workflow: sdlc",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("commit log missing %q:\n%s", want, log)
		}
	}
}

// runCode refuses — both interactive and headless — when the run has
// no design canvas yet. The precheck fires before any session work, so
// the run dir gets no code/ subdirectory and no `work: update code`
// commit lands.
func TestRunCodeRefusesWithoutDesignCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele", "No design"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	for _, args := range [][]string{
		{"tele", "no-design"},
		{"--one-shot", "tele", "no-design"},
	} {
		out.Reset()
		errb.Reset()
		if code := runCode(args, &out, &errb); code == 0 {
			t.Fatalf("expected refusal exit for %v, got 0; stdout=%q stderr=%q", args, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "design canvas missing") {
			t.Fatalf("expected design-canvas error for %v, got stderr=%q", args, errb.String())
		}
		if !strings.Contains(errb.String(), "moe sdlc design tele no-design") {
			t.Fatalf("expected guidance to run design first for %v, got stderr=%q", args, errb.String())
		}
	}

	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "no-design", "documents", "code")); !os.IsNotExist(err) {
		t.Fatalf("code dir should not exist after refusals: err=%v", err)
	}
	if log := gitLog(t, root, "--format=%s", "--grep=^work: update code"); strings.TrimSpace(log) != "" {
		t.Fatalf("no code commit should land on refusal; got:\n%s", log)
	}
}

// promptStageNextStage offers [Y/n/o] for sdlc non-push stages and
// `o` invokes the next stage with --one-shot prepended. Non-sdlc
// workflows keep the [Y/n] label and never see the o option. Mirrors
// capturePromptDispatch's shape: stub the next.Run, pipe stdin, call
// the helper directly so the test isn't bound to stdinIsTerminal().
func TestPromptNextStageOfferOneShot(t *testing.T) {
	cases := []struct {
		name      string
		workflow  string
		input     string
		wantLabel string
		wantArgs  []string
	}{
		{name: "sdlc-o-runs-headless", workflow: "sdlc", input: "o\n", wantLabel: "[Y/n/o]", wantArgs: []string{"--one-shot", "tele", "fix-it"}},
		{name: "sdlc-default-runs-interactive", workflow: "sdlc", input: "\n", wantLabel: "[Y/n/o]", wantArgs: []string{"tele", "fix-it"}},
		{name: "sdlc-y-runs-interactive", workflow: "sdlc", input: "y\n", wantLabel: "[Y/n/o]", wantArgs: []string{"tele", "fix-it"}},
		{name: "sdlc-n-declines", workflow: "sdlc", input: "n\n", wantLabel: "[Y/n/o]", wantArgs: nil},
		{name: "kb-no-o-option", workflow: "kb", input: "o\n", wantLabel: "[Y/n]", wantArgs: nil},
		{name: "kb-default-runs", workflow: "kb", input: "\n", wantLabel: "[Y/n]", wantArgs: []string{"tele", "fix-it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &promptDispatchRecord{}
			next := &Command{
				Name: "code",
				Run: func(args []string, _, _ io.Writer) int {
					rec.ran = true
					rec.args = append([]string(nil), args...)
					return 0
				},
			}
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: tc.workflow, Status: run.StatusInProgress}

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := io.WriteString(w, tc.input); err != nil {
				t.Fatal(err)
			}
			w.Close()
			oldStdin := os.Stdin
			os.Stdin = r
			t.Cleanup(func() { os.Stdin = oldStdin })

			var stdout, stderr bytes.Buffer
			if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe "+tc.workflow+" code tele fix-it", &stdout, &stderr); code != 0 {
				t.Fatalf("promptStageNextStage exit=%d stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.wantLabel) {
				t.Fatalf("expected label %q in prompt, got: %q", tc.wantLabel, stdout.String())
			}
			if tc.wantArgs == nil {
				if rec.ran {
					t.Fatalf("expected no dispatch, got args=%v", rec.args)
				}
				return
			}
			if !rec.ran {
				t.Fatalf("expected dispatch with args=%v, got none", tc.wantArgs)
			}
			if got, want := strings.Join(rec.args, " "), strings.Join(tc.wantArgs, " "); got != want {
				t.Fatalf("dispatched args = %q, want %q", got, want)
			}
		})
	}
}
