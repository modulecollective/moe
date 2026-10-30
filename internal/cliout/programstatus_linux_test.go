//go:build linux

package cliout

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func TestRexStatusOutputGate(t *testing.T) {
	f, err := os.Open("/dev/ptmx")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base := map[string]string{"REX_BLOCK": "b", "REX_SESSION": "s", "TERM": "xterm-256color", "TMUX": "", "STY": "", "MOE_SERVE_AGENT": "", "NO_COLOR": "1"}
	for k, v := range base {
		t.Setenv(k, v)
	}
	if !RexStatusEnabled(unwrapWriter{w: f}) {
		t.Fatal("Rex terminal disabled")
	}
	if RexStatusEnabled(&bytes.Buffer{}) || RexStatusEnabled(io.Discard) {
		t.Fatal("redirect enabled")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if RexStatusEnabled(null) {
		t.Fatal("null enabled")
	}
	for _, tc := range []struct{ key, value string }{{"REX_BLOCK", ""}, {"REX_SESSION", ""}, {"TMUX", "nested"}, {"STY", "nested"}, {"TERM", "screen-256color"}, {"TERM", "tmux-256color"}, {"MOE_SERVE_AGENT", "1"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if RexStatusEnabled(f) {
				t.Fatal("unexpectedly enabled")
			}
		})
	}
}
