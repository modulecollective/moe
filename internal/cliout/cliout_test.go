package cliout

import (
	"os"
	"testing"
)

// TestIsTTYRejectsDevNull is the targeted regression mirroring
// init_test.go's TestStdinIsTerminalRejectsDevNull: ModeCharDevice is
// set for both TTYs and /dev/null, so IsTTY has to additionally rule
// out the null device. Without the SameFile guard a future caller
// wiring IsTTY to a behaviour decision would silently misclassify
// /dev/null as a terminal — the same shape as the init.go bug that
// motivated the original guard.
func TestIsTTYRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })

	if IsTTY(f) {
		t.Fatalf("IsTTY(%s) = true; expected false", os.DevNull)
	}
}
