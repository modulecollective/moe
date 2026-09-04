package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/bureaucracy"
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

// TestPromptInitCommit pins the answer rule at `moe init`'s "commit
// now? [Y/n]" prompt. A reflex Enter and an explicit `y` commit; `n`
// and a bare Ctrl-D both leave the tree staged. The EOF row is the
// point of the test: Ctrl-D is how an operator says "I'm leaving,"
// and until this branch existed it committed instead.
//
// Driving promptInitCommit rather than runInit is what makes the
// answer observable at all — runInit's stdinIsTerminal() gate takes
// the "not a terminal" branch before the read whenever stdin is a
// pipe, so there is no way to feed it an answer from a test.
func TestPromptInitCommit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      string
		wantCommit bool
	}{
		{name: "blank-commits", input: "\n", wantCommit: true},
		{name: "explicit-y-commits", input: "y\n", wantCommit: true},
		{name: "n-leaves-staged", input: "n\n", wantCommit: false},
		// Raw "" — a closed pipe with zero bytes, i.e. a bare Ctrl-D.
		{name: "eof-leaves-staged", input: "", wantCommit: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gittest.SetupEnv(t)
			dir := t.TempDir()
			if err := bureaucracy.Init(dir, ""); err != nil {
				t.Fatalf("bureaucracy.Init: %v", err)
			}
			withStdinLine(t, tc.input)

			var stdout, stderr bytes.Buffer
			if code := promptInitCommit(dir, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			// `git log` errors on a repo with no commits; rev-list
			// --all succeeds with empty output either way.
			log := gittest.Output(t, dir, "rev-list", "--all", "--pretty=%s")
			if got := strings.Contains(log, "Initialize bureaucracy"); got != tc.wantCommit {
				t.Fatalf("committed=%v, want %v; log:\n%s", got, tc.wantCommit, log)
			}
			out := stdout.String()
			if got := strings.Contains(out, "left staged"); got == tc.wantCommit {
				t.Fatalf("'left staged' present=%v with wantCommit=%v; stdout=%q", got, tc.wantCommit, out)
			}
			if tc.name == "eof-leaves-staged" {
				// A terminal echoes nothing for Ctrl-D and the prompt
				// has no trailing newline, so the message would
				// otherwise land on the tail of "[Y/n] ".
				if !strings.Contains(out, "[Y/n] \nleft staged") {
					t.Fatalf("expected the EOF message on its own line, got %q", out)
				}
			}
		})
	}
}
