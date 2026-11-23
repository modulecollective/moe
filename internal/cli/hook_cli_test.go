package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// TestHookFireRegistered: `moe hook` lists `fire` as a subcommand so a
// init-ordering drift in hook_cli.go would surface here, not at first
// invocation.
func TestHookFireRegistered(t *testing.T) {
	g, err := LookupGroup("hook")
	if err != nil {
		t.Fatal(err)
	}
	if g.Lookup("fire") == nil {
		t.Fatal("hook group missing `fire` subcommand")
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"hook"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "fire") {
		t.Fatalf("hook usage missing `fire`: %q", out.String())
	}
}

// TestHookFireDevEnvDumpsMergedEnv: dispatchHookFire("dev-env") runs
// dev-env.d/* and dumps the merged KEY=VALUE on stdout. Mirrors the
// "smoke test" the design names — edit a script, fire it, see the
// new env come back.
func TestHookFireDevEnvDumpsMergedEnv(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "DATABASE_URL=postgres://localhost/fire"
echo "PORT=9999"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-seed.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sandboxPath := t.TempDir()
	md := &run.Metadata{ID: "hook-fire-123", Project: projID, Workflow: hookFireWorkflow}

	var stdout, stderr bytes.Buffer
	code := dispatchHookFire(root, sandboxPath, &project.Metadata{ID: projID}, md, "dev-env", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"DATABASE_URL=postgres://localhost/fire",
		"PORT=9999",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

// TestHookFireDevEnvTeardownSourcesSetupEnv: dispatchHookFire
// ("dev-env-teardown") runs setup first (in-memory), then teardown
// sees those vars as exported. Pins the "fire dev-env first to
// populate the env teardown expects" branch from the design.
func TestHookFireDevEnvTeardownSourcesSetupEnv(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	setupDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	teardownDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setup := `#!/bin/sh
echo "MOE_DEV_TMPDIR=/tmp/fire"
`
	if err := os.WriteFile(filepath.Join(setupDir, "10-tmp.sh"), []byte(setup), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt")
	teardown := `#!/bin/sh
echo "saw MOE_DEV_TMPDIR=$MOE_DEV_TMPDIR" > ` + receipt + `
`
	if err := os.WriteFile(filepath.Join(teardownDir, "10-cleanup.sh"), []byte(teardown), 0o755); err != nil {
		t.Fatal(err)
	}

	md := &run.Metadata{ID: "hook-fire-99", Project: projID, Workflow: hookFireWorkflow}
	code := dispatchHookFire(root, t.TempDir(), &project.Metadata{ID: projID}, md, "dev-env-teardown", io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	body, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "saw MOE_DEV_TMPDIR=/tmp/fire") {
		t.Fatalf("receipt missing exported var:\n%s", string(body))
	}
}

// TestHookFirePrePushRunsProjectScripts: dispatchHookFire("pre-push")
// runs pre-push.d/* against the sandbox. The script sees MOE_SANDBOX
// pointing at the worktree the operator is iterating against.
func TestHookFirePrePushRunsProjectScripts(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	prePushDir := filepath.Join(root, project.Dir(projID), "hooks", "pre-push.d")
	if err := os.MkdirAll(prePushDir, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt")
	script := `#!/bin/sh
echo "ran pre-push in MOE_SANDBOX=$MOE_SANDBOX" > ` + receipt + `
`
	if err := os.WriteFile(filepath.Join(prePushDir, "10-check.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sandboxPath := t.TempDir()
	md := &run.Metadata{ID: "hook-fire-7", Project: projID, Workflow: hookFireWorkflow}
	pj := &project.Metadata{ID: projID, DefaultBranch: "main"}

	code := dispatchHookFire(root, sandboxPath, pj, md, "pre-push", io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	body, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "MOE_SANDBOX="+sandboxPath) {
		t.Fatalf("receipt missing sandbox path:\n%s", string(body))
	}
}

// prePushFireFixture lays out a project whose dev-env.d emits
// devEnvOut, whose pre-push.d runs prePush, and whose
// dev-env-teardown.d records $GEN to the returned teardown receipt.
func prePushFireFixture(t *testing.T, devEnvOut, prePush string) (root, sandboxPath, teardownReceipt string, md *run.Metadata, pj *project.Metadata) {
	t.Helper()
	root = t.TempDir()
	projID := "tele"
	teardownReceipt = filepath.Join(t.TempDir(), "teardown")
	writeHookScript(t, root, projID, "dev-env", "10-env.sh", devEnvOut)
	writeHookScript(t, root, projID, "pre-push", "10-check.sh", prePush)
	writeHookScript(t, root, projID, "dev-env-teardown", "10-down.sh",
		"#!/bin/sh\nprintf '%s' \"$GEN\" > "+teardownReceipt+"\n")
	md = &run.Metadata{ID: "hook-fire-7", Project: projID, Workflow: hookFireWorkflow}
	pj = &project.Metadata{ID: projID, DefaultBranch: "main"}
	return root, t.TempDir(), teardownReceipt, md, pj
}

// TestHookFirePrePushSeesDevEnv: a pre-push script under fire runs with
// the env dev-env.d emitted, MOE_* still wins over a dev-env script
// that tries to redirect it, no cache file lands in the transient
// sandbox, and teardown runs with the same env afterwards.
func TestHookFirePrePushSeesDevEnv(t *testing.T) {
	receipt := filepath.Join(t.TempDir(), "receipt")
	root, sandboxPath, teardown, md, pj := prePushFireFixture(t,
		"#!/bin/sh\nprintf 'GEN=fire\\nMOE_SANDBOX=/bogus\\n'\n",
		"#!/bin/sh\nprintf '%s|%s' \"$GEN\" \"$MOE_SANDBOX\" > "+receipt+"\n")

	var stdout, stderr bytes.Buffer
	if code := dispatchHookFire(root, sandboxPath, pj, md, "pre-push", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	body, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "fire|"+sandboxPath; got != want {
		t.Fatalf("pre-push receipt = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(sandboxPath, ".moe", "dev-env.env")); !os.IsNotExist(err) {
		t.Fatalf("hook fire wrote a dev-env cache into the sandbox: %v", err)
	}
	if got, err := os.ReadFile(teardown); err != nil || string(got) != "fire" {
		t.Fatalf("teardown receipt = %q, %v; want %q", got, err, "fire")
	}
}

// TestHookFirePrePushTearsDownOnScriptFailure: a failing pre-push
// script still exits 1, and teardown still runs.
func TestHookFirePrePushTearsDownOnScriptFailure(t *testing.T) {
	root, sandboxPath, teardown, md, pj := prePushFireFixture(t,
		"#!/bin/sh\nprintf 'GEN=fire\\n'\n",
		"#!/bin/sh\nexit 3\n")

	if code := dispatchHookFire(root, sandboxPath, pj, md, "pre-push", io.Discard, io.Discard); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if got, err := os.ReadFile(teardown); err != nil || string(got) != "fire" {
		t.Fatalf("teardown receipt = %q, %v; want %q", got, err, "fire")
	}
}

// TestHookFirePrePushSetupFailureStops: a failing dev-env.d script
// exits 1 before any pre-push script runs.
func TestHookFirePrePushSetupFailureStops(t *testing.T) {
	receipt := filepath.Join(t.TempDir(), "receipt")
	root, sandboxPath, _, md, pj := prePushFireFixture(t,
		"#!/bin/sh\nexit 7\n",
		"#!/bin/sh\ntouch "+receipt+"\n")

	if code := dispatchHookFire(root, sandboxPath, pj, md, "pre-push", io.Discard, io.Discard); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("pre-push script ran after dev-env setup failed: %v", err)
	}
}

func TestHookFireRollsBackPartialSetup(t *testing.T) {
	for _, event := range hookFireEvents {
		t.Run(event, func(t *testing.T) {
			root, workTree, md := devEnvRollbackFixture(t)
			md.Workflow = hookFireWorkflow
			writeHookScript(t, root, md.Project, "dev-env", "20-fail.sh", "#!/bin/sh\necho setup-failed >&2\nexit 7\n")
			writeHookScript(t, root, md.Project, "pre-push", "10-check.sh", "#!/bin/sh\ntouch \"$MOE_SANDBOX/pre-push-ran\"\n")
			var stdout, stderr bytes.Buffer
			if code := dispatchHookFire(root, workTree, &project.Metadata{ID: md.Project}, md, event, &stdout, &stderr); code != 1 {
				t.Fatalf("exit=%d, want 1; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "setup-failed") || !strings.Contains(stderr.String(), "exit status 7") {
				t.Fatalf("missing setup failure: %s", stderr.String())
			}
			for _, name := range []string{"resource", "later", "pre-push-ran", devEnvCacheRel} {
				if _, err := os.Stat(filepath.Join(workTree, name)); !os.IsNotExist(err) {
					t.Errorf("unexpected residue %s: %v", name, err)
				}
			}
			want := "down:" + filepath.Join(workTree, "resource") + "\nafter\n"
			if trace, err := os.ReadFile(filepath.Join(workTree, "trace")); err != nil || string(trace) != want {
				t.Fatalf("teardown did not run exactly once: %q, %v; want %q", trace, err, want)
			}
			if got, err := os.ReadFile(filepath.Join(workTree, "sentinel")); err != nil || string(got) != "keep" {
				t.Fatalf("unrelated sentinel changed: %q, %v", got, err)
			}
		})
	}
}

// TestHookFireUnknownEvent: a typo for the event name fails with exit
// code 2 and lists the known events.
func TestHookFireUnknownEvent(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "hook-fire-1", Project: "tele", Workflow: hookFireWorkflow}
	var stdout, stderr bytes.Buffer
	code := dispatchHookFire(root, t.TempDir(), &project.Metadata{}, md, "bogus", &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown event") {
		t.Fatalf("stderr missing 'unknown event': %q", stderr.String())
	}
}

// TestHookFireRejectsUnknownEventBeforeMint checks both scratch and legacy
// namespaces: a typo must return before either cleanup or allocation.
func TestHookFireRejectsUnknownEventBeforeMint(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(root, "projects", projID)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"id":"tele","status":"incubating","submodule":"projects/tele/src","remote":"git@example.com:x/tele.git","default_branch":"main"}`
	if err := os.WriteFile(filepath.Join(projDir, "project.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_HOME", root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook", "fire", projID, "bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit=%d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown event "bogus"`) {
		t.Errorf("stderr missing `unknown event \"bogus\"`: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "dev-env, dev-env-teardown, pre-push") {
		t.Errorf("stderr missing known-events list: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "sandbox:") {
		t.Errorf("stdout printed sandbox banner after typo: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".moe", "hook-fire")); !os.IsNotExist(err) {
		t.Fatalf("scratch namespace created despite typo: %v", err)
	}
	clonesDir := filepath.Join(root, ".moe", "clones", projID)
	entries, err := os.ReadDir(clonesDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), hookFirePrefix) {
			t.Errorf("hook-fire sandbox %s minted despite typo'd event", e.Name())
		}
	}
}

// TestHookFireCwdWinsOverMoeHome pins proposal #1 of hook-dev-cleanup:
// `moe hook fire` walks up from cwd first and ignores $MOE_HOME when
// the two disagree. The operator's in-flight edits live on the cwd
// worktree, so the verb has to read them — not the pinned home tree.
// On disagreement, the verb prints a one-line note on stderr so the
// new behaviour is self-documenting on first hit.
func TestHookFireCwdWinsOverMoeHome(t *testing.T) {
	cwdRoot := t.TempDir()
	homeRoot := t.TempDir()
	for _, root := range []string{cwdRoot, homeRoot} {
		if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projID := "tele"
	projDir := filepath.Join(cwdRoot, "projects", projID)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projJSON := `{"id":"tele","status":"incubating","submodule":"projects/tele/src","remote":"git@example.com:x/tele.git","default_branch":"main"}`
	if err := os.WriteFile(filepath.Join(projDir, "project.json"), []byte(projJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant the edit-under-test on the cwd-root: a dev-env.d script
	// that emits a sentinel. If hook fire read the home tree, the
	// sentinel wouldn't surface on stdout.
	hookDir := filepath.Join(cwdRoot, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "CWD_SENTINEL=found"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-seed.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	gittest.SetupEnv(t)
	src := filepath.Join(projDir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(src, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(src, "commit", "--allow-empty", "-m", "init"); err != nil {
		t.Fatal(err)
	}

	// chdir into cwdRoot for the duration of the test so os.Getwd
	// resolves there. The runner caller's cwd is what FindCwdFirst
	// walks from.
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	if err := os.Chdir(cwdRoot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_HOME", homeRoot)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"hook", "fire", projID, "dev-env"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "CWD_SENTINEL=found") {
		t.Errorf("stdout missing cwd sentinel — fire read the wrong tree:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "hook fire: using cwd bureaucracy") {
		t.Errorf("stderr missing cwd-wins nag:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "MOE_HOME") {
		t.Errorf("stderr nag should name $MOE_HOME:\n%s", stderr.String())
	}
}
