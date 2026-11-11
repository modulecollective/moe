// Package runowner is the per-run invocation guard: a process-held
// nonblocking flock on <root>/.moe/run-owners/<project>/<run>.lock that a
// moe invocation keeps from stage admission through its post-turn
// decision prompt and whichever continuation the operator picks.
//
// It exists because the two lifetimes moe already had both end too
// early. The repository lock must stay short, and a settled stage
// session must be allowed to delete its branch — so between session
// teardown and the operator's answer nothing said "someone is still
// deciding about this run", and a dynamic pulse could ride and merge it
// out from under the prompt.
//
// Ownership is the lock, nothing else: no file-existence predicate, no
// PID probe, no heartbeat, no expiry. Closing the descriptor releases
// it; process death releases it in the kernel. Guard files are never
// unlinked — removing one would let two contenders lock different
// inodes under the same path.
package runowner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/modulecollective/moe/internal/repolock"
)

// BusyError reports that another open guard — another process, or another
// acquisition in this one — owns the run.
type BusyError struct {
	Project, Run string
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("run %s/%s is owned by another moe invocation (an open stage or an unanswered prompt); "+
		"finish or exit that invocation first", e.Project, e.Run)
}

// IsBusy reports whether err is (or wraps) a BusyError.
func IsBusy(err error) bool {
	_, ok := errors.AsType[*BusyError](err)
	return ok
}

// Handle is one held guard. Possession of a live Handle is the owner's
// authority to continue on exactly that run; it authorises nothing else.
type Handle struct {
	Root, Project, Run string
	f                  *os.File
}

// Path is the guard file for one run under the canonical root.
func Path(root, projectID, runID string) string {
	return filepath.Join(root, ".moe", "run-owners", projectID, runID+".lock")
}

// Acquire takes the run's guard without waiting. Contention returns a
// *BusyError — including a second Acquire of the same run inside this
// process, because flock binds to the open file description, not the
// PID. The descriptor is close-on-exec (os.OpenFile's default), so
// agents and viewers spawned while it is held never inherit ownership.
func Acquire(root, projectID, runID string) (*Handle, error) {
	if _, err := repolock.EnsureDir(root); err != nil {
		return nil, fmt.Errorf("runowner: %w", err)
	}
	p := Path(root, projectID, runID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, fmt.Errorf("runowner: %w", err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("runowner: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &BusyError{Project: projectID, Run: runID}
		}
		return nil, fmt.Errorf("runowner: flock %s: %w", p, err)
	}
	return &Handle{Root: root, Project: projectID, Run: runID, f: f}, nil
}

// Release closes the descriptor, which drops the lock. Idempotent; the
// guard file stays.
func (h *Handle) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	f := h.f
	h.f = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("runowner: release %s/%s: %w", h.Project, h.Run, err)
	}
	return nil
}

// Owns reports whether h is a live guard for exactly this run.
func (h *Handle) Owns(root, projectID, runID string) bool {
	return h != nil && h.f != nil && h.Root == root && h.Project == projectID && h.Run == runID
}

// Busy is the read-side probe planners use to skip a run without trying
// to start it. A missing guard file is unowned; contention is owned; any
// other failure is returned, never read as "unowned". The answer is a
// snapshot — admission still has to Acquire.
func Busy(root, projectID, runID string) (bool, error) {
	p := Path(root, projectID, runID)
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runowner: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("runowner: flock %s: %w", p, err)
	}
	return false, nil
}
