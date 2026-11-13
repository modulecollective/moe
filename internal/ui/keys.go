package ui

import (
	"bytes"
	"unicode/utf8"
)

// Key is one decoded keystroke or paste.
type Key struct {
	Code KeyCode
	Rune rune   // for KeyRune
	Text string // for KeyPaste
	// Truncated is set on a paste longer than maxPaste: Text holds the
	// first maxPaste bytes and the rest was dropped, which the caller
	// must say rather than pretend the paste was whole.
	Truncated bool
}

// KeyCode names a key the console acts on.
type KeyCode int

const (
	KeyNone KeyCode = iota
	KeyRune
	KeyPaste
	KeyEnter
	KeyTab
	KeyBackTab
	KeyBackspace
	KeyDelete
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyEsc
	KeyCtrlC
	KeyCtrlS
	KeyCtrlX
	KeyCtrlZ
	KeyCtrlL
	KeyWordLeft
	KeyWordRight
	KeyWordBackspace
)

// maxPaste bounds one bracketed paste. Notes and captures are short; a
// paste past this is reported, never silently cut.
const maxPaste = 256 << 10

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// decoder turns terminal input bytes into Keys. Input arrives in
// arbitrary fragments: an escape sequence or a UTF-8 rune split across
// reads is held until the rest arrives. A lone Escape is ambiguous until
// the caller has waited a moment and calls flush.
type decoder struct {
	buf     []byte
	pasting bool
	paste   []byte
	trunc   bool
}

// feed appends p and returns every key now complete.
func (d *decoder) feed(p []byte) []Key {
	d.buf = append(d.buf, p...)
	var out []Key
	for len(d.buf) > 0 {
		if d.pasting {
			k, ok := d.takePaste()
			if !ok {
				break
			}
			out = append(out, k)
			continue
		}
		k, n := decodeOne(d.buf)
		if n == 0 {
			break // incomplete
		}
		d.buf = d.buf[n:]
		if k.Code == KeyPaste { // the start marker
			d.pasting, d.paste, d.trunc = true, nil, false
			continue
		}
		if k.Code != KeyNone {
			out = append(out, k)
		}
	}
	return out
}

// takePaste moves bytes into the paste until the end marker. Paste is
// text, never keys: nothing inside it is decoded.
func (d *decoder) takePaste() (Key, bool) {
	if i := bytes.Index(d.buf, pasteEnd); i >= 0 {
		d.addPaste(d.buf[:i])
		d.buf = d.buf[i+len(pasteEnd):]
		d.pasting = false
		return Key{Code: KeyPaste, Text: pasteText(d.paste), Truncated: d.trunc}, true
	}
	// Keep a possible partial end marker for the next read.
	keep := 0
	for k := len(pasteEnd) - 1; k > 0; k-- {
		if bytes.HasSuffix(d.buf, pasteEnd[:k]) {
			keep = k
			break
		}
	}
	d.addPaste(d.buf[:len(d.buf)-keep])
	d.buf = d.buf[len(d.buf)-keep:]
	return Key{}, false
}

func (d *decoder) addPaste(p []byte) {
	room := maxPaste - len(d.paste)
	if len(p) > room {
		p, d.trunc = p[:max(room, 0)], true
	}
	d.paste = append(d.paste, p...)
}

// pasteText normalises line endings and makes the bytes valid UTF-8; a
// paste cut at maxPaste drops a trailing partial rune.
func pasteText(p []byte) string {
	p = bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	p = bytes.ReplaceAll(p, []byte("\r"), []byte("\n"))
	for len(p) > 0 {
		r, n := utf8.DecodeLastRune(p)
		if r != utf8.RuneError || n != 1 || utf8.FullRune(p[len(p)-n:]) {
			break
		}
		p = p[:len(p)-1]
	}
	return string(bytes.ToValidUTF8(p, []byte("�")))
}

// flush resolves input that cannot complete: a lone Escape (or any
// partial sequence) after the caller's wait. A partial sequence reads as
// Escape; its remainder is dropped rather than typed as text.
func (d *decoder) flush() []Key {
	if d.pasting || len(d.buf) == 0 {
		return nil
	}
	b := d.buf
	d.buf = nil
	if len(b) == 1 && b[0] == 0x1b {
		return []Key{{Code: KeyEsc}}
	}
	if b[0] == 0x1b {
		return nil // partial modified input must not become a shortcut
	}
	return []Key{{Code: KeyRune, Rune: utf8.RuneError}}
}

