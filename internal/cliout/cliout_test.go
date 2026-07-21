package cliout

import (
	"io"
	"os"
	"testing"
)

// unwrapWriter stands in for dash's watch-mode EL injector: a
// pass-through decorator over the operator's terminal.
type unwrapWriter struct{ w io.Writer }

func (u unwrapWriter) Write(p []byte) (int, error) { return u.w.Write(p) }
func (u unwrapWriter) Unwrap() io.Writer           { return u.w }

// TestIsTTYFollowsUnwrap: a decorator around stdout must not read as
// "not a terminal", or every caller gating styling on this predicate
// silently drops colour the moment someone wraps the writer.
func TestIsTTYFollowsUnwrap(t *testing.T) {
	f, err := os.Open("/dev/ptmx")
	if err != nil {
		t.Skipf("no pty device to stand in for a terminal: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if !IsTTY(f) {
		t.Skipf("/dev/ptmx does not classify as a terminal here; nothing to unwrap to")
	}

	if !IsTTY(unwrapWriter{w: unwrapWriter{w: f}}) {
		t.Fatal("IsTTY(wrapped tty) = false; expected the unwrap chain to reach the *os.File")
	}
}

// TestIsTTYUnwrapStopsAtNonTerminal: unwrapping is not a licence to
// call anything with an Unwrap method a terminal.
func TestIsTTYUnwrapStopsAtNonTerminal(t *testing.T) {
	if IsTTY(unwrapWriter{w: io.Discard}) {
		t.Fatal("IsTTY(wrapped io.Discard) = true; expected false")
	}
}

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
