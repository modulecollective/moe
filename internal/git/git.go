// Package git wraps the small set of `git` shell-outs the bureaucracy
// CLI does at runtime. Three execution helpers cover the real shapes:
// Run streams stdio to the terminal so credential helpers can prompt;
// Output captures stdout for programmatic use; Combined captures
// stdout+stderr together for forwarding git's own error prose.
package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run invokes git with stdio wired to the user's terminal so credential
// helpers and SSH prompts can complete. Capturing stderr would hide
// those prompts and make the command appear to hang.
func Run(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
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
