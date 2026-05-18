package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/workspace"
)

// TestWorkspaceGroupRegistersFullCRUD asserts the verb group's
// dispatch table carries the rounded-out CRUD shape: new, list,
// remove, release, refresh. A regression here means a verb fell off
// the registration and won't be reachable from `moe workspace ...`.
func TestWorkspaceGroupRegistersFullCRUD(t *testing.T) {
	cmd, ok := commands["workspace"]
	if !ok {
		t.Fatal(`expected top-level command "workspace" to be registered`)
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "list", "remove", "release", "refresh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("workspace usage missing subcommand %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "dev-env-refresh") {
		t.Fatalf("workspace usage still lists dev-env-refresh: %q", out.String())
	}
}

// TestWorkspaceNewIsIdempotent calls `workspace new` twice and
// confirms the second call says the workspace already exists rather
// than failing or re-cloning.
func TestWorkspaceNewIsIdempotent(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "new", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("first new: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "created") {
		t.Fatalf("first new should say created: %q", out.String())
	}
	if !workspace.Exists(root, "tele", "dev") {
		t.Fatal("workspace dir missing after `new`")
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"workspace", "new", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("second new: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Fatalf("second new should report already exists: %q", out.String())
	}
}

// TestWorkspaceListPrintsRows seeds a workspace, claims it, drops a
// dirty file, and asserts the table contains the expected columns
// and dirty marker.
func TestWorkspaceListPrintsRows(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	wp, err := workspace.Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wp, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "list"}, &out, &errb); code != 0 {
		t.Fatalf("list: exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"PROJECT", "NAME", "BRANCH", "CLAIM", "DIRTY", "DEV-ENV", "tele", "dev", "tele/run-a", "*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list output missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "tele/dev") {
		t.Fatalf("list output should not pre-join project/name into one cell: %q", got)
	}
}

// TestWorkspaceListEmptyIsSilent confirms zero workspaces prints
// nothing and exits 0 — same posture project list takes for
// empty state.
func TestWorkspaceListEmptyIsSilent(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "list"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "" {
		t.Fatalf("expected empty stdout, got %q", out.String())
	}
}

// TestWorkspaceRemoveRefusesClaimed: the verb cannot delete a
// workspace held by a run. The operator's recovery is to close the
// run (or run `workspace release`) first.
func TestWorkspaceRemoveRefusesClaimed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "remove", "tele", "dev"}, &out, &errb); code == 0 {
		t.Fatalf("expected non-zero on claimed workspace, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "tele/run-a") {
		t.Fatalf("error should name the holding run, got: %q", errb.String())
	}
	if !workspace.Exists(root, "tele", "dev") {
		t.Fatal("workspace dir should survive a refused remove")
	}
}

// TestWorkspaceRemoveDeletesAndRunsTeardown confirms that a
// teardown script under dev-env-teardown.d/* fires and the
// directory is gone after `workspace remove`.
func TestWorkspaceRemoveDeletesAndRunsTeardown(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Ensure(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}
	wp := workspace.Path(root, "tele", "dev")
	// Seed a cached env so teardown has something to load.
	if err := writeDevEnvCache(filepath.Join(wp, devEnvCacheRel),
		map[string]string{"DATABASE_URL": "postgres://localhost/x"}); err != nil {
		t.Fatal(err)
	}
	// Drop a teardown script that records the env it saw.
	receipt := filepath.Join(t.TempDir(), "receipt")
	teardownDir := filepath.Join(root, project.Dir("tele"), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"DB=$DATABASE_URL\" > " + receipt + "\n"
	if err := os.WriteFile(filepath.Join(teardownDir, "10-cleanup.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "remove", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("remove: exit=%d stderr=%q", code, errb.String())
	}
	if workspace.Exists(root, "tele", "dev") {
		t.Fatal("workspace dir should be gone after remove")
	}
	got, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatalf("teardown receipt missing: %v", err)
	}
	if string(got) != "DB=postgres://localhost/x\n" {
		t.Fatalf("teardown env = %q, want DB=postgres://localhost/x", string(got))
	}
}

// TestWorkspaceRemoveMissingIsNoop: a missing workspace prints a
// short note and exits 0 — same idempotent shape as sandbox.Remove.
func TestWorkspaceRemoveMissingIsNoop(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "remove", "tele", "ghost"}, &out, &errb); code != 0 {
		t.Fatalf("expected exit 0 on missing workspace, got %d (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Fatalf("expected does-not-exist note, got %q", out.String())
	}
}

// TestWorkspaceReleaseNamesPriorHolder confirms `workspace release`
// clears claim.json and reports who used to hold it.
func TestWorkspaceReleaseNamesPriorHolder(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "release", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("release: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "tele/run-a") {
		t.Fatalf("release should name prior holder, got %q", out.String())
	}
	c, err := workspace.ReadClaim(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatalf("expected claim cleared, got %+v", c)
	}
}

// TestWorkspaceReleaseUnclaimedIsClear: a release on an unclaimed
// workspace prints a "no claim" line and exits 0.
func TestWorkspaceReleaseUnclaimedIsClear(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Ensure(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "release", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("release: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "no claim") {
		t.Fatalf("expected no-claim line, got %q", out.String())
	}
}

// TestWorkspaceRefreshRebuildsCacheEagerly drops a dev-env.d setup
// script that emits a key, runs `workspace refresh`, and confirms
// the cache appears on disk with the script's output — proving the
// rename is more than cosmetic and setup now runs eagerly rather than
// waiting for the next stage open.
func TestWorkspaceRefreshRebuildsCacheEagerly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Ensure(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}
	wp := workspace.Path(root, "tele", "dev")
	cache := filepath.Join(wp, devEnvCacheRel)

	// Drop a dev-env.d script that emits a KEY=VALUE line.
	setupDir := filepath.Join(root, project.Dir("tele"), "hooks", devEnvDirRel)
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setupDir, "10-emit.sh"),
		[]byte("#!/bin/sh\necho REFRESHED=yes\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"workspace", "refresh", "tele", "dev"}, &out, &errb); code != 0 {
		t.Fatalf("refresh: exit=%d stderr=%q", code, errb.String())
	}
	body, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("expected cache rebuilt eagerly, missing: %v", err)
	}
	if !strings.Contains(string(body), "REFRESHED=yes") {
		t.Fatalf("cache = %q, expected REFRESHED=yes", body)
	}
}

// TestProjectRemoveRefusesWithWorkspaces: project removal must refuse
// while a named workspace still exists, naming the workspace and
// pointing at `moe workspace remove`.
func TestProjectRemoveRefusesWithWorkspaces(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := workspace.Ensure(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"project", "remove", "tele"}, &out, &errb); code == 0 {
		t.Fatalf("expected refusal, stdout=%q stderr=%q", out.String(), errb.String())
	}
	for _, want := range []string{"dev", "moe workspace remove"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("error should mention %q, got: %q", want, errb.String())
		}
	}
}
