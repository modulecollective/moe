//go:build !unix

package procgroup

import "os/exec"

// Isolate is a no-op off unix. Process-group isolation (and the
// job-object equivalent Windows would need) isn't wired here because
// the headless cascades and the resident serve that depend on the group
// kill run on the linux box; stock CommandContext behavior (leader-only
// SIGKILL on cancel) stays.
func Isolate(*exec.Cmd) {}
