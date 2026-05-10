// Package git wraps the small set of `git` shell-outs the bureaucracy
// CLI does at runtime. Three execution primitives cover the
// fire-and-check / capture / interleave shapes — Run discards output
// on success and folds it into the error on failure, Output captures
// stdout for programmatic use, Combined returns stdout+stderr together
// for diagnostics — and two more cover shapes the rest of the codebase
// kept reaching past internal/git to get: Probe for silent exit-code
// answers and Stream for live stdio passthrough on interactive ops
// (push, pull) where progress and credential prompts need to reach the
// operator.
//
// Every primitive except Stream funnels through one private execGit,
// so the worktree-shared index.lock retry, the error shape, and the
// tracing hook all live in exactly one place. Stream sits outside the
// retry loop on purpose: an interactive run has already written prompts
// and progress to the terminal by the time it fails, and a retry would
// replay them.
//
// Convention: dir == "" leaves cmd.Dir unset, so the command runs in
// the caller's cwd. That covers the one operation we do outside any
// repo (LsRemoteDefault against a URL) without a separate signature.
//
// Probe returns bool — no error. A missing git binary or a corrupt
// repo will surface at the next Run/Output call (which folds stderr
// into a clear error). Current callers all run after a real-repo
// invariant has been established, so the conflation doesn't bite.
//
// Anything in the rest of the codebase that shells out to `git`
// directly is a bug — bypassing this package skips the index-lock
// retry, drops out of the tracing hook, and reinvents the error shape.
// A forbid lint enforces that; see `make check-git-boundary`.
package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// writeRetryCap, readRetryCap, and indexLockRetryStep bound the retry
// loop on the worktree-shared `index.lock: File exists` race. Vars
// rather than consts so tests can shrink them; production values are
// 2s/500ms wall-time with 50ms steps. The split caps balance two
// asymmetric needs:
//
//   - Run (writes) gets the longer cap because a stuck write lock from
//     a crashed prior git invocation needs to eventually surface as a
//     hard fail — a too-short cap would mask the real problem.
//   - Output/Combined/Probe/typed wrappers (reads) get the shorter cap
//     because they sit on hot polling paths (`hunk diff --watch`) where
//     a UI stall longer than ~500ms is what the user notices. 10 ticks
//     at 50ms catches the typical 50-200ms race comfortably.
//
// Stream has no retry at all — interactive ops have already painted
// progress and prompts to the terminal by the time they fail.
var (
	writeRetryCap      = 2 * time.Second
	readRetryCap       = 500 * time.Millisecond
	indexLockRetryStep = 50 * time.Millisecond
)

// indexLockSubstr is the stderr fragment git emits when another
// process holds the worktree index lock. Both the bare-repo and
// linked-worktree paths produce this same suffix, so a substring
// match catches every shape — no need to anchor on the directory
// portion.
const indexLockSubstr = "index.lock': File exists"

// Hook, if set, is called after every git invocation that went through
// this package — including each retry attempt and the final Stream
// call. dir is the cmd.Dir we used (empty when we left it unset); args
// is git's argv (sans the leading "git"); dur is wall-time for that
// single attempt; err is the cmd.Run error (nil on success).
//
// Used as a test seam (intercept calls without subprocess mocking) and
// as the entry point for MOE_GIT_TRACE=1 once that wiring lands.
// Setting Hook from multiple goroutines without external coordination
// is the caller's problem — production callers set it once at startup.
var Hook func(dir string, args []string, dur time.Duration, err error)

// Run invokes git in dir. Stdout and stderr are captured together; on
// success they're discarded, on failure they're folded into the
// returned error. For write operations (commit, add, fetch, push when
// non-interactive); use Stream when stdio needs to reach the terminal.
//
// Worktree-shared `index.lock` contention is retried inside
// writeRetryCap. Any other error is returned on the first attempt —
// the retry only fires when stderr contains the lock-file substring
// git itself prints, so unrelated failures pass through untouched.
func Run(dir string, args ...string) error {
	combined, _, err := execGit(dir, args, true, writeRetryCap)
	if err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(combined)))
	}
	return nil
}

