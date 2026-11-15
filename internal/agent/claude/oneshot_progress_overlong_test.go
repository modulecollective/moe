package claude

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// eofReader records whether its reader was read to EOF — the proof the
// progress pipe was drained rather than abandoned.
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

// TestPipeOneShotProgressReportsOverlongLine: a line past the 8 MiB cap
// stops the scanner. The operator is told, and the rest of the stream is
// still read so the child can't block on a full pipe before Wait.
func TestPipeOneShotProgressReportsOverlongLine(t *testing.T) {
	tail := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"x"}}]}}` + "\n"
	r := &eofReader{r: strings.NewReader(strings.Repeat("x", 9<<20) + "\n" + tail)}
	var w bytes.Buffer
	pipeOneShotProgress(r, &w, "", nil)
	if !strings.Contains(w.String(), "> progress stopped: bufio.Scanner: token too long") {
		t.Errorf("progress = %q, want the stop reported", w.String())
	}
	if !r.eof {
		t.Error("stream abandoned before EOF")
	}
}
