package codex

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

type eofReader struct {
	r   io.Reader
	eof bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.eof = true
	}
	return n, err
}

// TestPipeExecProgressReportsOverlongLine: a line past the 8 MiB cap stops
// the scanner. The operator is told instead of progress going quiet, and
// the stream is still drained to EOF.
func TestPipeExecProgressReportsOverlongLine(t *testing.T) {
	tail := `{"type":"item.started","item":{"type":"command_execution","command":"ls"}}` + "\n"
	r := &eofReader{r: strings.NewReader(strings.Repeat("x", 9<<20) + "\n" + tail)}
	var w bytes.Buffer
	pipeExecProgress(r, &w, "", make(chan string, 1))
	if !strings.Contains(w.String(), "> progress stopped: bufio.Scanner: token too long") {
		t.Errorf("progress = %q, want the stop reported", w.String())
	}
	if !r.eof {
		t.Error("stream abandoned before EOF")
	}
}
