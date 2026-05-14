package cli

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// TestInstallGitTrace_SetsHookAndFormatsLine asserts that, with
// MOE_GIT_TRACE=1, installGitTrace populates git.Hook, and that firing
// the hook writes a single line in the documented format to stderr.
// The env-unset path is covered by the early return in installGitTrace
// itself; real-git hook firing is covered by TestHook_FiresOnRunAndStream
// in internal/git.
func TestInstallGitTrace_SetsHookAndFormatsLine(t *testing.T) {
	t.Setenv("MOE_GIT_TRACE", "1")

	prev := git.Hook
	t.Cleanup(func() { git.Hook = prev })
	git.Hook = nil

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	savedStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = savedStderr })

	installGitTrace()
	if git.Hook == nil {
		t.Fatal("git.Hook unset after installGitTrace with MOE_GIT_TRACE=1")
	}

	git.Hook("/tmp/repo", []string{"commit", "-m", "msg"}, 42*time.Millisecond, errors.New("boom"))

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}

	got := string(out)
	want := `git-trace dir="/tmp/repo" args=["commit" "-m" "msg"] dur=42ms err=boom` + "\n"
	if got != want {
		t.Fatalf("stderr line mismatch:\n got: %q\nwant: %q", got, want)
	}
}
