package dash

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/cliout"
)

// banded pairs one visible rune with the band classified for it.
type banded struct {
	band Band
	r    rune
}

// flatten walks a line's spans back into rune/band pairs, so a test can
// assert what got which band without pinning where runs coalesced.
func flatten(spans []Span) []banded {
	var out []banded
	for _, sp := range spans {
		for _, r := range sp.Text {
			out = append(out, banded{sp.Band, r})
		}
	}
	return out
}

// plainOf strips classification back to the runes underneath.
func plainOf(bs []banded) string {
	var b strings.Builder
	for _, p := range bs {
		b.WriteRune(p.r)
	}
	return b.String()
}

// wantBand asserts every occurrence of r on the line carries band.
func wantBand(t *testing.T, bs []banded, r rune, band Band, label string) {
	t.Helper()
	seen := false
	for _, p := range bs {
		if p.r != r {
			continue
		}
		seen = true
		if p.band != band {
			t.Errorf("%s: %q banded %v, want %v", label, r, p.band, band)
		}
	}
	if !seen {
		t.Errorf("%s: no %q on the line", label, r)
	}
}

// wantAllNonSpace asserts every non-space rune on the line carries band,
// and that spaces stay BandNone (so padding never drags an escape into a
// column the layout counts).
func wantAllNonSpace(t *testing.T, bs []banded, band Band, label string) {
	t.Helper()
	nonSpace := 0
	for _, p := range bs {
		if p.r == ' ' {
			if p.band != BandNone {
				t.Errorf("%s: padding space banded %v", label, p.band)
			}
			continue
		}
		nonSpace++
		if p.band != band {
			t.Errorf("%s: %q banded %v, want %v", label, p.r, p.band, band)
		}
	}
	if nonSpace == 0 {
		t.Errorf("%s: nothing to classify on the line", label)
	}
}

// assertPlainRoundTrip is the invariant that keeps classification
// honest: spans only partition, so concatenating them yields the built
// lines byte for byte. Column widths, the gutter, the rail's alignment,
// and — once serve wraps spans in markup — the web's copy-paste text are
// all downstream of that.
func assertPlainRoundTrip(t *testing.T, plain []string, spans [][]Span) {
	t.Helper()
	if len(spans) != len(plain) {
		t.Fatalf("span line count = %d, want %d", len(spans), len(plain))
	}
	for i := range plain {
		if got := plainOf(flatten(spans[i])); got != plain[i] {
			t.Errorf("line %d flattens to %q, want %q", i, got, plain[i])
		}
	}
}

// TestHistogramSpans pins the heat gradient: bar rows ramp
// dim→mid→bright bottom to top, the window peak's cap flares plasma,
// and the caption reads as part of the ornament.
func TestHistogramSpans(t *testing.T) {
	counts := make([]int, HistDays)
	counts[0] = 24 // the window max: full height, so its top cell is █
	counts[1] = 1  // a one-cell bar down on the baseline

	plain := BuildActivityHistogram(counts, false)
	spans := HistogramSpans(plain)
	assertPlainRoundTrip(t, plain, spans)

	gutter := len([]rune(histGutter))
	at := func(row, day int) banded {
		return flatten(spans[row])[gutter+day]
	}

	// Peak column: plasma cap on the top row, then bright/mid/dim grain
	// as the bar descends through the bands.
	if p := at(0, 0); p.r != '█' || p.band != BandPlasma {
		t.Errorf("peak cap = %q/%v, want █/plasma", p.r, p.band)
	}
	if p := at(1, 0); p.r != barInterior || p.band != BandMid {
		t.Errorf("peak middle = %q/%v, want %q/mid", p.r, p.band, barInterior)
	}
	if p := at(2, 0); p.r != barInterior || p.band != BandDim {
		t.Errorf("peak base = %q/%v, want %q/dim", p.r, p.band, barInterior)
	}

	// A quiet day sits ember-dim on the baseline and doesn't steal the
	// peak's flare.
	if p := at(HistRows-1, 1); p.band != BandDim {
		t.Errorf("one-cell bar banded %v, want dim", p.band)
	}

	if got := plainOf(flatten(spans[HistRows])); got != "" {
		t.Errorf("spacer = %q, want it left empty", got)
	}
	wantBand(t, flatten(spans[HistRows+1]), 'a', BandMid, "caption") // "activity · last N days"
}

