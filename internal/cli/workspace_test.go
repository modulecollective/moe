package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/workspace"
)

// seedProjectWithSubmodule extends seedProject with the bits the
// sandbox/workspace path needs: a real submodule on disk, a
// project.json with default_branch set, all committed cleanly so
// run.New's clean-tree precondition passes.
//
// The submodule itself is a tiny seed repo with one commit on main —
// matching push_test.go's pushFixture, scaled down for tests that
// only need `--workspace` to wire end-to-end and don't actually push.
func seedProjectWithSubmodule(t *testing.T, root, projectID string) {
	t.Helper()
	requireGitForCli(t)
	// Bare origin → seed clone → register as submodule under projects/<p>/src.
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
	// -A so bureaucracy.conf (markBureaucracy's marker) and any other
	// pending state ride along — seedProject does the same so run.New's
	// clean-tree precondition passes on the next call.
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "Register project "+projectID)
}

// requireGitForCli mirrors the sandbox/workspace test guard so cli
// tests that need a working git binary skip cleanly elsewhere.
func requireGitForCli(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func TestAttachRunWorkspaceFreshensCommitFreeBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace string
	}{
		{name: "per-run sandbox"},
		{name: "named workspace", workspace: "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedProjectWithSubmodule(t, root, "tele")
			md := run.Metadata{Project: "tele", ID: "fix-it", Workspace: tc.workspace}
			workTree, err := attachRunWorkspace(root, &md, "moe/fix-it")
			if err != nil {
				t.Fatalf("initial attach: %v", err)
			}

			src := filepath.Join(root, "projects", "tele", "src")
			writeFile(t, filepath.Join(src, "fresh.txt"), "fresh\n")
			gittest.Run(t, src, "add", "fresh.txt")
			gittest.Run(t, src, "commit", "-m", "advance main")

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			savedStdout := os.Stdout
			os.Stdout = w
			_, attachErr := attachRunWorkspace(root, &md, "moe/fix-it")
			os.Stdout = savedStdout
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			message, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if attachErr != nil {
				t.Fatalf("reattach: %v", attachErr)
			}
			if got, want := string(message), "freshened moe/fix-it → origin/main (was 1 commits behind)\n"; got != want {
				t.Fatalf("freshen output = %q, want %q", got, want)
			}
			if got, want := gittest.Output(t, workTree, "rev-parse", "HEAD"), gittest.Output(t, src, "rev-parse", "HEAD"); got != want {
				t.Fatalf("workspace HEAD = %s, want source HEAD %s", got, want)
			}
		})
	}
}

func TestAttachRunWorkspaceWarnsAndContinuesOnDirtyFreshen(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	md := run.Metadata{Project: "tele", ID: "fix-it"}
	workTree, err := attachRunWorkspace(root, &md, "moe/fix-it")
	if err != nil {
		t.Fatalf("initial attach: %v", err)
	}
	want := gittest.Output(t, workTree, "rev-parse", "HEAD")

	src := filepath.Join(root, "projects", "tele", "src")
	writeFile(t, filepath.Join(src, "README.md"), "upstream\n")
	gittest.Run(t, src, "commit", "-am", "advance main")
	writeFile(t, filepath.Join(workTree, "README.md"), "local\n")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedStderr := os.Stderr
	os.Stderr = w
	_, attachErr := attachRunWorkspace(root, &md, "moe/fix-it")
	os.Stderr = savedStderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	warning, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if attachErr != nil {
		t.Fatalf("reattach should warn and continue: %v", attachErr)
	}
	for _, wantText := range []string{"warning: freshen moe/fix-it", "local changes"} {
		if !strings.Contains(string(warning), wantText) {
			t.Errorf("warning missing %q: %q", wantText, warning)
		}
	}
	if got := gittest.Output(t, workTree, "rev-parse", "HEAD"); got != want {
		t.Fatalf("workspace HEAD moved from %s to %s", want, got)
	}
}

