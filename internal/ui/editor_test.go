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