// TestHistogramSpansQuiet: the cold-state collapse is a single
// caption-ish line, so it takes the caption's band.
func TestHistogramSpansQuiet(t *testing.T) {
	plain := BuildActivityHistogram(make([]int, HistDays), false)
	spans := HistogramSpans(plain)
	assertPlainRoundTrip(t, plain, spans)
	wantAllNonSpace(t, flatten(spans[0]), BandMid, "quiet line")
}

// TestHistogramSpansStar: the live star is the chart's only BandStar
// cell, it sits in the last column, and idle charts have none.
func TestHistogramSpansStar(t *testing.T) {
	for name, counts := range histFixtures() {
		for _, live := range []bool{false, true} {
			plain := BuildActivityHistogram(counts, live)
			spans := HistogramSpans(plain)
			assertPlainRoundTrip(t, plain, spans)
			stars := 0
			for _, line := range spans {
				bs := flatten(line)
				for c, p := range bs {
					if p.band != BandStar {
						continue
					}
					stars++
					if c != len(bs)-1 || p.r != starRest {
						t.Errorf("%s: star %q at col %d of %d", name, p.r, c, len(bs))
					}
				}
			}
			if want := map[bool]int{false: 0, true: 1}[live]; stars != want {
				t.Errorf("%s live=%v: %d star spans, want %d", name, live, stars, want)
			}
		}
	}
}

// TestStyledHistogramFrames: a live chart animates through the twinkle
// table, every frame the same shape and differing from frame 0 only in
// the star cell; an idle chart is a single still frame. Colour or not,
// the glyphs cycle.
func TestStyledHistogramFrames(t *testing.T) {
	counts := histFixtures()["tall"]
	if f := StyledHistogramFrames(&bytes.Buffer{}, BuildActivityHistogram(counts, false)); len(f) != 1 {
		t.Fatalf("idle: %d frames, want 1", len(f))
	}
	plain := BuildActivityHistogram(counts, true)
	starRow := -1
	for r, l := range plain[:HistRows] {
		if c, _ := utf8.DecodeLastRuneInString(l); c == starRest {
			starRow = r
		}
	}
	for _, color := range []bool{false, true} {
		t.Run(fmt.Sprint("color=", color), func(t *testing.T) {
			frames := histogramFrames(plain, color)
			if len(frames) != len(starTwinkle) {
				t.Fatalf("%d frames, want %d", len(frames), len(starTwinkle))
			}
			if frames[0][starRow] == frames[1][starRow] {
				t.Errorf("frames 0 and 1 share the star row %q", frames[0][starRow])
			}
			base := stripAll(frames[0])
			for i, f := range frames {
				got := stripAll(f)
				if len(got) != len(base) {
					t.Fatalf("frame %d: %d lines, want %d", i, len(got), len(base))
				}
				for r := range got {
					a, b := []rune(base[r]), []rune(got[r])
					if len(a) != len(b) {
						t.Fatalf("frame %d line %d: %d runes, want %d", i, r, len(b), len(a))
					}
					for c := range a {
						if a[c] != b[c] && (r != starRow || c != len(a)-1) {
							t.Errorf("frame %d differs at line %d col %d", i, r, c)
						}
					}
				}
				last := []rune(got[starRow])
				if g := last[len(last)-1]; g != starTwinkle[i].glyph {
					t.Errorf("frame %d star = %q, want %q", i, g, starTwinkle[i].glyph)
				}
				if color {
					p := parsePainted(t, f[starRow])
					if c := p[len(p)-1].color; c != sgrFor(starTwinkle[i].band) {
						t.Errorf("frame %d star colour %q, want %q", i, c, sgrFor(starTwinkle[i].band))
					}
				}
			}
			if !color && strings.Contains(strings.Join(frames[0], ""), "\x1b") {
				t.Error("uncoloured frames carry escapes")
			}
		})
	}
	for i, s := range starTwinkle {
		if i%2 == 0 && s.glyph != starRest {
			t.Errorf("step %d is %q: the rest pose must return every other frame", i, s.glyph)
		}
	}
}

