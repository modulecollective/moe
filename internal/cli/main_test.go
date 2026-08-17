package cli

import (
	"os"
	"testing"
)

// TestMain is the cli test binary's entry point. It used to neutralize
// the run-traffic pulse tail for every test in the package; that seam is
// gone — nothing fires a pulse in-process any more — so a test that
// drives `moe sdlc close` or `moe sdlc push` simply doesn't sweep.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
