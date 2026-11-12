package ui

import (
	"strings"
	"testing"
)

func codes(ks []Key) string {
	var parts []string
	for _, k := range ks {
		switch k.Code {
		case KeyRune:
			parts = append(parts, "r:"+string(k.Rune))
		case KeyPaste:
			s := "paste:" + k.Text
			if k.Truncated {
				s += "+cut"
			}
			parts = append(parts, s)
		default:
			parts = append(parts, map[KeyCode]string{
				KeyEnter: "enter", KeyTab: "tab", KeyBackTab: "backtab", KeyBackspace: "bs",
				KeyDelete: "del", KeyUp: "up", KeyDown: "down", KeyLeft: "left", KeyRight: "right",
				KeyHome: "home", KeyEnd: "end", KeyPgUp: "pgup", KeyPgDn: "pgdn", KeyEsc: "esc",
				KeyCtrlC: "^C", KeyCtrlS: "^S", KeyCtrlX: "^X", KeyCtrlZ: "^Z", KeyCtrlL: "^L",
			}[k.Code])
		}
	}
	return strings.Join(parts, " ")
}

// Every byte split of a sequence decodes the same as the whole.
func TestDecoderFragments(t *testing.T) {
	in := "a\x1b[A\x1bOB\x1b[1;5C\x1b[3~\x1b[5~\x1b[6~\x1b[H\x1b[4~é😀\r\x7f\t\x1b[Z\x13\x03\x18"
	want := "r:a up down right del pgup pgdn home end r:é r:😀 enter bs tab backtab ^S ^C ^X"
	for split := range len(in) + 1 {
		var d decoder
		got := append(d.feed([]byte(in[:split])), d.feed([]byte(in[split:]))...)
		if codes(got) != want {
			t.Fatalf("split %d: %s", split, codes(got))
		}
	}
	// Byte at a time.
	var d decoder
	var got []Key
	for i := range len(in) {
		got = append(got, d.feed([]byte{in[i]})...)
	}
	if codes(got) != want {
		t.Fatalf("bytewise: %s", codes(got))
	}
}

// A lone Escape waits for flush; a following key is not swallowed.
func TestDecoderLoneEscape(t *testing.T) {
	var d decoder
	if got := d.feed([]byte{0x1b}); len(got) != 0 || !d.pending() {
		t.Fatalf("lone esc decoded early: %s", codes(got))
	}
	if got := codes(d.flush()); got != "esc" {
		t.Fatalf("flush = %s", got)
	}
	if got := codes(d.feed([]byte("\x1b\x1b[A"))); got != "esc up" {
		t.Fatalf("esc esc-seq = %s", got)
	}
	if got := codes(d.feed([]byte("\x1bx"))); got != "esc r:x" {
		t.Fatalf("alt-x = %s", got)
	}
}

// Paste is text: shortcuts and escapes inside it are not keys, line
// endings normalise, and an end marker split across reads still ends it.
func TestDecoderPaste(t *testing.T) {
	var d decoder
	in := "\x1b[200~q:finish\r\n\x1b[Aline\x03\x1b[201~x"
	for split := range len(in) + 1 {
		d = decoder{}
		got := append(d.feed([]byte(in[:split])), d.feed([]byte(in[split:]))...)
		if s := codes(got); s != "paste:q:finish\n\x1b[Aline\x03 r:x" {
			t.Fatalf("split %d: %q", split, s)
		}
	}
	d = decoder{}
	got := d.feed([]byte("\x1b[200~" + strings.Repeat("é", maxPaste) + "\x1b[201~"))
	if len(got) != 1 || !got[0].Truncated || len(got[0].Text) > maxPaste || !strings.HasSuffix(got[0].Text, "é") {
		t.Fatalf("oversized paste: truncated=%v len=%d", got[0].Truncated, len(got[0].Text))
	}
}

// Unknown sequences and stray control bytes are consumed, never typed.
func TestDecoderIgnoresUnknown(t *testing.T) {
	var d decoder
	if got := codes(d.feed([]byte("\x1b[99~\x1b[?1;2c\x01\x1b[200;x"))); got != "" {
		t.Fatalf("unknown = %q", got)
	}
}
