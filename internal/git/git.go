// Package git wraps the small set of `git` shell-outs the bureaucracy
// CLI does at runtime. Three execution helpers cover the real shapes:
// Run discards output on success and folds it into the error on
// failure; Output captures stdout for programmatic use; Combined
// captures stdout+stderr together for forwarding git's own error prose.
package git

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// indexLockRetryCap and indexLockRetryStep bound the retry loop in Run
// when git fails with the worktree-shared `index.lock: File exists`
// race. Vars rather than consts so tests can shrink them; production
// values are 2s wall-time, 50ms increments. The cap exists so a real
// stuck lock (crashed git from a prior run) still surfaces a clear
// terminal error rather than spinning forever.
var (
	indexLockRetryCap  = 2 * time.Second
	indexLockRetryStep = 50 * time.Millisecond
)

// indexLockSubstr is the stderr fragment git emits when another
// process holds the worktree index lock. Both the bare-repo and
// linked-worktree paths produce this same suffix, so a substring
// match catches every shape — no need to anchor on the directory
// portion.
const indexLockSubstr = "index.lock': File exists"

// Run invokes git in dir. Stdout and stderr are captured together; on
// success they are discarded, on failure they are folded into the
// returned error — same shape as Output. No caller today needs
// terminal passthrough; the interactive `git push` path in cli/push.go
// shells out directly with its own writers.
//
// Worktree-shared `index.lock` contention (another moe process or a
// `hunk diff --watch` poll racing this one) is retried inside the
// indexLockRetryCap budget. Any other error is returned on the first
// attempt — the retry only fires when stderr contains the lock-file
// substring git itself prints, so unrelated failures pass through
// untouched.
func Run(dir string, args ...string) error {
	deadline := time.Now().Add(indexLockRetryCap)
	for {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		stderr := strings.TrimSpace(string(out))
		if strings.Contains(stderr, indexLockSubstr) && time.Now().Before(deadline) {
			time.Sleep(indexLockRetryStep)
			continue
		}
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, stderr)
	}
}

// Output runs git in dir capturing stdout. On failure, stderr is folded
// into the returned error message.
func Output(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// Combined runs git in dir capturing stdout+stderr together, trimmed,
// and returns the captured output even on error so callers can include
// it verbatim in diagnostics.
func Combined(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// RevParse returns the resolved SHA for ref in dir.
func RevParse(dir, ref string) (string, error) {
	out, err := Output(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return parseStatusZ(out)
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
