//go:build linux

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func ownedEnvFixture(t *testing.T) (*runowner.Handle, *run.Metadata, string) {
	t.Helper()
	root := newDevEnvTestRoot(t)
	md := &run.Metadata{Project: "tele", ID: "verify", Workflow: "sdlc", Status: run.StatusInProgress}
	path := sandbox.Path(root, md.Project, md.ID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, path)
	gittest.Commit(t, path, "source seed")
	h, err := runowner.Acquire(root, md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release() })
	r, err := clonereclaim.Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "sibling"), "outer resource must survive\n")
	t.Setenv("TEST_OWNED_PARENT", parent)
	t.Setenv("OWNED_ROOT", filepath.Join(parent, "sibling"))
	t.Setenv("MOE_HOME", filepath.Join(parent, "sibling"))
	t.Setenv("MOE_DEV_TMPDIR", filepath.Join(parent, "sibling"))
	writeHookScript(t, root, md.Project, "dev-env", "10-own.sh", `#!/bin/sh
set -eu
owned=$(mktemp -d "$TEST_OWNED_PARENT/owned.XXXXXX")
printf 'create\n' >> "$TEST_OWNED_PARENT/setup"
printf 'OWNED_ROOT=%s\nMOE_DEV_TMPDIR=%s\nOWNED_TRACE=%s\n' "$owned" "$owned" "$TEST_OWNED_PARENT/cleanup"
`)
	writeHookScript(t, root, md.Project, "dev-env-teardown", "10-down.sh", ownedCleanupScript)
	return h, md, parent
}

const ownedCleanupScript = `#!/bin/sh
set -eu
test -d "$MOE_SANDBOX"
test -f "$MOE_SANDBOX/.moe/dev-env.env"
test -z "$MOE_HOME"
printf '%s\n' "$OWNED_ROOT" >> "$OWNED_TRACE"
if [ -d "$OWNED_ROOT" ]; then rm -r -- "$OWNED_ROOT"; fi
if [ -f "$TEST_OWNED_PARENT/fail" ]; then exit 13; fi
`

func TestOwnedDevEnvSetupRefreshAndTeardownUseOriginalAddresses(t *testing.T) {
	h, md, parent := ownedEnvFixture(t)
	env, fresh, err := devEnvSetupOwned(h, md, io.Discard, io.Discard)
	if err != nil || !fresh {
		t.Fatalf("setup: %v %v", fresh, err)
	}
	first := env["OWNED_ROOT"]
	if !strings.HasPrefix(first, parent+"/owned.") {
		t.Fatal("setup did not allocate fixture resource")
	}
	if _, fresh, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err != nil || fresh {
		t.Fatalf("cache hit: %v %v", fresh, err)
	}
	// A vanished managed directory triggers the actual CLI cache-refresh path.
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	env, fresh, err = devEnvSetupOwned(h, md, io.Discard, io.Discard)
	if err != nil || !fresh || env["OWNED_ROOT"] == first {
		t.Fatalf("refresh: %v %v", fresh, err)
	}
	second := env["OWNED_ROOT"]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := devEnvTeardownOwned(ctx, h, md, "terminal", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("owned resource survived successful cleanup")
	}
	if b, err := os.ReadFile(filepath.Join(parent, "sibling")); err != nil || string(b) != "outer resource must survive\n" {
		t.Fatalf("ambient sentinel changed: %q %v", b, err)
	}
	trace, err := os.ReadFile(filepath.Join(parent, "cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	if string(trace) != first+"\n"+second+"\n" {
		t.Fatalf("wrong cleanup addresses: %q", trace)
	}
	if err := devEnvTeardownOwned(ctx, h, md, "terminal", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	traceAgain, err := os.ReadFile(filepath.Join(parent, "cleanup"))
	if err != nil || !bytes.Equal(trace, traceAgain) {
		t.Fatal("completed cleanup replayed")
	}
	if _, err := os.Stat(sandbox.Path(h.Root, md.Project, md.ID)); err != nil {
		t.Fatal("environment cleanup removed clone")
	}
}

func TestOwnedDevEnvFailedTeardownBlocksSetupAndRetriesSavedSnapshot(t *testing.T) {
	h, md, parent := ownedEnvFixture(t)
	env, _, err := devEnvSetupOwned(h, md, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(sandbox.Path(h.Root, md.Project, md.ID), devEnvCacheRel)
	original, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(parent, "fail"), "fail after partial cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := devEnvTeardownOwned(ctx, h, md, "refresh", false, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "exited 13") {
		t.Fatalf("failure not reported: %v", err)
	}
	if _, _, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err == nil {
		t.Fatal("stage overwrote pending cleanup")
	}
	if b, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(b, original) {
		t.Fatal("failed teardown lost cache")
	}
	if err := os.Remove(filepath.Join(parent, "fail")); err != nil {
		t.Fatal(err)
	}
	if err := devEnvTeardownOwned(ctx, h, md, "refresh", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	trace, err := os.ReadFile(filepath.Join(parent, "cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	if string(trace) != strings.Repeat(env["OWNED_ROOT"]+"\n", 2) {
		t.Fatalf("retry substituted addresses: %q", trace)
	}
	if _, err := os.Stat(filepath.Join(parent, "sibling")); err != nil {
		t.Fatal("ambient resource was touched")
	}
	if _, fresh, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err != nil || !fresh {
		t.Fatalf("completed cleanup did not permit fresh setup: %v %v", fresh, err)
	}
}

func TestOwnedDevEnvChangedHookAndMissingContractRefuse(t *testing.T) {
	h, md, parent := ownedEnvFixture(t)
	if _, _, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(h.Root, "projects", md.Project, "hooks", devEnvTeardownDirRel, "10-down.sh")
	writeHookScript(t, h.Root, md.Project, "dev-env-teardown", "10-down.sh", "#!/bin/sh\nexit 0\n")
	if err := devEnvTeardownOwned(context.Background(), h, md, "terminal", false, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "missing or changed") {
		t.Fatalf("changed hook accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "cleanup")); !os.IsNotExist(err) {
		t.Fatal("changed hook cleanup ran")
	}
	if err := os.WriteFile(script, []byte(ownedCleanupScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := devEnvTeardownOwned(context.Background(), h, md, "terminal", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if _, _, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err == nil {
		t.Fatal("setup without teardown contract allocated")
	}
}
