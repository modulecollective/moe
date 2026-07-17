package cli

import (
	"io"
	"os"
	"testing"
)

// TestMain neutralizes the pulse hook by default for the whole cli test
// binary. firePulse tails every run-traffic verb (sdlc/twin close, sdlc
// push, cascade auto-close) with a blocking, run-minting agent survey —
// correct in production, but a test that merely drives `moe sdlc close`
// must not spawn one and must not see its HEAD advance past the close
// commit onto the pulse run's open commit. Tests that specifically
// exercise the hook install their own stub via stubFirePulse, which
// overrides this no-op and restores it on cleanup. Same isolation the
// per-test runStageSession stub gives the agent turn itself.
func TestMain(m *testing.M) {
	firePulse = func(root, projectID string, stdout, stderr io.Writer) {}
	os.Exit(m.Run())
}
