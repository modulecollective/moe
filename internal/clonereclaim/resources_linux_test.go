//go:build linux

package clonereclaim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/runowner"
)

type resourceFixture struct {
	h                  *runowner.Handle
	r                  *Registry
	cache              []byte
	scripts            []Script
	env                []string
	owned, trace, fail string
}

func ownedResourceFixture(t *testing.T, body string) resourceFixture {
	t.Helper()
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	parent := t.TempDir()
	f := resourceFixture{h: h, r: r, owned: filepath.Join(parent, "owned"), trace: filepath.Join(parent, "trace"), fail: filepath.Join(parent, "fail")}
	put(t, f.owned, "fixture resource")
	if body == "" {
		body = `#!/bin/sh
set -eu
printf 'run\n' >> "$TRACE"
if [ -f "$OWNED" ]; then rm -- "$OWNED"; fi
if [ -f "$FAIL" ]; then exit 9; fi
`
	}
	rel := filepath.Join("projects", "p", "hooks", "dev-env-teardown.d", "10-owned.sh")
	put(t, filepath.Join(h.Root, rel), body)
	if err := os.Chmod(filepath.Join(h.Root, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	f.scripts, err = ScriptIdentities(h.Root, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	f.cache = []byte("# moe-dev-env-v1 p/r none\nOWNED=" + f.owned + "\nTRACE=" + f.trace + "\nFAIL=" + f.fail + "\n")
	put(t, filepath.Join(path, ".moe", cacheName), string(f.cache))
	f.env = append(os.Environ(), "OWNED="+f.owned, "TRACE="+f.trace, "FAIL="+f.fail)
	if _, err := r.RecordResources(h, f.cache, nil, f.scripts, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f resourceFixture) plan(t *testing.T, retry bool) CleanupPlan {
	t.Helper()
	p, err := f.r.PlanCleanup(f.h, "refresh", f.scripts, retry)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f resourceFixture) count(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(f.trace)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run\n")
}

func TestCleanupSavedOwnershipFailureAndRetry(t *testing.T) {
	f := ownedResourceFixture(t, "")
	if err := f.r.ValidateEnvironment(f.h); err != nil {
		t.Fatal(err)
	}
	put(t, f.fail, "fail first attempt")
	plan := f.plan(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.r.ExecuteCleanup(ctx, f.h, plan, f.env, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "exited 9") {
		t.Fatalf("nonzero cleanup: %v", err)
	}
	if f.count(t) != 1 {
		t.Fatal("hook did not run exactly once")
	}
	cache, exists, err := f.r.CacheBytes(f.h)
	if err != nil || !exists || !bytes.Equal(cache, f.cache) {
		t.Fatal("failure lost original cache")
	}
	if err := f.r.ValidateEnvironment(f.h); err == nil {
		t.Fatal("failed cleanup admitted a cache hit")
	}
	if _, err := f.r.PlanCleanup(f.h, "refresh", f.scripts, false); err == nil {
		t.Fatal("ordinary stage retried failed cleanup")
	}
	if err := os.Remove(f.fail); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ExecuteCleanup(ctx, f.h, f.plan(t, true), f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := f.r.CacheBytes(f.h); err != nil || exists {
		t.Fatal("successful cleanup retained reusable cache")
	}
	if f.count(t) != 2 {
		t.Fatal("retry did not run exactly once")
	}
	if err := f.r.ValidateEnvironment(f.h); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ExecuteCleanup(ctx, f.h, f.plan(t, true), f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count(t) != 2 {
		t.Fatal("completed cleanup ran again")
	}
	history, err := f.r.resources()
	if err != nil || len(history) != 1 || !bytes.Equal(history[0].Cache, f.cache) {
		t.Fatal("original addresses were not retained")
	}
	// A new cache can append only after proved cleanup of the old snapshot.
	put(t, filepath.Join(f.r.Identity.Path, ".moe", cacheName), string(f.cache))
	if _, err := f.r.RecordResources(f.h, f.cache, nil, f.scripts, false); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ClearCompletedCache(f.h, plan); err == nil {
		t.Fatal("stale plan could clear new ownership")
	}
}

func TestCleanupCacheClearFailureNeverRerunsHooks(t *testing.T) {
	for _, phase := range []string{"directory-sync", "receipt-sync"} {
		t.Run(phase, func(t *testing.T) {
			f := ownedResourceFixture(t, "")
			plan := f.plan(t, false)
			original := syncRecordFile
			defer func() { syncRecordFile = original }()
			injected := errors.New("injected cache-clear sync failure")
			syncRecordFile = func(file *os.File) error {
				if phase == "directory-sync" && file.Name() == filepath.Join(f.r.Identity.Path, ".moe") {
					return injected
				}
				if phase == "receipt-sync" && file.Name() == plan.Receipt && fileExists(filepath.Join(plan.Receipt, "cache-cleared.json")) {
					return injected
				}
				return file.Sync()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.r.ExecuteCleanup(ctx, f.h, plan, f.env, io.Discard, io.Discard); !errors.Is(err, injected) {
				t.Fatalf("cache-clear failure hidden: %v", err)
			}
			if f.count(t) != 1 {
				t.Fatal("initial teardown did not run once")
			}
			syncRecordFile = original
			// Even removing the hook cannot turn a successful teardown into a
			// second execution: only the interrupted clear phase is retried.
			if err := os.Remove(filepath.Join(f.h.Root, f.scripts[0].Path)); err != nil {
				t.Fatal(err)
			}
			retry, err := f.r.PlanCleanup(f.h, "refresh", nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if retry.CompletedAttempt == "" {
				t.Fatal("lost teardown success")
			}
			if err := f.r.ExecuteCleanup(ctx, f.h, retry, f.env, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if f.count(t) != 1 {
				t.Fatal("cache-clear retry ran hooks")
			}
		})
	}
}

func TestCleanupInterruptedTimeoutHoldsWithoutRetry(t *testing.T) {
	f := ownedResourceFixture(t, "#!/bin/sh\nprintf run\\n >> \"$TRACE\"\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := f.r.ExecuteCleanup(ctx, f.h, f.plan(t, false), f.env, io.Discard, io.Discard)
	if _, ok := errors.AsType[*HoldError](err); !ok {
		t.Fatalf("timeout did not retain uncertain completion: %v", err)
	}
	if !fileExists(f.trace) {
		t.Fatal("timeout fixture never reached hook")
	}
	if _, err := f.r.PlanCleanup(f.h, "terminal", f.scripts, true); err == nil {
		t.Fatal("ordinary exact-run retry discharged interruption")
	}
	if cache, exists, err := f.r.CacheBytes(f.h); err != nil || !exists || !bytes.Equal(cache, f.cache) {
		t.Fatal("timeout lost cache")
	}
}

func TestCleanupWaitsForDetachedDescendants(t *testing.T) {
	f := ownedResourceFixture(t, `#!/bin/sh
setsid sh -c '(sleep .05; printf done > "$OWNED") </dev/null >/dev/null 2>&1 & exit 0' &
exit 0
`)
	if err := os.Remove(f.owned); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.r.ExecuteCleanup(ctx, f.h, f.plan(t, false), f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(f.owned); err != nil || string(b) != "done" {
		t.Fatalf("completion preceded detached job: %q %v", b, err)
	}
}

func TestCleanupPreflightRefusals(t *testing.T) {
	for _, kind := range []string{"missing-hook", "changed-hook", "missing-cache", "changed-cache", "wrong-owner", "missing-contract", "nonempty-free", "unsafe-script", "pending-ownership"} {
		t.Run(kind, func(t *testing.T) {
			f := ownedResourceFixture(t, "")
			current := f.scripts
			switch kind {
			case "missing-hook":
				current = nil
			case "changed-hook":
				current = append([]Script(nil), f.scripts...)
				current[0].Digest = strings.Repeat("0", 64)
			case "missing-cache":
				if err := os.Remove(filepath.Join(f.r.Identity.Path, ".moe", cacheName)); err != nil {
					t.Fatal(err)
				}
			case "changed-cache":
				put(t, filepath.Join(f.r.Identity.Path, ".moe", cacheName), "different")
			case "wrong-owner", "missing-contract", "nonempty-free", "unsafe-script":
				cache := f.cache
				teardown := f.scripts
				free := false
				if kind == "wrong-owner" {
					cache = bytes.Replace(cache, []byte("p/r"), []byte("p/other"), 1)
				}
				if kind == "missing-contract" {
					teardown = nil
				}
				if kind == "nonempty-free" {
					free = true
				}
				if kind == "unsafe-script" {
					teardown = []Script{{Path: "../outside", Digest: strings.Repeat("0", 64)}}
				}
				s := Resources{Version: 1, Generation: f.r.Identity.Generation, ID: strings.Repeat("1", 32), Sequence: 2, Cache: cache, Digest: digest(cache), Teardown: teardown, ResourceFree: free}
				if err := f.r.validateResources(s); err == nil {
					t.Fatal("invalid ownership accepted")
				}
				return
			case "pending-ownership":
				if _, err := f.r.RecordResources(f.h, f.cache, nil, f.scripts, false); err == nil {
					t.Fatal("overwrote pending ownership")
				}
				return
			}
			if _, err := f.r.PlanCleanup(f.h, "refresh", current, false); err == nil {
				t.Fatal("unsafe preflight admitted")
			}
			if f.count(t) != 0 {
				t.Fatal("preflight ran hooks")
			}
		})
	}
}

func TestCleanupOutputAndIntentBounds(t *testing.T) {
	t.Run("output", func(t *testing.T) {
		f := ownedResourceFixture(t, "#!/bin/sh\nprintf started > \"$TRACE\"\nhead -c 3145728 /dev/zero\n")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var output bytes.Buffer
		if err := f.r.ExecuteCleanup(ctx, f.h, f.plan(t, false), f.env, &output, io.Discard); err == nil {
			t.Fatal("oversized output accepted")
		}
		if output.Len() > cleanupOutputLimit {
			t.Fatal("unbounded captured output")
		}
		if _, exists, err := f.r.CacheBytes(f.h); err != nil || !exists {
			t.Fatal("output failure cleared cache")
		}
	})
	t.Run("intent-sync", func(t *testing.T) {
		f := ownedResourceFixture(t, "")
		plan := f.plan(t, false)
		original := syncRecordFile
		defer func() { syncRecordFile = original }()
		injected := errors.New("intent sync failed")
		syncRecordFile = func(*os.File) error { return injected }
		if err := f.r.ExecuteCleanup(context.Background(), f.h, plan, f.env, io.Discard, io.Discard); !errors.Is(err, injected) {
			t.Fatalf("intent failure: %v", err)
		}
		if f.count(t) != 0 {
			t.Fatal("hook preceded durable intent")
		}
	})
}

func TestResourceFreeCacheAndUnownedCache(t *testing.T) {
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if p, err := r.PlanCleanup(h, "terminal", nil, false); err != nil || p.Resources != nil {
		t.Fatalf("unallocated generation: %+v %v", p, err)
	}
	cache := []byte("# moe-dev-env-v1 p/r none\n")
	put(t, filepath.Join(path, ".moe", cacheName), string(cache))
	if err := r.ValidateEnvironment(h); err == nil {
		t.Fatal("unrecorded cache reused")
	}
	if _, err := r.RecordResources(h, cache, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanCleanup(h, "terminal", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ExecuteCleanup(context.Background(), h, plan, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := r.CacheBytes(h); err != nil || exists {
		t.Fatal("free cache not cleared")
	}
}
