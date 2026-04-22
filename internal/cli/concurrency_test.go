package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// Register the project so run.New's presence check passes.
	if err := os.MkdirAll(filepath.Join(root, "projects", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "demo", "project.json"), []byte(`{"id":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "projects/demo/project.json")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "seed project")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := withRepoLock(root, repolock.Options{
				Purpose:    "run-new",
				Budget:     30 * 1_000_000_000, // 30s in ns
				BackoffCap: 10_000_000,         // 10ms
			}, func() error {
				md, err := run.New(root, "demo", "concurrent one", run.Options{Workflow: "sdlc"})
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

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
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
	out, err := exec.Command("git", "-C", root, "log", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	gotOpens := strings.Count(string(out), "Open run demo/")
	if gotOpens != n {
		t.Errorf("git history has %d open-run commits, want %d\n%s", gotOpens, n, out)
	}
}
