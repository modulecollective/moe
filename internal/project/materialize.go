package project

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

// SubmoduleInitError is returned by EnsureMaterialized when the
// project's submodule mount-point exists but is not initialised on
// this machine and the auto-init shell-out failed. The CLI catches it
// via errors.As to print a retry hint that the operator can paste into
// a shell.
type SubmoduleInitError struct {
	Root      string
	ProjectID string
	Output    string
	Err       error
}

func (e *SubmoduleInitError) Error() string {
	cmd := materializeRetryCommand(e.ProjectID)
	msg := fmt.Sprintf(
		"project: %q submodule could not be initialised.\n"+
			"  %s\n"+
			"  failed: %v",
		e.ProjectID, cmd, e.Err)
	if trimmed := strings.TrimSpace(e.Output); trimmed != "" {
		msg += "\n  output:\n" + indent(trimmed, "    ")
	}
	msg += fmt.Sprintf("\nRun that command manually in %s once the underlying issue is resolved, then retry.", e.Root)
	return msg
}

func (e *SubmoduleInitError) Unwrap() error { return e.Err }

// EnsureMaterialized makes sure projects/<id>/src is present
// (auto-runs `git submodule update --init --recursive` when the
// mount-point is the empty stub left by `git submodule add`). Returns
// *SubmoduleInitError on failure with the verbatim retry command.
// Cheap and idempotent — a non-empty src short-circuits with one
// `os.ReadDir`.
//
// Init-only. Pointer drift (gitlink moved, checkout didn't) is handled
// downstream: sync.AdvanceSubmodule fast-forwards already-checked-out
// submodules, and sandbox.EnsureAt's `git checkout HEAD` picks up
// whatever the canonical submodule's HEAD points at when the per-run
// clone is created. The gate's job is "is there source on disk at all,"
// not "is it at the right SHA."
//
// --recursive is the default: the cost on current projects (none nest
// submodules) is zero, and a future nested submodule that silently
// isn't there is the exact surprise this gate is removing.
//
// Returns nil (no error) when there's nothing to do: src missing
// (caller's "project doesn't exist" problem to name); src non-empty
// (already materialised); .gitmodules absent or missing this
// submodule's stanza (not a registered submodule on this checkout).
func EnsureMaterialized(root, projectID string) error {
	src := filepath.Join(root, SubmoduleDir(projectID))
	info, err := os.Stat(src)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("project: read %s: %w", src, err)
	}
	if len(entries) > 0 {
		return nil
	}

	declared, err := gitmodulesDeclares(filepath.Join(root, ".gitmodules"), SubmoduleDir(projectID))
	if err != nil {
		return err
	}
	if !declared {
		return nil
	}

	fmt.Fprintf(os.Stderr, "project: initialising submodule %s ...\n", SubmoduleDir(projectID))
	// -c protocol.file.allow=always relaxes CVE-2022-39253 hardening for
	// this one invocation. MoE's own workspace plumbing rewrites
	// submodule URLs to file:// paths under .moe/clones/ so parallel
	// runs share a clone; without the flag, modern git (>=2.38.1)
	// rejects the transport and the auto-init dies with "fatal:
	// transport 'file' not allowed". The threat model the guard
	// protects against is malicious upstream submodule URLs, not MoE's
	// own local clones — and the flag is scoped per-invocation, not
	// written into persistent config.
	out, err := git.Combined(root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive", SubmoduleDir(projectID))
	if err != nil {
		return &SubmoduleInitError{
			Root:      root,
			ProjectID: projectID,
			Output:    out,
			Err:       err,
		}
	}
	if strings.TrimSpace(out) != "" {
		fmt.Fprintln(os.Stderr, out)
	}
	return nil
}

// materializeRetryCommand renders the verbatim shell command an
// operator can run to materialise a project's submodule by hand —
// kept in lockstep with EnsureMaterialized's git invocation so the
// hint never drifts from what the auto-init actually runs.
func materializeRetryCommand(projectID string) string {
	return "git -c protocol.file.allow=always submodule update --init --recursive " + SubmoduleDir(projectID)
}

// gitmodulesDeclares parses .gitmodules looking for `path = want`.
// Returns false (no error) when .gitmodules is missing — that's the
// "not a bureaucracy / no submodules declared" shape and the gate
// silently no-ops in that case.
func gitmodulesDeclares(path, want string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("project: open %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "path") {
			continue
		}
		_, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(val) == want {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("project: scan %s: %w", path, err)
	}
	return false, nil
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