// Output runs git in dir capturing stdout. On failure, stderr is
// folded into the returned error message. Index-lock retry uses the
// shorter read cap.
func Output(dir string, args ...string) (string, error) {
	stdout, stderr, err := execGit(dir, args, false, readRetryCap)
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}

// Combined runs git in dir capturing stdout+stderr in git's own write
// order, trimmed, and returns the captured output even on error so
// callers can include it verbatim in diagnostics. Index-lock retry
// uses the shorter read cap.
func Combined(dir string, args ...string) (string, error) {
	out, _, err := execGit(dir, args, true, readRetryCap)
	return strings.TrimSpace(string(out)), err
}

// Probe runs git in dir and reports whether it exited 0. All output is
// suppressed — Probe is for the shape where exit code IS the answer
// (`diff --quiet`, `rev-parse --verify --quiet`, `remote get-url`).
// Index-lock retry uses the shorter read cap; the typed wrapper HasRef
// is the most-common shape.
func Probe(dir string, args ...string) bool {
	_, _, err := execGit(dir, args, true, readRetryCap)
	return err == nil
}

// Stream runs git in dir with stdin wired to os.Stdin and
// stdout/stderr piped to the supplied writers. For interactive runs
// (push, pull, anything that may prompt for credentials or paint
// progress). No retry — interactive callers see errors directly and a
// retry would replay prompts.
func Stream(dir string, stdout, stderr io.Writer, args ...string) error {
	start := time.Now()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if Hook != nil {
		Hook(dir, args, time.Since(start), err)
	}
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// execGit is the one place git actually runs. combined==true shares
// one buffer between stdout and stderr (preserving git's interleave
// order, used by Run/Combined/Probe); combined==false captures them
// separately (used by Output and typed wrappers that parse stdout).
// retryCap bounds the index-lock retry loop; 0 disables retry.
//
// Hook (if set) fires once per attempt — retries are visible to a
// tracer, not hidden inside this loop.
func execGit(dir string, args []string, combined bool, retryCap time.Duration) (stdoutBuf, stderrBuf []byte, err error) {
	deadline := time.Now().Add(retryCap)
	for {
		start := time.Now()
		cmd := exec.Command("git", args...)
		if dir != "" {
			cmd.Dir = dir
		}
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		if combined {
			cmd.Stderr = &outBuf
		} else {
			cmd.Stderr = &errBuf
		}
		err = cmd.Run()
		if Hook != nil {
			Hook(dir, args, time.Since(start), err)
		}
		if err == nil {
			return outBuf.Bytes(), errBuf.Bytes(), nil
		}
		// Pick the buffer that actually carries git's stderr — when
		// combined, it's the shared outBuf; otherwise errBuf.
		check := errBuf.Bytes()
		if combined {
			check = outBuf.Bytes()
		}
		if retryCap > 0 && bytes.Contains(check, []byte(indexLockSubstr)) && time.Now().Before(deadline) {
			time.Sleep(indexLockRetryStep)
			continue
		}
		return outBuf.Bytes(), errBuf.Bytes(), err
	}
}

// RevParse returns the resolved SHA for ref in dir.
func RevParse(dir, ref string) (string, error) {
	out, err := Output(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// HEAD returns the SHA of dir's HEAD. Sugar over RevParse(dir, "HEAD").
func HEAD(dir string) (string, error) {
	return RevParse(dir, "HEAD")
}

// HasRef reports whether ref resolves in dir. Probe sugar for
// `rev-parse --verify --quiet <ref>` — the most-common Probe shape and
// worth naming so callers don't open-code the flags every time.
func HasRef(dir, ref string) bool {
	return Probe(dir, "rev-parse", "--verify", "--quiet", ref)
}

// Upstream returns the upstream ref name (e.g. "origin/main") for the
// branch checked out in dir, or "" if no upstream is configured (or
// HEAD is detached, or any other rev-parse @{u} failure). The "" case
// is intentionally permissive: callers want a single check for "is
// there an upstream to pull from / push to" and any failure here means
// the answer is "no". A real repo problem (missing git, corrupt index)
// will surface at the next Run/Output call.
func Upstream(dir string) (string, error) {
	stdout, _, err := execGit(dir,
		[]string{"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"},
		false, readRetryCap)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(stdout)), nil
}

// AheadOf returns the commit count of base..head — i.e. how many
// commits head has that base doesn't. Returns (0, nil) on any rev-list
// failure: the only current caller wants to skip the check when base
// is an unknown ref, and the eventual push will surface a real error
// itself. Wraps `rev-list --count <base>..<head>`.
func AheadOf(dir, base, head string) (int, error) {
	stdout, _, err := execGit(dir,
		[]string{"rev-list", "--count", base + ".." + head},
		false, readRetryCap)
	if err != nil {
		return 0, nil
	}
	n, parseErr := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if parseErr != nil {
		return 0, fmt.Errorf("git rev-list --count %s..%s: parse %q: %w",
			base, head, strings.TrimSpace(string(stdout)), parseErr)
	}
	return n, nil
}

// LsRemoteDefault returns the default-branch name advertised by url —
// the "<branch>" of `ref: refs/heads/<branch>\tHEAD` in
// `ls-remote --symref <url> HEAD` output. Pure URL operation: runs
// outside any repo (dir == "").
func LsRemoteDefault(url string) (string, error) {
	stdout, stderr, err := execGit("",
		[]string{"ls-remote", "--symref", url, "HEAD"},
		false, readRetryCap)
	if err != nil {
		return "", fmt.Errorf("git ls-remote --symref %s: %w (%s)",
			url, err, strings.TrimSpace(string(stderr)))
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		const prefix = "refs/heads/"
		if strings.HasPrefix(fields[1], prefix) {
			return strings.TrimPrefix(fields[1], prefix), nil
		}
	}
	return "", fmt.Errorf("git ls-remote --symref %s: no symbolic HEAD in output", url)
}

// ShortSHA returns the 7-character short form of sha (git's default).
// Returns sha unchanged if shorter than 7.
func ShortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// StatusEntry is one record of `git status --porcelain=v1 -z` output:
// the two-character XY code plus the path verbatim. From is the
// original path on rename/copy entries (X or Y is 'R' or 'C') and
// empty otherwise.
type StatusEntry struct {
	XY   string
	Path string
	From string
}

// Status reports the working-tree status of dir, scoped to paths
// (all paths if none given). Output is parsed from
// `git status --porcelain=v1 -z --untracked-files=all`, so paths
// arrive verbatim — no core-quoting, no octal escapes — and rename
// records carry both new (Path) and original (From).
//
// -z reports rename records as NEW\0OLD (the reverse of the human
// `R old -> new` form), so the first NUL-terminated path is current
// and the second is the source.
func Status(dir string, paths ...string) ([]StatusEntry, error) {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	stdout, stderr, err := execGit(dir, args, false, readRetryCap)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return parseStatusZ(stdout)
}

func parseStatusZ(out []byte) ([]StatusEntry, error) {
	// -z terminates every field with NUL (no trailing newline). An
	// empty output means a clean tree.
	out = bytes.TrimRight(out, "\x00")
	if len(out) == 0 {
		return nil, nil
	}
	tokens := bytes.Split(out, []byte{0})
	var entries []StatusEntry
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			return nil, fmt.Errorf("git status: malformed record %q", string(tok))
		}
		// Layout per record: "XY <path>" with a single space at index 2.
		xy := string(tok[:2])
		path := string(tok[3:])
		entry := StatusEntry{XY: xy, Path: path}
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("git status: rename record %q missing source path", xy+" "+path)
			}
			entry.From = string(tokens[i])
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
