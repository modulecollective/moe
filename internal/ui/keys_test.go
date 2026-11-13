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
				KeyEnter: "enter", KeyShiftEnter: "shift-enter", KeyTab: "tab", KeyBackTab: "backtab", KeyBackspace: "bs",
				KeyDelete: "del", KeyUp: "up", KeyDown: "down", KeyLeft: "left", KeyRight: "right",
				KeyHome: "home", KeyEnd: "end", KeyPgUp: "pgup", KeyPgDn: "pgdn", KeyEsc: "esc",
				KeyCtrlC: "^C", KeyCtrlS: "^S", KeyCtrlX: "^X", KeyCtrlZ: "^Z", KeyCtrlL: "^L", KeyWordLeft: "word-left", KeyWordRight: "word-right", KeyWordBackspace: "word-bs",
			}[k.Code])
		}
	}
	return strings.Join(parts, " ")
}

// Every byte split of a sequence decodes the same as the whole.
func TestDecoderFragments(t *testing.T) {
	in := "a\x1b[A\x1bOB\x1b[C\x1b[3~\x1b[5~\x1b[6~\x1b[H\x1b[4~é😀\r\x7f\t\x1b[Z\x13\x03\x18"
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
	if got := codes(d.feed([]byte("\x1bx"))); got != "" {
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

func TestDecoderWordKeys(t *testing.T) {
	for _, seq := range []struct{ in, want string }{
		{"\x1b\x7f", "word-bs"}, {"\x1b\x08", "word-bs"}, {"\x17", "word-bs"},
		{"\x1b[1;3D", "word-left"}, {"\x1bb", "word-left"},
		{"\x1b[1;3C", "word-right"}, {"\x1bf", "word-right"},
		{"\x1bq", ""}, {"\x1b[1;5C", ""}, {"\x1b[3;3~", ""}, {"\x1bé", ""},
	} {
		for split := range len(seq.in) + 1 {
			var d decoder
			got := append(d.feed([]byte(seq.in[:split])), d.feed([]byte(seq.in[split:]))...)
			if codes(got) != seq.want {
				t.Fatalf("%q split %d: %q", seq.in, split, codes(got))
			}
		}
	}
	var d decoder
	d.feed([]byte("\x1b[1;3"))
	if got := d.flush(); len(got) != 0 {
		t.Fatalf("partial modified input became %v", got)
	}
	if got := codes(d.feed([]byte("\x1b[200~\x1b\x7f\x17\x1bb\x1b[201~"))); got != "paste:\x1b\x7f\x17\x1bb" {
		t.Fatal(got)
	}
}

func TestDecoderShiftEnter(t *testing.T) {
	for _, seq := range []string{"\x1b[13;2u", "\x1b[27;2;13~"} {
		in := "é" + seq + "x\r\n"
		want := "r:é shift-enter r:x enter enter"
		for split := range len(in) + 1 {
			var d decoder
			got := append(d.feed([]byte(in[:split])), d.feed([]byte(in[split:]))...)
			if codes(got) != want || d.pending() {
				t.Fatalf("%q split %d: %q", seq, split, codes(got))
			}
		}
		var d decoder
		var got []Key
		for i := range len(in) {
			got = append(got, d.feed([]byte{in[i]})...)
		}
		if codes(got) != want || d.pending() {
			t.Fatalf("%q bytewise: %q", seq, codes(got))
		}
		for end := 2; end < len(seq); end++ {
			d = decoder{}
			if got := d.feed([]byte(seq[:end])); len(got) != 0 || !d.pending() {
				t.Fatalf("%q prefix %d decoded early: %v", seq, end, got)
			}
			if got := d.flush(); len(got) != 0 || d.pending() {
				t.Fatalf("%q prefix %d flush: %v", seq, end, got)
			}
			if got := codes(d.feed([]byte("x"))); got != "r:x" {
				t.Fatalf("text after flush: %q", got)
			}
		}
		d = decoder{}
		if got := d.feed([]byte("\x1b[200~" + seq + "\x1b[201~")); len(got) != 1 || got[0].Code != KeyPaste || got[0].Text != seq {
			t.Fatalf("%q paste decoded as keys: %v", seq, got)
		}
	}
}

func TestDecoderRejectsOtherModifiedEnter(t *testing.T) {
	for _, seq := range []string{
		"\x1b[13;3u", "\x1b[13;5u", "\x1b[13;4u", "\x1b[13u",
		"\x1b[27;3;13~", "\x1b[27;5;13~", "\x1b[27;2;14~",
		"\x1b[13;2:3u", "\x1b[13;2:2u", "\x1b[13:13;2u",
	} {
		in := "a" + seq + "x"
		for split := range len(in) + 1 {
			var d decoder
			got := append(d.feed([]byte(in[:split])), d.feed([]byte(in[split:]))...)
			if codes(got) != "r:a r:x" || d.pending() {
				t.Fatalf("%q split %d: %q", seq, split, codes(got))
			}
		}
	}
}
