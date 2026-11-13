package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func typeKeys(e *editor, ks ...Key) {
	for _, k := range ks {
		e.key(k)
	}
}

func runes(s string) []Key {
	var ks []Key
	for _, r := range s {
		ks = append(ks, Key{Code: KeyRune, Rune: r})
	}
	return ks
}

func TestEditorEditsKeepUTF8(t *testing.T) {
	e := newEditor("", false)
	typeKeys(e, runes("héllo 😀")...)
	typeKeys(e, Key{Code: KeyBackspace}, Key{Code: KeyLeft}, Key{Code: KeyLeft}, Key{Code: KeyDelete})
	if got := e.String(); got != "héll " || !utf8.ValidString(got) {
		t.Fatalf("text = %q", got)
	}
	typeKeys(e, Key{Code: KeyHome}, Key{Code: KeyRune, Rune: '>'}, Key{Code: KeyEnd}, Key{Code: KeyEnter})
	typeKeys(e, Key{Code: KeyPaste, Text: "two\nthree"})
	if got := e.String(); got != ">héll \ntwo\nthree" {
		t.Fatalf("text = %q", got)
	}
	typeKeys(e, Key{Code: KeyUp}, Key{Code: KeyHome}, Key{Code: KeyRune, Rune: '*'})
	if got := e.String(); got != ">héll \n*two\nthree" {
		t.Fatalf("after up = %q", got)
	}
}

func TestEditorLayoutWrapsAndShowsControls(t *testing.T) {
	e := newEditor("abcdef\x1b[2Jx\nz", false)
	rows, r, c := e.layout(4)
	want := []string{"abcd", "ef^[", "[2Jx", "z"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q", rows)
	}
	if r != 3 || c != 1 {
		t.Fatalf("cursor = %d,%d", r, c)
	}
	for _, row := range rows {
		if strings.ContainsRune(row, 0x1b) {
			t.Fatal("escape painted raw")
		}
	}
	// Wide runes never straddle the margin.
	e = newEditor("a😀😀", false)
	if rows, _, _ := e.layout(4); strings.Join(rows, "|") != "a😀|😀" {
		t.Fatalf("wide rows = %q", rows)
	}
}

func TestEditorSingleLineAndLimit(t *testing.T) {
	e := newEditor("", true)
	if e.key(Key{Code: KeyEnter}) {
		t.Fatal("single-line editor took Enter")
	}
	e.key(Key{Code: KeyPaste, Text: "new idea\nmoe/x"})
	if e.String() != "new idea moe/x" {
		t.Fatalf("single = %q", e.String())
	}
	big := newEditor(strings.Repeat("a", maxText), false)
	if big.key(Key{Code: KeyRune, Rune: 'b'}) || len(big.String()) != maxText {
		t.Fatal("insert past the limit accepted")
	}
}

func TestEditorWordKeys(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, tc := range []struct {
			text              string
			at, back, forward int
		}{
			{"héllo 世界/p-x  tail", 13, 6, 18},
			{"one\n\t two", 7, 6, 9},
			{"one two", 0, 0, 3}, {"one two", 5, 4, 7}, {"one two", 7, 4, 7},
			{"one\n\t two", 6, 0, 9},
		} {
			for _, code := range []KeyCode{KeyWordLeft, KeyWordRight, KeyWordBackspace} {
				e := newEditor(tc.text, single)
				e.cur = tc.at
				e.key(Key{Code: code})
				want := tc.back
				text := tc.text
				if code == KeyWordRight {
					want = tc.forward
				}
				if code == KeyWordBackspace {
					rs := []rune(text)
					text = string(rs[:tc.back]) + string(rs[tc.at:])
				}
				if e.cur != want || e.String() != text || !utf8.ValidString(e.String()) {
					t.Fatalf("%q at %d code %d: cur=%d text=%q; want %d %q", tc.text, tc.at, code, e.cur, e.String(), want, text)
				}
			}
		}
	}
}
