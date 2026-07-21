package dash

import (
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
)

// Terminal colour for the dash's two art blocks. Both blocks are built
// once and consumed twice — CLI Render here, and the serve templates'
// <pre> — and an escape code must never reach the web. So the builders
// stay plain runes and colour is a pure post-pass over their output,
// applied in Render behind cliout.Enabled. Nothing upstream of these
// functions knows colour exists.
//
// The palette is the operator's tmux theme (see cliout): structure is
// dim chrome, idle things sit mid-amber, live things are lit bright,
// heat is plasma.

// paintLine wraps each rune of s in the SGR colorOf returns for it; ""
// leaves the rune bare. Consecutive runes sharing a colour share one
// escape, so a 60-column bar row costs a handful of sequences rather
// than 120.
func paintLine(s string, colorOf func(col int, r rune) string) string {
	var b strings.Builder
	cur := ""
	for col, r := range []rune(s) {
		if want := colorOf(col, r); want != cur {
			if want == "" {
				b.WriteString(cliout.Reset)
			} else {
				b.WriteString(want)
			}
			cur = want
		}
		b.WriteRune(r)
	}
	if cur != "" {
		b.WriteString(cliout.Reset)
	}
	return b.String()
}

// paintNonSpace is the flat classifier: one colour for every non-space
// rune on the line, nothing for the padding.
func paintNonSpace(color string) func(int, rune) string {
	return func(_ int, r rune) string {
		if r == ' ' {
			return ""
		}
		return color
	}
}

// styleHistogramANSI colours the built activity chart. Heat rises: the
// baseline row is dim ember, the top band bright amber, so a short bar
// stays low and quiet while a tall one climbs into the light. The
// window's peak — the only column that reaches the top row with a full
// block — gets a plasma flare on its cap. Caption (and the cold-state
// "(quiet)" line) go mid-amber: the strip commits to the theme as an
// ornament rather than reading as body text.
//
// Deliberate tradeoff: a one-cell bar is dim-on-black and recedes.
// Quiet days should. The alternative — colouring each column by its own
// height — paints every bar one flat colour and loses the within-bar
// gradient that makes the strip read as flame.
func styleHistogramANSI(lines []string) []string {
	if len(lines) < 2 {
		// Cold state: the single "(quiet)" line.
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = paintLine(l, paintNonSpace(cliout.Mid))
		}
		return out
	}
	rows := len(lines) - 2 // bar rows, then a blank spacer, then the caption
	out := make([]string, len(lines))
	for i, l := range lines {
		switch {
		case i < rows:
			band, top := histBand(i, rows), i == 0
			out[i] = paintLine(l, func(_ int, r rune) string {
				switch {
				case r == ' ':
					return ""
				case top && r == '█':
					return cliout.Plasma
				default:
					return band
				}
			})
		case i == rows:
			out[i] = l // the spacer is genuinely empty
		default:
			out[i] = paintLine(l, paintNonSpace(cliout.Mid))
		}
	}
	return out
}

// histBand picks the ramp colour for bar row `row` of `rows`, counting
// from the top. HistRows is 3 today — one row per rung — but the spread
// keeps the mapping sane for any height.
func histBand(row, rows int) string {
	ramp := []string{cliout.Dim, cliout.Mid, cliout.Bright}
	i := (rows - 1 - row) * len(ramp) / rows
	if i >= len(ramp) {
		i = len(ramp) - 1
	}
	return ramp[i]
}

// styleFactoryANSI colours the built factory art. Classification is
// positional — the builder's line shapes are the contract:
//
//	1 line  → the empty-state dotted row: all chrome.
//	3 lines → [drift wisps, base puffs, rail].
//
// A running station reads as a lit segment venting plasma exhaust that
// cools to amber sparks as it rises; idle stations and goods sit
// mid-amber; the rail recedes into dim chrome.
func styleFactoryANSI(lines []string) []string {
	out := make([]string, len(lines))
	if len(lines) != 3 {
		for i, l := range lines {
			out[i] = paintLine(l, paintNonSpace(cliout.Dim))
		}
		return out
	}
	out[0] = paintLine(lines[0], paintNonSpace(cliout.Mid))
	out[1] = paintLine(lines[1], paintNonSpace(cliout.Plasma))
	out[2] = paintLine(lines[2], railColor([]rune(lines[1]), []rune(lines[2])))
	return out
}

// railColor classifies each rune of the rail row. base is the basePuff
// row, read to tell a running station from a parked one: basePuff fires
// with p=1.0 at a running station's chimney, and the chimney is the
// station glyph's own column, so a non-space directly above a bracketed
// glyph means the station below it is live.
//
// Bracket context is checked before the glyph itself because the
// station set overlaps the rail's furniture — ▶ is also the feed arrow,
// ▦ also a backlog input. Everything the switch doesn't claim (rail
// filler, brackets, the feed arrow, +N overflow tags) is chrome.
func railColor(base, rail []rune) func(int, rune) string {
	return func(col int, r rune) string {
		if r == ' ' {
			return ""
		}
		if col > 0 && rail[col-1] == '[' {
			if col < len(base) && base[col] != ' ' {
				return cliout.Bright
			}
			return cliout.Mid
		}
		switch string(r) {
		case inputGlyph, outputGlyph:
			return cliout.Mid
		}
		return cliout.Dim
	}
}
