package dash

import (
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/cliout"
)

// Colour for the dash's activity chart. It is built once and consumed
// twice — CLI Render here, and the serve templates' <pre> — so
// the builders stay plain runes and classification is a pure post-pass
// over their output. That post-pass yields semantic bands, not escapes:
// the CLI maps bands to SGR behind cliout.Enabled, serve maps the same
// bands to CSS classes. Nothing upstream of these functions knows colour
// exists, and the two surfaces can't drift on what counts as live, idle
// or chrome — the positional rules are written once, here.
//
// The palette is the operator's tmux theme (see cliout): structure is
// dim chrome, idle things sit mid-amber, live things are lit bright,
// heat is plasma.

// Band is the emphasis a rune gets, named for what it means rather than
// for a colour: the ramp is visibility-monotonic, so the web's light
// theme reverses the same hexes and Bright is the *darkest* one there.
// BandNone is unpainted — padding, which must never drag an escape into
// a column the layout counts.
type Band int

const (
	BandNone Band = iota
	BandDim
	BandMid
	BandBright
	BandPlasma
	// BandStar is today's live star. Its terminal colour follows its glyph
	// (see starTwinkle); serve gives it a class with a CSS twinkle.
	BandStar
)

// starTwinkle is the terminal star's cycle: one frame per step, each a
// one-cell glyph with its band. The rest pose comes back every other
// frame so it reads as a star that glints rather than a slot machine.
// Fixed, no RNG; retune here.
var starTwinkle = []struct {
	glyph rune
	band  Band
}{
	{starRest, BandBright},
	{'✦', BandPlasma},
	{starRest, BandBright},
	{'·', BandMid},
	{starRest, BandBright},
	{'✧', BandPlasma},
}

// isStarGlyph reports whether r is one of the twinkle's glyphs.
func isStarGlyph(r rune) bool {
	for _, s := range starTwinkle {
		if s.glyph == r {
			return true
		}
	}
	return false
}

// Span is a run of consecutive runes sharing one band. The grouping is
// done once here so both consumers inherit it: a 60-column bar row costs
// a handful of escapes (or elements) rather than 120.
type Span struct {
	Text string
	Band Band
}

// spanLine groups the runes of s into spans by the band bandOf returns
// for each. Every rune lands in exactly one span, so concatenating a
// line's span texts reproduces the built line byte for byte.
func spanLine(s string, bandOf func(col int, r rune) Band) []Span {
	var out []Span
	var buf []rune
	cur := BandNone
	flush := func() {
		if len(buf) > 0 {
			out = append(out, Span{Text: string(buf), Band: cur})
			buf = buf[:0]
		}
	}
	for col, r := range []rune(s) {
		if b := bandOf(col, r); b != cur {
			flush()
			cur = b
		}
		buf = append(buf, r)
	}
	flush()
	return out
}

// nonSpaceBand is the flat classifier: one band for every non-space rune
// on the line, nothing for the padding.
func nonSpaceBand(band Band) func(int, rune) Band {
	return func(_ int, r rune) Band {
		if r == ' ' {
			return BandNone
		}
		return band
	}
}

// bandNone leaves a line unpainted. Its runes still come back as one
// BandNone span, so the text round-trips even when the line isn't the
// empty string it's expected to be.
func bandNone(int, rune) Band { return BandNone }

// HistogramSpans classifies the built activity chart. Heat rises: the
// baseline row is dim ember, the top band bright amber, so a short bar
// stays low and quiet while a tall one climbs into the light. The
// window's peak — the only column that reaches the top row with a full
// block — gets a plasma flare on its cap. Caption (and the cold-state
// "(quiet)" line) go mid-amber: the strip commits to the theme as an
// ornament rather than reading as body text.
//
// The live star — a star glyph in the last column of a bar row — is the
// one BandStar cell.
//
// Deliberate tradeoff: a one-cell bar is dim-on-black and recedes.
// Quiet days should. The alternative — classifying each column by its
// own height — paints every bar one flat band and loses the within-bar
// gradient that makes the strip read as flame.
func HistogramSpans(lines []string) [][]Span {
	out := make([][]Span, len(lines))
	if len(lines) < 2 {
		// Cold state: the single "(quiet)" line.
		for i, l := range lines {
			out[i] = spanLine(l, nonSpaceBand(BandMid))
		}
		return out
	}
	rows := len(lines) - 2 // bar rows, then a blank spacer, then the caption
	for i, l := range lines {
		switch {
		case i < rows:
			band, top := histBand(i, rows), i == 0
			last := utf8.RuneCountInString(l) - 1
			out[i] = spanLine(l, func(col int, r rune) Band {
				switch {
				case r == ' ':
					return BandNone
				case col == last && isStarGlyph(r):
					return BandStar
				case top && r == '█':
					return BandPlasma
				default:
					return band
				}
			})
		case i == rows:
			out[i] = spanLine(l, bandNone) // the spacer is genuinely empty
		default:
			out[i] = spanLine(l, nonSpaceBand(BandMid))
		}
	}
	return out
}

