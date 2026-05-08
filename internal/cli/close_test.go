package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// seedCloseFixture composes the test setup every close test wants: a
// bureaucracy repo with the marker committed, plus a run seeded via
// seedRun (which also lands project.json). Without this, the marker
// stays untracked and close's clean-tree check refuses.
func seedCloseFixture(t *testing.T, projectID, runID, workflow, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	addCmd := exec.Command("git", "-C", root, "add", "bureaucracy.conf")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commit := exec.Command("git", "-C", root, "commit", "-m", "mark bureaucracy")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	seedRun(t, root, projectID, runID, workflow, status)
	return root
}

// TestSDLCCloseRemovesSandboxAndCommits is the happy path: an
// in-progress sdlc run with a sandbox on disk ends up closed, with the
// sandbox gone and a properly trailered close commit on HEAD.
func TestSDLCCloseRemovesSandboxAndCommits(t *testing.T) {
	root := seedCloseFixture(t, "tele", "abandon-me", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// sandbox.Remove just os.RemoveAll()s two paths — we fake a sandbox
	// with plain dirs so the test doesn't need a live submodule.
	sandboxPath := sandbox.Path(root, "tele", "abandon-me")
	gitDirPath := filepath.Join(root, ".moe", "clones", "tele", ".git-modules", "abandon-me")
	if err := os.MkdirAll(sandboxPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitDirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "abandon-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed sdlc tele/abandon-me") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	if _, err := os.Stat(sandboxPath); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox gone, stat err=%v", err)
	}
	if _, err := os.Stat(gitDirPath); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox gitdir gone, stat err=%v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "abandon-me", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(body), `"status": "closed"`) {
		t.Fatalf("run.json status not flipped:\n%s", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close sdlc run tele/abandon-me") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: abandon-me",
		"MoE-Project: tele",
		"MoE-Workflow: sdlc",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

// TestSDLCCloseWithoutSandboxIsFine covers the case where the operator
// opened a run but never got as far as `moe sdlc code` — no sandbox
// exists yet. Close should still work; sandbox.Remove is idempotent.
func TestSDLCCloseWithoutSandboxIsFine(t *testing.T) {
	root := seedCloseFixture(t, "tele", "never-opened", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "never-opened"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed sdlc tele/never-opened") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}
}

// TestSDLCCloseRefusesPushed: a pushed run's terminal state is reached
// via GitHub + sync, not by local close. The error must point the
// operator at that path so they don't try to force it.
func TestSDLCCloseRefusesPushed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"pushed", run.StatusPushed},
		{"merged", run.StatusMerged},
		{"closed", run.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "done-"+tc.name, "sdlc", tc.status)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			beforeHead := gitLog(t, root, "-1", "--format=%H")

			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", "close", "--no-edit", "tele", "done-" + tc.name}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s, stdout=%q", tc.status, out.String())
			}
			if tc.status == run.StatusPushed {
				// Pushed gets a steering message pointing at sync.
				if !strings.Contains(errb.String(), "moe sync") {
					t.Fatalf("pushed refusal should mention `moe sync`: %q", errb.String())
				}
			} else {
				if !strings.Contains(errb.String(), "already") {
					t.Fatalf("terminal refusal should say 'already': %q", errb.String())
				}
			}

			// No new commit and run.json status unchanged.
			afterHead := gitLog(t, root, "-1", "--format=%H")
			if beforeHead != afterHead {
				t.Fatalf("refused close created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
			}
			body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "done-"+tc.name, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"status": "`+tc.status+`"`) {
				t.Fatalf("run.json status mutated under refusal:\n%s", body)
			}
		})
	}
}

// TestSDLCCloseWorkflowMismatch: `sdlc close` must refuse a run that
// lives in a different workflow, even if the slug matches — the slug
// namespace is shared per project, so this is a real footgun.
func TestSDLCCloseWorkflowMismatch(t *testing.T) {
	root := seedCloseFixture(t, "tele", "not-sdlc", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "not-sdlc"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on workflow mismatch, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "kb run") || !strings.Contains(errb.String(), "not sdlc") {
		t.Fatalf("expected workflow-mismatch error, got: %q", errb.String())
	}
}

// TestSDLCCloseMissingRun: running against a slug that was never
// opened should fail fast, not panic or silently commit.
func TestSDLCCloseMissingRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele") // seedProject commits everything pending, including the marker
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing run, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("expected does-not-exist error, got: %q", errb.String())
	}
}

// TestKBCloseBumpsStatusAndCommits: kb close has no sandbox/branch
// cleanup step — just status flip + trailered commit. Assert both.
func TestKBCloseBumpsStatusAndCommits(t *testing.T) {
	root := seedCloseFixture(t, "tele", "dead-end", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "close", "--no-edit", "tele", "dead-end"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed kb tele/dead-end") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "dead-end", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(body), `"status": "closed"`) {
		t.Fatalf("run.json status not flipped:\n%s", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close kb run tele/dead-end") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: dead-end",
		"MoE-Project: tele",
		"MoE-Workflow: kb",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

// TestKBCloseLeavesUnrelatedPathsAlone: kb has no sandbox, so even if
// one exists on disk (e.g., left over from an sdlc run in the same
// project), kb close must not touch it.
func TestKBCloseLeavesUnrelatedPathsAlone(t *testing.T) {
	root := seedCloseFixture(t, "tele", "kb-run", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Sibling sdlc sandbox that must survive the kb close.
	sibling := sandbox.Path(root, "tele", "sdlc-run")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "close", "--no-edit", "tele", "kb-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling sandbox should be untouched: %v", err)
	}
}

// TestKBCloseRefusesTerminal mirrors the sdlc refusal table for kb.
// pushed shouldn't normally happen for kb (no push stage), but if a
// run ends up in that state somehow we still refuse locally rather
// than guess.
func TestKBCloseRefusesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"merged", run.StatusMerged},
		{"closed", run.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "kb-done-"+tc.name, "kb", tc.status)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			beforeHead := gitLog(t, root, "-1", "--format=%H")
			var out, errb bytes.Buffer
			code := Run([]string{"kb", "close", "--no-edit", "tele", "kb-done-" + tc.name}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s, stdout=%q", tc.status, out.String())
			}
			if !strings.Contains(errb.String(), "already") {
				t.Fatalf("refusal should say 'already': %q", errb.String())
			}
			afterHead := gitLog(t, root, "-1", "--format=%H")
			if beforeHead != afterHead {
				t.Fatalf("refused close created a commit")
			}
		})
	}
}

// TestSDLCCloseRegisteredInUsage: the dispatcher's usage listing is
// what an operator discovers via `moe sdlc`; a wiring regression
// should show up here even if the command itself still works.
func TestSDLCCloseRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "close") {
		t.Fatalf("sdlc usage missing 'close':\n%s", out.String())
	}
}

func TestKBCloseRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"kb"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "close") {
		t.Fatalf("kb usage missing 'close':\n%s", out.String())
	}
}

func TestQuickCloseRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"quick"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "close") {
		t.Fatalf("quick usage missing 'close':\n%s", out.String())
	}
}

// TestQuickCloseRemovesSandboxAndCommits parallels
// TestSDLCCloseRemovesSandboxAndCommits: quick code runs in a per-run
// sandbox just like sdlc code, so close must release that workspace
// rather than leave a dead clone on disk. Asserts the same three
// post-conditions: sandbox gone, status flipped, trailered commit on
// HEAD with the quick-flavoured subject.
func TestQuickCloseRemovesSandboxAndCommits(t *testing.T) {
	root := seedCloseFixture(t, "tele", "abandon-quick", "quick", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	sandboxPath := sandbox.Path(root, "tele", "abandon-quick")
	gitDirPath := filepath.Join(root, ".moe", "clones", "tele", ".git-modules", "abandon-quick")
	if err := os.MkdirAll(sandboxPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitDirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"quick", "close", "--no-edit", "tele", "abandon-quick"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed quick tele/abandon-quick") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	if _, err := os.Stat(sandboxPath); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox gone, stat err=%v", err)
	}
	if _, err := os.Stat(gitDirPath); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox gitdir gone, stat err=%v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "abandon-quick", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(body), `"status": "closed"`) {
		t.Fatalf("run.json status not flipped:\n%s", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close quick run tele/abandon-quick") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: abandon-quick",
		"MoE-Project: tele",
		"MoE-Workflow: quick",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}
