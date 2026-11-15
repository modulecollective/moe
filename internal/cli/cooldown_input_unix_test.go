//go:build unix

package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestCooldownControlsReportDeadReader: a read failure ends the controls
// for the rest of the wait, and the operator is told. Closing the tty — the
// normal stop — stays quiet.
func TestCooldownControlsReportDeadReader(t *testing.T) {
	ch := make(chan cooldownControl, 1)
	var stderr bytes.Buffer
	readCooldownControls(strings.NewReader(strings.Repeat("x", 70<<10)+"\nc\n"), ch, &stderr)
	if !strings.Contains(stderr.String(), "cooldown: terminal controls stopped (bufio.Scanner: token too long)") {
		t.Errorf("stderr = %q, want the dead reader reported", stderr.String())
	}

	stderr.Reset()
	readCooldownControls(io.MultiReader(strings.NewReader("c\n"), errReader{os.ErrClosed}), ch, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want a closed tty to stay quiet", stderr.String())
	}
	if ctl := <-ch; ctl != controlContinue {
		t.Errorf("control = %v, want continue", ctl)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