// TestRunNewWithWorkspaceFlagPersistsToRunJSON confirms the flag
// reaches the on-disk metadata so every later verb (sdlc code, push,
// close, sync, shell) has a single source of truth for "is this run
// using a named workspace?".
func TestRunNewWithWorkspaceFlagPersistsToRunJSON(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"--workspace=dev", "tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "fix-it", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	var md run.Metadata
	if err := json.Unmarshal(body, &md); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	if md.Workspace != "dev" {
		t.Fatalf("Workspace = %q, want %q", md.Workspace, "dev")
	}
}

// TestRunNewWithWorkspaceFlagRefusesIfClaimed exercises the
// pre-flight check: a second run that names the same workspace while
// it's claimed by an in-progress run is refused at sdlc-new time.
// The error names the holder so the operator knows which run to close
// before retrying (or to pick a different workspace name).
func TestRunNewWithWorkspaceFlagRefusesIfClaimed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Plant a claim by a fictional run-a, simulating "run-a opened with
	// --workspace dev and reached its first sdlc code attach."
	if _, err := workspace.Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	var out, errb bytes.Buffer
	code := runNew("sdlc", []string{"--workspace=dev", "tele/fix-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on conflicting claim, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "run tele/run-a") {
		t.Fatalf("expected error to name the holding run, got: %q", errb.String())
	}
}

// TestRunNewWithWorkspaceFlagRejectedOnNonSdlc confirms the flag is
// gated to sdlc — the kb / idea workflows have no code stage
// to use a workspace.
func TestRunNewWithWorkspaceFlagRejectedOnNonSdlc(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := runNew("kb", []string{"--workspace=dev", "tele/dns-basics"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on --workspace with kb, got 0; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "--workspace") {
		t.Fatalf("expected error to name the flag, got: %q", errb.String())
	}
}

// TestShellRunWorkspaceLandsInClonePath confirms the shell verb resolves
// the run's workspace path correctly. Stubs $SHELL with a script that
// writes its cwd to a known file, then asserts the recorded path
// matches sandbox.Path's expectation. Skipped on non-unix targets so
// the script bits don't grow a Windows fork.
func TestShellRunWorkspaceLandsInClonePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses POSIX shell semantics")
	}
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele/fix-it"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("seed run failed")
	}
	// Pre-create the sandbox so the shell verb finds something on disk
	// (the test doesn't run sdlc code, which would otherwise create it).
	mdPath := filepath.Join(root, "projects", "tele", "runs", "fix-it", "run.json")
	body, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	var md run.Metadata
	if err := json.Unmarshal(body, &md); err != nil {
		t.Fatal(err)
	}
	if _, err := attachRunWorkspace(root, &md, "moe/fix-it"); err != nil {
		t.Fatalf("attach sandbox: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(filepath.Join(root, ".moe", "clones", "tele", "fix-it"))
	if err != nil {
		t.Fatal(err)
	}

	cwdLog, stubShell := writeShellStub(t)
	t.Setenv("SHELL", stubShell)

	var out, errb bytes.Buffer
	if code := runShell([]string{"tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("shell: exit=%d stderr=%q", code, errb.String())
	}
	got, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatalf("cwd log missing: %v", err)
	}
	gotResolved, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if gotResolved != wantPath {
		t.Fatalf("shell cwd = %s, want %s", gotResolved, wantPath)
	}
}

func TestShellRunWorkspaceRefusesStaleDevEnvCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses POSIX shell semantics")
	}
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	if code := runNew("sdlc", []string{"tele/fix-it"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("seed run failed")
	}
	mdPath := filepath.Join(root, "projects", "tele", "runs", "fix-it", "run.json")
	body, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	var md run.Metadata
	if err := json.Unmarshal(body, &md); err != nil {
		t.Fatal(err)
	}
	wp, err := attachRunWorkspace(root, &md, "moe/fix-it")
	if err != nil {
		t.Fatalf("attach sandbox: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "vanished")
	if err := writeDevEnvCache(filepath.Join(wp, devEnvCacheRel), map[string]string{"MOE_HOME": missing}); err != nil {
		t.Fatal(err)
	}

	cwdLog, stubShell := writeShellStub(t)
	t.Setenv("SHELL", stubShell)
	var out, errb bytes.Buffer
	if code := runShell([]string{"tele/fix-it"}, &out, &errb); code == 0 {
		t.Fatalf("stale shell cache should fail, stdout=%q", out.String())
	}
	for _, want := range []string{"MOE_HOME", missing, "reopen a sandbox-backed stage"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("error missing %q: %q", want, errb.String())
		}
	}
	if _, err := os.Stat(cwdLog); !os.IsNotExist(err) {
		t.Fatalf("shell stub was invoked: %v", err)
	}
}

// TestShellNamedWorkspaceCreatesLazily covers the standalone form.
// Without any run involved, `moe workspace shell tele dev` materialises
// the workspace dir on first call and lands the shell in it. Promoted
// from the previous `moe sdlc shell --workspace dev` flag form;
// workspaces aren't sdlc-specific, so the verb sits with the rest of
// the workspace admin verbs.
func TestShellNamedWorkspaceCreatesLazily(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses POSIX shell semantics")
	}
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	cwdLog, stubShell := writeShellStub(t)
	t.Setenv("SHELL", stubShell)

	var out, errb bytes.Buffer
	if code := runWorkspaceShell([]string{"tele/dev"}, &out, &errb); code != 0 {
		t.Fatalf("shell: exit=%d stderr=%q", code, errb.String())
	}
	wantPath, err := filepath.EvalSymlinks(filepath.Join(root, ".moe", "named", "tele", "dev"))
	if err != nil {
		t.Fatalf("workspace dir not created: %v", err)
	}
	got, err := os.ReadFile(cwdLog)
	if err != nil {
		t.Fatalf("cwd log missing: %v", err)
	}
	gotResolved, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if gotResolved != wantPath {
		t.Fatalf("shell cwd = %s, want %s", gotResolved, wantPath)
	}
	if !strings.Contains(out.String(), "unclaimed") {
		t.Fatalf("expected unclaimed marker in stdout, got: %q", out.String())
	}
}

