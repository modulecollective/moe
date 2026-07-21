package dash

import (
	"math/rand"
	"strings"
	"testing"

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

	plain := BuildActivityHistogram(counts)
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
	plain := BuildActivityHistogram(make([]int, HistDays))
	spans := HistogramSpans(plain)
	assertPlainRoundTrip(t, plain, spans)
	wantAllNonSpace(t, flatten(spans[0]), BandMid, "quiet line")
}

// TestFactorySpans pins the factory's heat mapping on a known-shape
// rail: a live station lit bright over plasma exhaust, a parked one in
// mid amber, goods mid, and the rail itself dim chrome. The two station
// glyphs differ, so bracket context is exercised alongside the
// base-row liveness read.
func TestFactorySpans(t *testing.T) {
	state := FactoryState{
		BacklogCount: 2,
		ActiveStages: []ActiveStation{
			{Stage: "design", RunningDoc: "design"}, // live → ⚒
			{Stage: "code"},                         // parked → ⚙
		},
		CompletedCount: 1,
	}
	// Seed 5 draws two drift wisps, so the decorative row has something
	// to assert; the base row's puff is p=1.0 at every live chimney and
	// doesn't depend on the seed.
	plain := BuildFactoryArt(state, ArtWidth, rand.New(rand.NewSource(5)))
	if len(plain) != 3 {
		t.Fatalf("art line count = %d, want 3", len(plain))
	}
	spans := FactorySpans(plain)
	assertPlainRoundTrip(t, plain, spans)

	wantAllNonSpace(t, flatten(spans[0]), BandMid, "drift row")
	wantAllNonSpace(t, flatten(spans[1]), BandPlasma, "base row")

	rail := flatten(spans[2])
	wantBand(t, rail, '⚒', BandBright, "running station")
	wantBand(t, rail, '⚙', BandMid, "parked station")
	wantBand(t, rail, '▦', BandMid, "backlog input")
	wantBand(t, rail, '▣', BandMid, "completed output")
	wantBand(t, rail, '━', BandDim, "rail filler")
	wantBand(t, rail, '▶', BandDim, "feed arrow")
	wantBand(t, rail, '[', BandDim, "bracket")
}

// TestFactorySpansEmpty: the one-line empty state is all chrome —
// there's no heat to show.
func TestFactorySpansEmpty(t *testing.T) {
	plain := BuildFactoryArt(FactoryState{}, ArtWidth, rand.New(rand.NewSource(1)))
	if len(plain) != 1 {
		t.Fatalf("empty art line count = %d, want 1", len(plain))
	}
	spans := FactorySpans(plain)
	assertPlainRoundTrip(t, plain, spans)
	wantAllNonSpace(t, flatten(spans[0]), BandDim, "empty dots")
}

// TestFactorySpansOverloadedStationGlyph: ▶ is both the feed arrow
// (chrome) and the "awaiting merge" station glyph (a station). Nothing
// about the rune tells them apart — only the bracket to its left does.
// A rail carrying both readings at once pins that lookback.
func TestFactorySpansOverloadedStationGlyph(t *testing.T) {
	state := FactoryState{
		BacklogCount: 1,                                          // "▦ ▶" — input, then the feed arrow
		ActiveStages: []ActiveStation{{Stage: "awaiting merge"}}, // parked "[▶]"
	}
	plain := BuildFactoryArt(state, ArtWidth, rand.New(rand.NewSource(2)))
	spans := FactorySpans(plain)
	assertPlainRoundTrip(t, plain, spans)

	rail := flatten(spans[2])
	var feeds, stations int
	for i, p := range rail {
		if p.r != '▶' {
			continue
		}
		if i > 0 && rail[i-1].r == '[' {
			stations++
			if p.band != BandMid {
				t.Errorf("bracketed ▶ (parked station) banded %v, want mid", p.band)
			}
			continue
		}
		feeds++
		if p.band != BandDim {
			t.Errorf("bare ▶ (feed arrow) banded %v, want dim", p.band)
		}
	}
	if feeds != 1 || stations != 1 {
		t.Fatalf("rail %q: %d feed arrows, %d stations; want 1 and 1", plain[2], feeds, stations)
	}
}

// TestStyleANSIMapsBands is the CLI half of the split: the ANSI stylers
// must put each band's own escape on each rune the classifier banded,
// leave BandNone bare, and add nothing else — so the styled lines strip
// back to the built ones. Walking a real render rune by rune pins the
// whole table at once.
func TestStyleANSIMapsBands(t *testing.T) {
	counts := make([]int, HistDays)
	counts[0] = 24
	state := FactoryState{
		BacklogCount:   2,
		ActiveStages:   []ActiveStation{{Stage: "design", RunningDoc: "design"}, {Stage: "code"}},
		CompletedCount: 1,
	}

	histPlain := BuildActivityHistogram(counts)
	factPlain := BuildFactoryArt(state, ArtWidth, rand.New(rand.NewSource(5)))

	for _, tc := range []struct {
		name  string
		spans [][]Span
		style []string
	}{
		{"histogram", HistogramSpans(histPlain), styleHistogramANSI(histPlain)},
		{"factory", FactorySpans(factPlain), styleFactoryANSI(factPlain)},
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
			t.Fatalf("unterminated SGR in %q", s)
		}
		if seq := string(rs[i : j+1]); seq == cliout.Reset {
			cur = ""
		} else {
			cur = seq
		}
		i = j + 1
	}
	return out
}
