//go:build !unix

package agent

import (
	"os"
	"os/exec"
)

// signalProcess forwards sig to the leader — the only reachable target
// without process groups. Matches stock behavior.
func signalProcess(cmd *exec.Cmd, sig os.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
