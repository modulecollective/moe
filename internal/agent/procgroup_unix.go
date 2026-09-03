//go:build unix

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

// signalProcess forwards sig to cmd's child. When the child was placed
// in its own process group (procgroup.Isolate), the signal goes to the
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