func TestShellNamedWorkspaceRefusesStaleDevEnvCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub uses POSIX shell semantics")
	}
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	wp, err := workspace.Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "vanished")
	if err := writeDevEnvCache(filepath.Join(wp, devEnvCacheRel), map[string]string{"MOE_DEV_TMPDIR": missing}); err != nil {
		t.Fatal(err)
	}
	cwdLog, stubShell := writeShellStub(t)
	t.Setenv("SHELL", stubShell)

	var out, errb bytes.Buffer
	if code := runWorkspaceShell([]string{"tele/dev"}, &out, &errb); code == 0 {
		t.Fatalf("stale workspace cache should fail, stdout=%q", out.String())
	}
	for _, want := range []string{"MOE_DEV_TMPDIR", missing, "moe workspace refresh tele/dev"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("error missing %q: %q", want, errb.String())
		}
	}
	if _, err := os.Stat(cwdLog); !os.IsNotExist(err) {
		t.Fatalf("shell stub was invoked: %v", err)
	}
}

// TestSdlcShellRejectsWorkspaceFlag pins the removal: the
// `--workspace` flag form on `moe sdlc shell` is gone — workspaces
// aren't sdlc-specific, so `moe workspace shell` owns that shape now.
// The flag should parse-error rather than silently no-op.
func TestSdlcShellRejectsWorkspaceFlag(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runShell([]string{"--workspace=dev", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on --workspace under sdlc shell; stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// writeShellStub drops a tiny POSIX shell script under t.TempDir that
// writes its $PWD to a sibling log file. Returns (logPath, scriptPath).
// Used by the shell-verb tests to verify cwd routing without launching
// a real interactive shell.
func writeShellStub(t *testing.T) (logPath, scriptPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "cwd.txt")
	scriptPath = filepath.Join(dir, "shell-stub")
	body := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" > " + logPath + "\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath, scriptPath
}
