package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/serve"
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

	staged := gittest.Output(t, dir, "diff", "--cached", "--binary")
	if staged == "" {
		t.Fatal("init must leave a staged scaffold")
	}
	srv, err := serve.New(serveOptions(dir, &stderr))
	if err != nil {
		t.Fatal(err)
	}
	requestDash := func() *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		return response
	}
	response := requestDash()
	if response.Code != http.StatusOK {
		t.Fatalf("fresh init GET / = %d: %s", response.Code, response.Body.String())
	}
	for _, section := range []string{"active", "intents", "chores", "backlog"} {
		if !strings.Contains(response.Body.String(), section+` <span class="count">(0)</span>`) {
			t.Errorf("missing empty %s section", section)
		}
	}
	if !strings.Contains(response.Body.String(), "0 projects registered") {
		t.Fatal("missing empty project count")
	}
	t.Setenv("MOE_HOME", dir)
	stdout.Reset()
	stderr.Reset()
	if code := runRunList([]string{"--all"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("fresh init run list exit=%d stdout=%q: %s", code, stdout.String(), stderr.String())
	}
	if _, err := git.HEAD(dir); err == nil {
		t.Fatal("observation committed the scaffold")
	}
	if after := gittest.Output(t, dir, "diff", "--cached", "--binary"); after != staged {
		t.Fatal("observation changed the staged scaffold")
	}
	gittest.Commit(t, dir, "Initialize bureaucracy")
	if response := requestDash(); response.Code != http.StatusOK {
		t.Fatalf("same server after first commit GET / = %d: %s", response.Code, response.Body.String())
	}
}

func TestInitDashboardBrokenHEAD(t *testing.T) {
	gittest.SetupEnv(t)
	dir := t.TempDir()
	if err := bureaucracy.Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	// A branch ref naming an absent object is corruption, not empty history.
	if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	srv, err := serve.New(serveOptions(dir, &stderr))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "git") {
		t.Fatalf("broken HEAD GET / = %d: %s", response.Code, response.Body.String())
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