// pending reports input waiting on more bytes or a flush.
func (d *decoder) pending() bool { return len(d.buf) > 0 && !d.pasting }

// decodeOne decodes the key at the head of b, returning how many bytes
// it used; 0 means b holds only an incomplete key. A KeyNone with n > 0
// is input consumed and ignored (an unknown sequence).
func decodeOne(b []byte) (Key, int) {
	c := b[0]
	switch {
	case c == 0x1b:
		return decodeEscape(b)
	case c == '\r' || c == '\n':
		return Key{Code: KeyEnter}, 1
	case c == '\t':
		return Key{Code: KeyTab}, 1
	case c == 0x7f || c == 0x08:
		return Key{Code: KeyBackspace}, 1
	case c == 0x03:
		return Key{Code: KeyCtrlC}, 1
	case c == 0x13:
		return Key{Code: KeyCtrlS}, 1
	case c == 0x18:
		return Key{Code: KeyCtrlX}, 1
	case c == 0x1a:
		return Key{Code: KeyCtrlZ}, 1
	case c == 0x0c:
		return Key{Code: KeyCtrlL}, 1
	case c == 0x17:
		return Key{Code: KeyWordBackspace}, 1
	case c < 0x20:
		return Key{}, 1
	}
	if !utf8.FullRune(b) {
		return Key{}, 0
	}
	r, n := utf8.DecodeRune(b)
	return Key{Code: KeyRune, Rune: r}, n
}

func decodeEscape(b []byte) (Key, int) {
	if len(b) == 1 {
		return Key{}, 0
	}
	switch b[1] {
	case '[':
		return decodeCSI(b)
	case 'O':
		if len(b) < 3 {
			return Key{}, 0
		}
		if k, ok := finalKey(b[2]); ok {
			return Key{Code: k}, 3
		}
		return Key{}, 3
	case 0x1b:
		// Escape then something: the first one is a key of its own.
		return Key{Code: KeyEsc}, 1
	case 0x7f, 0x08:
		return Key{Code: KeyWordBackspace}, 2
	case 'b':
		return Key{Code: KeyWordLeft}, 2
	case 'f':
		return Key{Code: KeyWordRight}, 2
	}
	// Unsupported Meta input is consumed whole, including a split rune.
	if !utf8.FullRune(b[1:]) {
		return Key{}, 0
	}
	_, n := utf8.DecodeRune(b[1:])
	return Key{}, 1 + n
}

// decodeCSI reads ESC [ params final.
func decodeCSI(b []byte) (Key, int) {
	i := 2
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	if i >= len(b) {
		if i-2 > 32 {
			return Key{}, i // not a key anyone sends; drop it
		}
		return Key{}, 0
	}
	final, params := b[i], string(b[2:i])
	n := i + 1
	if final < 0x40 || final > 0x7e {
		return Key{}, n
	}
	if params == "1;3" {
		switch final {
		case 'D':
			return Key{Code: KeyWordLeft}, n
		case 'C':
			return Key{Code: KeyWordRight}, n
		}
	}
	if params != "" && params != "1" && final != '~' {
		return Key{}, n
	}
	if final == '~' {
		if bytes.ContainsRune([]byte(params), ';') {
			return Key{}, n
		}
		num := params
		switch num {
		case "1", "7":
			return Key{Code: KeyHome}, n
		case "4", "8":
			return Key{Code: KeyEnd}, n
		case "3":
			return Key{Code: KeyDelete}, n
		case "5":
			return Key{Code: KeyPgUp}, n
		case "6":
			return Key{Code: KeyPgDn}, n
		case "200":
			return Key{Code: KeyPaste}, n // start marker; feed handles it
		}
		return Key{}, n
	}
	if final == 'Z' {
		return Key{Code: KeyBackTab}, n
	}
	if k, ok := finalKey(final); ok {
		return Key{Code: k}, n
	}
	return Key{}, n
}

func finalKey(c byte) (KeyCode, bool) {
	switch c {
	case 'A':
		return KeyUp, true
	case 'B':
		return KeyDown, true
	case 'C':
		return KeyRight, true
	case 'D':
		return KeyLeft, true
	case 'H':
		return KeyHome, true
	case 'F':
		return KeyEnd, true
	}
	return KeyNone, false
}

func cutByte(s string, sep byte) (string, string, bool) {
	if i := bytes.IndexByte([]byte(s), sep); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}