// stripAll removes SGR escapes from each line.
func stripAll(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		var b strings.Builder
		for _, p := range parsePaintedNoT(l) {
			b.WriteRune(p.r)
		}
		out[i] = b.String()
	}
	return out
}

// TestStyleANSIMapsBands is the CLI half of the split: the ANSI stylers
// must put each band's own escape on each rune the classifier banded,
// leave BandNone bare, and add nothing else — so the styled lines strip
// back to the built ones. Walking a real render rune by rune pins the
// whole table at once.
func TestStyleANSIMapsBands(t *testing.T) {
	counts := make([]int, HistDays)
	counts[0] = 24
	counts[HistDays-1] = 5

	histPlain := BuildActivityHistogram(counts, false)
	livePlain := BuildActivityHistogram(counts, true)

	for _, tc := range []struct {
		name  string
		spans [][]Span
		style []string
	}{
		{"histogram", HistogramSpans(histPlain), styleHistogramANSI(histPlain)},
		{"live", HistogramSpans(livePlain), styleHistogramANSI(livePlain)},
	} {
		if len(tc.style) != len(tc.spans) {
			t.Fatalf("%s: styled line count = %d, want %d", tc.name, len(tc.style), len(tc.spans))
		}
		for i := range tc.spans {
			got, want := parsePainted(t, tc.style[i]), flatten(tc.spans[i])
			if len(got) != len(want) {
				t.Fatalf("%s line %d: %d styled runes, want %d", tc.name, i, len(got), len(want))
			}
			for j, p := range got {
				if p.r != want[j].r {
					t.Fatalf("%s line %d: styled rune %d = %q, want %q", tc.name, i, j, p.r, want[j].r)
				}
				if p.color != sgrFor(want[j].band) {
					t.Errorf("%s line %d: %q coloured %q, want %q (%v)",
						tc.name, i, p.r, p.color, sgrFor(want[j].band), want[j].band)
				}
			}
		}
	}
}

// sgrFor spells the band→escape mapping out longhand rather than calling
// Band.sgr, so the test pins the table instead of restating it.
func sgrFor(b Band) string {
	switch b {
	case BandDim:
		return cliout.Dim
	case BandMid:
		return cliout.Mid
	case BandBright:
		return cliout.Bright
	case BandPlasma:
		return cliout.Plasma
	case BandStar:
		return cliout.Bright // the rest pose; frames recolour the glint
	}
	return ""
}

// painted pairs one visible rune with the SGR colour in effect for it
// ("" when the rune is bare).
type painted struct {
	color string
	r     rune
}

// parsePainted walks a styled line back into rune/colour pairs, so a
// test can assert what got which colour without pinning where the
// escapes were coalesced.
func parsePainted(t *testing.T, s string) []painted {
	t.Helper()
	out, ok := parsePaintedOK(s)
	if !ok {
		t.Fatalf("unterminated SGR in %q", s)
	}
	return out
}

func parsePaintedNoT(s string) []painted {
	out, _ := parsePaintedOK(s)
	return out
}

func parsePaintedOK(s string) ([]painted, bool) {
	var out []painted
	cur := ""
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if rs[i] != '\x1b' {
			out = append(out, painted{cur, rs[i]})
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] != 'm' {
			j++
		}
		if j == len(rs) {
			return out, false
		}
		if seq := string(rs[i : j+1]); seq == cliout.Reset {
			cur = ""
		} else {
			cur = seq
		}
		i = j + 1
	}
	return out, true
}
