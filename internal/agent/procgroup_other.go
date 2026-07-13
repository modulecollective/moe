//go:build !unix

package agent

import (
	"os"
	"os/exec"
)

// SetProcessGroup is a no-op off unix. Process-group isolation (and the
// job-object equivalent Windows would need) isn't wired here because the
// headless cascades that depend on the group kill run on the linux box;
// stock CommandContext behavior (leader-only SIGKILL on deadline) stays.
func SetProcessGroup(*exec.Cmd) {}

// signalProcess forwards sig to the leader — the only reachable target
// without process groups. Matches stock behavior.
func signalProcess(cmd *exec.Cmd, sig os.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
