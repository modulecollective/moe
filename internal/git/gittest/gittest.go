// Package gittest provides test fixture helpers for setting up
// throwaway git repos. It is the test-only sibling of internal/git —
// where internal/git wraps git for production callers (with retry,
// tracing, and a forbid-lint protecting it), gittest wraps git for
// test setup with t.Fatalf as the failure mode and zero ceremony at
// the callsite.
//
// The package shells out via os/exec directly rather than dogfooding
// internal/git for two reasons. First, internal/git's own tests are
// the primary dogfood site; routing through internal/git would create
// a circular dependency. Second, internal/git's error shape (formatted
// errors with retry-aware context) is wrong for tests, which want the
// raw git output folded into a t.Fatalf message. Test files are
// already exempted from the CI lint that forbids raw `exec.Command`
// outside internal/git, so this package's git invocations don't
// violate the boundary.
//
// Every exported helper takes *testing.T and fails the test on error.
// Fixtures don't compose with error returns — the caller's only
// response to "setup failed" is to abort the test — so the t.Fatalf
// style produces shorter callsites without losing diagnostics.
//
// Init isolates GIT_CONFIG_GLOBAL per-test via t.Setenv, so a stray
// ~/.gitconfig on a developer or CI box cannot leak into the fixture.
// t.Setenv refuses to run inside t.Parallel; tests that need parallel
// execution must take responsibility for their own isolation.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Init creates an initialized git repo in t.TempDir() and returns its
// path. The identity (user.name/user.email, commit.gpgsign=false) and
// GIT_CONFIG_GLOBAL isolation apply to every subsequent git invocation
// in the test, not just to this one repo — so a fixture that opens a
// donor/origin/seed repo alongside the main one inherits the same
// defaults without having to call Init twice. If `git` is not on PATH
// the test is skipped, not failed.
func Init(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	InitAt(t, dir)
	return dir
}

// InitAt initializes a git repo at dir (which must already exist).
// Used when the caller wants the repo inside a larger fixture tree
// rather than at the root of t.TempDir().
func InitAt(t *testing.T, dir string) {
	t.Helper()
	requireGit(t)
	isolateConfig(t)
	Run(t, dir, "init", "-q")
}

// InitBare creates an initialized bare repo in t.TempDir() and returns
// its path. Use it for the "remote" half of a clone/push fixture.
func InitBare(t *testing.T) string {
	t.Helper()
	requireGit(t)
	isolateConfig(t)
	dir := t.TempDir()
	Run(t, dir, "init", "--bare", "-q")
	return dir
}

// Run invokes `git <args...>` in dir. On non-zero exit the test fails
// with the combined stdout+stderr folded into the message.
func Run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// Output invokes `git <args...>` in dir and returns trimmed stdout.
// Non-zero exit fails the test with stderr folded into the message.
func Output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// Commit stages every change in dir and creates one commit with msg.
// Empty commits are allowed so callers can seed a repo with a HEAD
// without writing a file first. Returns the resulting SHA.
func Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	Run(t, dir, "add", "-A")
	Run(t, dir, "commit", "--allow-empty", "-m", msg)
	return HeadSHA(t, dir)
}

// WriteAndCommit writes content to <dir>/<relpath> (creating parent
// directories), stages it, commits with msg, and returns the SHA.
func WriteAndCommit(t *testing.T, dir, relpath, content, msg string) string {
	t.Helper()
	full := filepath.Join(dir, relpath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	Run(t, dir, "add", "--", relpath)
	Run(t, dir, "commit", "-m", msg)
	return HeadSHA(t, dir)
}

// HeadSHA returns the HEAD SHA of dir. Fails the test on error.
func HeadSHA(t *testing.T, dir string) string {
	t.Helper()
	return Output(t, dir, "rev-parse", "HEAD")
}

// requireGit skips the test if git is not on PATH. CI and developer
// boxes have git; stripped containers do not, and a missing-binary
// failure there should not turn the whole suite red.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
}

// isolateConfig points GIT_CONFIG_GLOBAL at a throwaway file seeded
// with the test identity and a gpgsign=false, then points
// GIT_CONFIG_SYSTEM at /dev/null so a strict CI box can't leak in via
// /etc/gitconfig either. Per-test scope is intentional: t.Setenv
// reverts at test end and refuses to run under t.Parallel — both the
// behaviour we want, since fixtures aren't parallel-safe and the
// identity must not leak across tests.
//
// The identity goes in GIT_CONFIG_GLOBAL (not `git config` in the
// repo) so it applies to every git invocation in the test — including
// secondary fixture repos (donor, origin, seed) the caller initializes
// with raw `Run(t, "", "init", …)`. Repo-local config would only cover
// the first repo and force every fixture to call Init twice.
func isolateConfig(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[user]\n\temail = test@example.com\n\tname = test\n" +
		"[commit]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("seed gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}
