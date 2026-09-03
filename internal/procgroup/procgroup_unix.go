//go:build unix

// Package procgroup isolates an exec.Cmd's child in its own process
// group so a context cancel reaps the whole tree rather than the
// leader alone.
//
// Two callers need it for the same reason. internal/agent runs a
// one-shot agent binary that spawns tool children (go test, npm
// install); internal/git runs `git push`, which spawns a transport
// helper (git-remote-https). In both cases the grandchild inherits the
// captured stdio pipe, so exec's default leader-only SIGKILL leaves
// Wait blocked on a process nobody is waiting for — a deadline that
// turns one hang into another.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Isolate puts cmd's child in its own process group and swaps exec's
// default context-cancel (a leader-only SIGKILL) for a group-wide one.
// On a deadline the whole group dies, so a grandchild can't outlive the
// cancel — it can't keep writing into a sandbox clone after the turn is
// declared over, and it can't hold the child's stdout pipe open past
// the drain, which is what lets a Wait reach clean EOF at the deadline
// instead of hanging.
//
// For non-interactive children only. A child that reads the operator's
// tty must keep stock behavior: moving it out of the terminal's
// foreground group earns it SIGTTIN stops and breaks direct Ctrl-C
// delivery.
func Isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// Negative pid targets the whole group. With Setpgid and Pgid 0
		// the child becomes its own group leader, so its pid is the
		// pgid. ESRCH means the group is already gone — map it to the
		// sentinel exec uses for an already-exited process so Wait
		// doesn't surface a spurious cancel error.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
