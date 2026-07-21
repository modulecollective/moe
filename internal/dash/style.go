package dash

import (
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
)

// Colour for the dash's two art blocks. Both blocks are built once and
// consumed twice — CLI Render here, and the serve templates' <pre> — so
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
)

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
			out[i] = spanLine(l, func(_ int, r rune) Band {
				switch {
				case r == ' ':
					return BandNone
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

// FactorySpans classifies the built factory art. Classification is
// positional — the builder's line shapes are the contract:
//
//	1 line  → the empty-state dotted row: all chrome.
//	3 lines → [drift wisps, base puffs, rail].
//
// A running station reads as a lit segment venting plasma exhaust that
// cools to amber sparks as it rises; idle stations and goods sit
// mid-amber; the rail recedes into dim chrome.
func FactorySpans(lines []string) [][]Span {
	out := make([][]Span, len(lines))
	if len(lines) != 3 {
		for i, l := range lines {
			out[i] = spanLine(l, nonSpaceBand(BandDim))
		}
		return out
	}
	out[0] = spanLine(lines[0], nonSpaceBand(BandMid))
	out[1] = spanLine(lines[1], nonSpaceBand(BandPlasma))
	out[2] = spanLine(lines[2], railBand([]rune(lines[1]), []rune(lines[2])))
	return out
}

// railBand classifies each rune of the rail row. base is the basePuff
// row, read to tell a running station from a parked one: basePuff fires
// with p=1.0 at a running station's chimney, and the chimney is the
// station glyph's own column, so a non-space directly above a bracketed
// glyph means the station below it is live.
//
// Bracket context is checked before the glyph itself because the
// station set overlaps the rail's furniture — ▶ is also the feed arrow,
// ▦ also a backlog input. Everything the switch doesn't claim (rail
// filler, brackets, the feed arrow, +N overflow tags) is chrome.
func railBand(base, rail []rune) func(int, rune) Band {
	return func(col int, r rune) Band {
		if r == ' ' {
			return BandNone
		}
		if col > 0 && rail[col-1] == '[' {
			if col < len(base) && base[col] != ' ' {
				return BandBright
			}
			return BandMid
		}
		switch string(r) {
		case inputGlyph, outputGlyph:
			return BandMid
		}
		return BandDim
	}
}

// sgr is the terminal escape for a band — the CLI half of the band
// mapping. BandNone has none; paintSpans resets instead.
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

// styleHistogramANSI and styleFactoryANSI are the CLI's consumers of the
// classification above: bands in, escapes out. Render calls them behind
// cliout.Enabled, so NO_COLOR still yields the plain built lines.
func styleHistogramANSI(lines []string) []string { return paintLines(HistogramSpans(lines)) }

func styleFactoryANSI(lines []string) []string { return paintLines(FactorySpans(lines)) }
