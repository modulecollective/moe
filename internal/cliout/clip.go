package cliout

import (
	"io"
	"unicode"
	"unicode/utf8"
)

// The terminal width policy for moe ui: how many cells a rune takes, and
// how to clip a line that may carry CSI sequences to a column budget
// without emitting a partial sequence. Approximate by design: grapheme
// clusters beyond the cases below are not modelled.

// ClipLine writes line clipped to columns cells. Complete CSI sequences
// pass through at zero width; a partial one ends the line.
func ClipLine(w io.Writer, line []byte, columns int) error {
	cells := 0
	clipped := false
	for len(line) > 0 {
		if n := CSILen(line); n > 0 {
			if err := writeBytes(w, line[:n]); err != nil {
				return err
			}
			line = line[n:]
			continue
		}
		if len(line) >= 2 && line[0] == '\x1b' && line[1] == '[' {
			// Never pass through a partial CSI sequence.
			break
		}

		size, width := TextUnit(line)
		if !clipped && width > 0 && cells+width > columns {
			clipped = true
		}
		if !clipped {
			if err := writeBytes(w, line[:size]); err != nil {
				return err
			}
			cells += width
		}
		line = line[size:]
	}
	return nil
}

// TextUnit is the leading text unit of p — a rune plus any presentation
// selector or keycap mark it carries — and the cells it occupies.
func TextUnit(p []byte) (size, width int) {
	r, size := utf8.DecodeRune(p)
	width = RuneWidth(r)
	rest := p[size:]

	// Emoji presentation turns otherwise narrow symbols (for example ❤)
	// into two-cell glyphs. Keep the selector with its base so clipping
	// cannot emit a sequence whose width was decided only after the base.
	if next, nextSize := utf8.DecodeRune(rest); next == '\ufe0e' || next == '\ufe0f' {
		size += nextSize
		rest = rest[nextSize:]
		if next == '\ufe0f' && width == 1 {
			width = 2
		}
	}
	// Keycaps are two cells whether or not their optional emoji-presentation
	// selector is present.
	if isKeycapBase(r) {
		if next, nextSize := utf8.DecodeRune(rest); next == '\u20e3' {
			size += nextSize
			width = 2
		}
	}
	return size, width
}

func isKeycapBase(r rune) bool {
	return r == '#' || r == '*' || r >= '0' && r <= '9'
}

// CSILen is the length of the complete CSI sequence p starts with, or 0.
func CSILen(p []byte) int {
	if len(p) < 3 || p[0] != '\x1b' || p[1] != '[' {
		return 0
	}
	for i := 2; i < len(p); i++ {
		if p[i] >= 0x40 && p[i] <= 0x7e {
			return i + 1
		}
	}
	return 0
}

func writeBytes(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// RuneWidth is the cells r occupies: 0, 1 or 2.
func RuneWidth(r rune) int {
	if r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Mn, r) ||
		unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) ||
		r >= 0x1f3fb && r <= 0x1f3ff ||
		r >= 0xe0100 && r <= 0xe01ef {
		return 0
	}
	if isWideTerminalRune(r) {
		return 2
	}
	return 1
}

// isWideTerminalRune is the compact width table this viewport needs. It
// covers East Asian wide/full-width blocks and the emoji ranges terminals
// conventionally render as two cells; ambiguous dashboard glyphs such as box
// drawing, block elements, and ▶ deliberately remain one cell.
func isWideTerminalRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r == 0x2329 || r == 0x232a,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r == 0x1f004 || r == 0x1f0cf || r == 0x1f18e,
		r >= 0x1f191 && r <= 0x1f19a,
		r >= 0x1f200 && r <= 0x1f202,
		r >= 0x1f210 && r <= 0x1f23b,
		r >= 0x1f240 && r <= 0x1f248,
		r >= 0x1f250 && r <= 0x1f251,
		r >= 0x1f300 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	switch r {
	case 0x231a, 0x231b, 0x23e9, 0x23ea, 0x23eb, 0x23ec, 0x23f0, 0x23f3,
		0x25fd, 0x25fe, 0x2614, 0x2615, 0x2648, 0x2649, 0x264a, 0x264b,
		0x264c, 0x264d, 0x264e, 0x264f, 0x2650, 0x2651, 0x2652, 0x2653,
		0x267f, 0x2693, 0x26a1, 0x26aa, 0x26ab, 0x26bd, 0x26be, 0x26c4,
		0x26c5, 0x26ce, 0x26d4, 0x26ea, 0x26f2, 0x26f3, 0x26f5, 0x26fa,
		0x26fd, 0x2705, 0x270a, 0x270b, 0x2728, 0x274c, 0x274e, 0x2753,
		0x2754, 0x2755, 0x2757, 0x2795, 0x2796, 0x2797, 0x27b0, 0x27bf,
		0x2b1b, 0x2b1c, 0x2b50, 0x2b55:
		return true
	}
	return false
}
