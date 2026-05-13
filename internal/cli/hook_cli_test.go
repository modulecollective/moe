package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestCleanPriorHookFireSandboxesStrictPrefix: prior hook-fire-*
// directories get removed; a real-run sandbox (slug without the
// hook-fire- prefix) stays put. Load-bearing safety check — without
// the strict prefix match, this verb could wipe an in-flight run's
// working tree.
func TestCleanPriorHookFireSandboxesStrictPrefix(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	base := filepath.Join(root, ".moe", "clones", projID)
	dirs := []string{
		"hook-fire-1700000000",
		"hook-fire-1700000001",
		"my-real-run",
		"another-real-run-2026-05-13",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanPriorHookFireSandboxes(root, projID, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"hook-fire-1700000000", "hook-fire-1700000001"} {
		if _, err := os.Stat(filepath.Join(base, d)); !os.IsNotExist(err) {
			t.Errorf("%s still present after clean (err=%v)", d, err)
		}
	}
	for _, d := range []string{"my-real-run", "another-real-run-2026-05-13"} {
		if _, err := os.Stat(filepath.Join(base, d)); err != nil {
			t.Errorf("real-run sandbox %s was removed: %v", d, err)
		}
	}
}

// TestCleanPriorHookFireSandboxesMissingDir: a project that has never
// had a fire (no .moe/clones/<p>/ dir yet) is a clean no-op.
func TestCleanPriorHookFireSandboxesMissingDir(t *testing.T) {
	root := t.TempDir()
	if err := cleanPriorHookFireSandboxes(root, "never-fired", io.Discard); err != nil {
		t.Fatalf("missing dir should be a no-op, got: %v", err)
	}
}
