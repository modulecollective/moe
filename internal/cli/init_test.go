package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestStdinIsTerminalRejectsDevNull is the targeted regression: the
// ModeCharDevice bit is set for both TTYs and /dev/null, so the helper
// has to additionally rule out the null device. Without the SameFile
// check, an exec.Command-spawned `moe init` (stdin defaults to
// /dev/null on Unix) flips this to true and self-commits.
func TestStdinIsTerminalRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })

	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin })

	if stdinIsTerminal() {
		t.Fatal("stdinIsTerminal() = true with stdin=/dev/null; expected false")
	}
}

// TestRunInitWithDevNullStdinLeavesStaged is the user-visible behavior
// test: with stdin wired to /dev/null, runInit must take the "not a
// terminal" branch — print "leaving staged" and skip the commit step.
// The original bug self-committed here because ModeCharDevice matched
// /dev/null and the EOF-on-read got treated as an empty (=yes) answer.
func TestRunInitWithDevNullStdinLeavesStaged(t *testing.T) {
	gittest.SetupEnv(t)
	dir := t.TempDir()

	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })

	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := runInit([]string{dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("runInit exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "leaving staged") {
		t.Fatalf("expected 'leaving staged' in stdout, got: %q", stdout.String())
	}
	// `git log` errors on a repo with no commits; rev-list --all
	// succeeds with empty output, which is what we want to see here.
	log := gittest.Output(t, dir, "rev-list", "--all", "--pretty=%s")
	if strings.Contains(log, "Initialize bureaucracy") {
		t.Fatalf("init self-committed under /dev/null stdin; log:\n%s", log)
	}
}
