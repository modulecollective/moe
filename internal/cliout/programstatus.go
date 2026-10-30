package cliout

import (
	"encoding/base64"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RexStatusEnabled checks the destination, not stdin. Inherited Rex context
// inside a multiplexer does not prove that OSC reports reach Rex.
func RexStatusEnabled(w io.Writer) bool {
	term := os.Getenv("TERM")
	return IsTTY(w) && os.Getenv("REX_BLOCK") != "" && os.Getenv("REX_SESSION") != "" &&
		os.Getenv("MOE_SERVE_AGENT") != "1" && os.Getenv("TMUX") == "" && os.Getenv("STY") == "" &&
		!strings.HasPrefix(term, "tmux") && !strings.HasPrefix(term, "screen")
}

// ProgramStatus is an OSC 7501 record. ID, State, Kind and App are protocol
// tokens supplied by the caller; only Title and Message contain external text.
// https://www.superlogical.com/rex/docs/build/program-status
type ProgramStatus struct {
	ID, State, Kind, App string
	Title, Message       string
}

// Normalized bounds decoded UTF-8 bytes and removes forbidden controls before
// comparison, so changes outside the protocol's limits don't cause re-emission.
func (r ProgramStatus) Normalized() ProgramStatus {
	r.Title = statusText(r.Title, 192)
	r.Message = statusText(r.Message, 2048)
	return r
}

func statusText(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// WriteTo emits a whole report in one write, outside any text renderer.
func (r ProgramStatus) WriteTo(w io.Writer) (int64, error) {
	r = r.Normalized()
	s := "\x1b]7501;state=" + r.State + ":id=" + r.ID
	if r.State != "clear" {
		s += ":app=" + r.App
		if r.Kind != "" {
			s += ":kind=" + r.Kind
		}
		s += ":title=" + base64.StdEncoding.EncodeToString([]byte(r.Title)) +
			":msg=" + base64.StdEncoding.EncodeToString([]byte(r.Message))
	}
	s += "\x1b\\"
	n, err := io.WriteString(w, s)
	if err == nil && n != len(s) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}