// histBand picks the ramp band for bar row `row` of `rows`, counting
// from the top. HistRows is 3 today — one row per rung — but the spread
// keeps the mapping sane for any height.
func histBand(row, rows int) Band {
	ramp := []Band{BandDim, BandMid, BandBright}
	i := (rows - 1 - row) * len(ramp) / rows
	if i >= len(ramp) {
		i = len(ramp) - 1
	}
	return ramp[i]
}

// sgr is the terminal escape for a band — the CLI half of the band
// mapping. BandNone has none; paintSpans resets instead. BandStar has no
// colour of its own: paintSpans asks starSGR for its glyph's.
func (b Band) sgr() string {
	switch b {
	case BandDim:
		return cliout.Dim
	case BandMid:
		return cliout.Mid
	case BandBright:
		return cliout.Bright
	case BandPlasma:
		return cliout.Plasma
	}
	return ""
}

// paintSpans renders one line's spans as SGR-wrapped text. Spans already
// coalesce, so this only ever emits one escape per span plus a closing
// reset.
func paintSpans(spans []Span) string {
	var b strings.Builder
	open := false
	for _, sp := range spans {
		switch {
		case sp.Band == BandNone && open:
			b.WriteString(cliout.Reset)
			open = false
		case sp.Band == BandStar:
			b.WriteString(starSGR(sp.Text))
			open = true
		case sp.Band != BandNone:
			b.WriteString(sp.Band.sgr())
			open = true
		}
		b.WriteString(sp.Text)
	}
	if open {
		b.WriteString(cliout.Reset)
	}
	return b.String()
}

// paintLines is the span→SGR pass over a whole art block.
func paintLines(spans [][]Span) []string {
	out := make([]string, len(spans))
	for i, line := range spans {
		out[i] = paintSpans(line)
	}
	return out
}

// starSGR is the escape for a star cell: the band its glyph has in
// starTwinkle.
func starSGR(text string) string {
	r, _ := utf8.DecodeRuneInString(text)
	for _, s := range starTwinkle {
		if s.glyph == r {
			return s.band.sgr()
		}
	}
	return BandBright.sgr()
}

// styleHistogramANSI is the CLI's consumer of the classification above:
// bands in, escapes out. Callers gate it behind cliout.Enabled, so
// NO_COLOR still yields the plain built lines.
func styleHistogramANSI(lines []string) []string { return paintLines(HistogramSpans(lines)) }

// StyledHistogramFrames is the built chart as moe ui animates it, behind
// the CLI's colour gate: one frame per twinkle step, identical except
// for the star cell's glyph and colour. A chart with no star is one
// frame, so an idle board never repaints. Without colour the glyphs
// still cycle.
func StyledHistogramFrames(w io.Writer, lines []string) [][]string {
	return histogramFrames(lines, cliout.Enabled(w))
}

func histogramFrames(lines []string, color bool) [][]string {
	row, col := starCell(lines)
	n := 1
	if row >= 0 {
		n = len(starTwinkle)
	}
	frames := make([][]string, n)
	for i := range frames {
		f := lines
		if row >= 0 {
			f = slices.Clone(lines)
			rs := []rune(f[row])
			rs[col] = starTwinkle[i].glyph
			f[row] = string(rs)
		}
		if color {
			f = styleHistogramANSI(f)
		}
		frames[i] = f
	}
	return frames
}

// starCell finds the live star in built chart lines: the row and rune
// column of the only BandStar cell, or -1, -1.
func starCell(lines []string) (row, col int) {
	for i, spans := range HistogramSpans(lines) {
		col := 0
		for _, sp := range spans {
			if sp.Band == BandStar {
				return i, col
			}
			col += utf8.RuneCountInString(sp.Text)
		}
	}
	return -1, -1
}
