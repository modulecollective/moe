package repolock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestEnsureGitignoreNeverEmpty asserts the property that broke
// TestConcurrentRunNewSerializes: a lock holder's `git status` must
// never catch <dir>/.gitignore existing-but-empty. os.WriteFile opens
// with O_TRUNC, so a bare-WriteFile ensureGitignore has a window where
// the file is truncated to zero bytes while every concurrent acquirer
// rewrites it; during that window `.moe/` is un-ignored and the holder
// sees a spurious dirty tree.
//
// Each round uses a fresh dir so the stat fast-path never short-circuits
// the write: K goroutines race the first-touch while a reader polls the
// path for the full duration of the race. Against the atomic tmp+rename
// writer the reader can only ever observe the file absent or fully
// written; against the old truncating writer it fails within a few
// rounds. The whole hammer is time-boxed to stay cheap.
func TestEnsureGitignoreNeverEmpty(t *testing.T) {
	const writers = 8
	deadline := time.Now().Add(300 * time.Millisecond)

	for round := 0; time.Now().Before(deadline); round++ {
		dir := filepath.Join(t.TempDir(), "round")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("round %d: mkdir: %v", round, err)
		}
		p := filepath.Join(dir, ".gitignore")

		stop := make(chan struct{})
		var readerWg sync.WaitGroup

		// Reader: poll the path until the writers finish. A successful
		// read of zero bytes means the writer left the file truncated.
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if b, err := os.ReadFile(p); err == nil && len(b) == 0 {
					t.Errorf("round %d: observed .gitignore existing but empty", round)
					return
				}
			}
		}()

		var writersWg sync.WaitGroup
		writersWg.Add(writers)
		for i := 0; i < writers; i++ {
			go func() {
				defer writersWg.Done()
				if err := ensureGitignore(dir); err != nil {
					t.Errorf("round %d: ensureGitignore: %v", round, err)
				}
			}()
		}

		writersWg.Wait()
		close(stop)
		readerWg.Wait()

		if t.Failed() {
			return
		}
	}
}
