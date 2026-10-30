package cliout

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

type statusWriter struct {
	bytes.Buffer
	writes int
	short  bool
}

func (w *statusWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *statusWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.short {
		return len(p) - 1, nil
	}
	return w.Buffer.Write(p)
}

func TestProgramStatusEncoding(t *testing.T) {
	r := ProgramStatus{ID: "moe/dash/questions", State: "blocked", Kind: "question", App: "moe",
		Title: strings.Repeat("界", 100) + "\xff", Message: "Ask:\n\t\x1b\x07\u0085" + strings.Repeat("é", 1200)}
	var w statusWriter
	n, err := r.WriteTo(&w)
	if err != nil || n != int64(w.Len()) || w.writes != 1 {
		t.Fatalf("write = %d, %v; writes=%d", n, err, w.writes)
	}
	wire := w.String()
	if len(wire) > 4096 || !strings.HasPrefix(wire, "\x1b]7501;state=blocked:id=moe/dash/questions:app=moe:kind=question:") || !strings.HasSuffix(wire, "\x1b\\") {
		t.Fatalf("invalid report: %q", wire)
	}
	fields := strings.Split(strings.TrimSuffix(wire, "\x1b\\"), ":")
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key != "title" && key != "msg" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil || !utf8.Valid(decoded) {
			t.Fatalf("%s decode: %v", key, err)
		}
		limit := 2048
		if key == "title" {
			limit = 192
		}
		if len(decoded) > limit {
			t.Fatalf("%s has %d bytes", key, len(decoded))
		}
		for _, r := range string(decoded) {
			if unicode.IsControl(r) {
				t.Fatalf("control %U in %s", r, key)
			}
		}
	}
	if got := r.Normalized(); got.Title != strings.Repeat("界", 64) || got.Message != "Ask:"+strings.Repeat("é", 1022) {
		t.Fatalf("normalization = %+v", got)
	}
	if got := statusText("a\xffb\u009fc", 192); got != "a�bc" {
		t.Fatalf("invalid UTF-8 normalization: %q", got)
	}
}

func TestProgramStatusClearAndShortWrite(t *testing.T) {
	var b bytes.Buffer
	_, err := (ProgramStatus{ID: "moe/dash", State: "clear", App: "moe", Message: "ignored"}).WriteTo(&b)
	if err != nil || b.String() != "\x1b]7501;state=clear:id=moe/dash\x1b\\" {
		t.Fatalf("clear = %q, %v", b.String(), err)
	}
	w := &statusWriter{short: true}
	_, err = (ProgramStatus{ID: "moe/dash", State: "clear"}).WriteTo(w)
	if !errors.Is(err, io.ErrShortWrite) || w.writes != 1 {
		t.Fatalf("short write: %v; writes=%d", err, w.writes)
	}
}
