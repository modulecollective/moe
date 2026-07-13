//go:build unix

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// SetProcessGroup puts cmd's child in its own process group and swaps
// exec's default context-cancel (a leader-only SIGKILL) for a group-wide
// SIGKILL. On a deadline the whole group dies, so a tool child (go test,
// npm install) that outlives the agent binary can't keep writing into
// the sandbox clone after the turn is declared over — and it can't hold
// the child's stdout pipe open past the drain, which is what lets
// DrainThenWait reach a clean EOF at the deadline instead of hanging.
//
// One-shot only: the interactive path keeps stock behavior because its
// child reads the operator's tty, and moving it out of the terminal's
// foreground group would earn it SIGTTIN stops and break direct Ctrl-C
// delivery.
func SetProcessGroup(cmd *exec.Cmd) {
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

// signalProcess forwards sig to cmd's child. When the child was placed
// in its own process group (SetProcessGroup), the signal goes to the
// whole group so tool children still receive an operator Ctrl-C — the
// tty no longer delivers it to them, since they left the terminal's
// foreground group. Otherwise it goes to the leader alone, matching
// stock behavior.
func signalProcess(cmd *exec.Cmd, sig os.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		if s, ok := sig.(syscall.Signal); ok {
			return syscall.Kill(-cmd.Process.Pid, s)
		}
	}
	return cmd.Process.Signal(sig)
}
