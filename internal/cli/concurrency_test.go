package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// TestConcurrentRunNewSerializes exercises the repo lock end-to-end:
// N goroutines call run.New through the same wrapper used by the CLI,
// all targeting the same bureaucracy. The lock must serialize them so
// no two commits clobber the index, and each caller must end up with
// a distinct run id (auto-suffix). Without the lock, git's "index
// locked" surface and/or intermediate-state interleaving would produce
// a mix of failures and dropped runs.
func TestConcurrentRunNewSerializes(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	// Register the project so run.New's presence check passes.
	if err := os.MkdirAll(filepath.Join(root, "projects", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "demo", "project.json"), []byte(`{"id":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "projects/demo/project.json")
	gittest.Run(t, root, "commit", "-m", "seed project")

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := repolock.With(root, repolock.Options{
				Purpose:    "run-new",
				Budget:     30 * 1_000_000_000, // 30s in ns
				BackoffCap: 10_000_000,         // 10ms
				// Distinct hostname per goroutine: same-process
				// contention now fails fast with NestedError, so model
				// the real shape — N separate moe invocations.
				Hostname: func() (string, error) { return fmt.Sprintf("host-%d", i), nil },
			}, func() error {
				// IDBase collisions get a dated/numbered suffix so
				// every goroutine resolves to a distinct slug under
				// the same base — the concurrency check is that the
				// lock serialises slug allocation.
				md, err := run.New(root, "demo", run.Options{IDBase: "concurrent-one", Workflow: "sdlc"})
				if err != nil {
					return err
				}
				ids[i] = md.ID
				return nil
			})
			errs[i] = err
		}()
	}
	wg.Wait()

	// Report every goroutine's error, not just the first in index
	// order: a flake here is a race, and the root cause may be a
	// sibling goroutine, not goroutine 0. Collect all before failing.
	failed := false
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
			failed = true
		}
	}
	if failed {
		t.Fatalf("one or more goroutines failed; see errors above")
	}

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			continue
		}
		if seen[ids[i]] {
			t.Errorf("duplicate run id %q", ids[i])
		}
		seen[ids[i]] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct ids, want %d", len(seen), n)
	}
	// Log should show n "Open run …" commits in the history.
	out := gittest.Output(t, root, "log", "--format=%s")
	gotOpens := strings.Count(out, "Open run demo/")
	if gotOpens != n {
		t.Errorf("git history has %d open-run commits, want %d\n%s", gotOpens, n, out)
	}
}
