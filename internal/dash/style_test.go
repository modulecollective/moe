package dash

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/cliout"
)

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

// plainOf strips styling back to the runes underneath.
func plainOf(ps []painted) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteRune(p.r)
	}
	return b.String()
}

// wantColor asserts every occurrence of r on the line carries color.
func wantColor(t *testing.T, ps []painted, r rune, color, label string) {
	t.Helper()
	seen := false
	for _, p := range ps {
		if p.r != r {
			continue
		}
		seen = true
		if p.color != color {
			t.Errorf("%s: %q coloured %q, want %q", label, r, p.color, color)
		}
	}
	if !seen {
		t.Errorf("%s: no %q on the line", label, r)
	}
}

// wantAllNonSpace asserts every non-space rune on the line carries
// color, and that spaces stay bare (so padding never drags an escape
// into a column the layout counts).
func wantAllNonSpace(t *testing.T, ps []painted, color, label string) {
	t.Helper()
	nonSpace := 0
	for _, p := range ps {
		if p.r == ' ' {
			if p.color != "" {
				t.Errorf("%s: padding space coloured %q", label, p.color)
			}
			continue
		}
		nonSpace++
		if p.color != color {
			t.Errorf("%s: %q coloured %q, want %q", label, p.r, p.color, color)
		}
	}
	if nonSpace == 0 {
		t.Errorf("%s: nothing to colour on the line", label)
	}
}

// assertPlainRoundTrip is the invariant that keeps styling honest: the
// styler only adds escapes, so stripping them yields the built lines
// byte for byte. Column widths, the gutter, and the rail's alignment
// are all downstream of that.
func assertPlainRoundTrip(t *testing.T, plain, styled []string) {
	t.Helper()
	if len(styled) != len(plain) {
		t.Fatalf("styled line count = %d, want %d", len(styled), len(plain))
	}
	for i := range plain {
		if got := plainOf(parsePainted(t, styled[i])); got != plain[i] {
			t.Errorf("line %d strips to %q, want %q", i, got, plain[i])
		}
	}
}

// TestStyleHistogramANSI pins the heat gradient: bar rows ramp
// dim→mid→bright bottom to top, the window peak's cap flares plasma,
// and the caption reads as part of the ornament.
func TestStyleHistogramANSI(t *testing.T) {
	counts := make([]int, HistDays)
	counts[0] = 24 // the window max: full height, so its top cell is █
	counts[1] = 1  // a one-cell bar down on the baseline

	plain := BuildActivityHistogram(counts)
	styled := styleHistogramANSI(plain)
	assertPlainRoundTrip(t, plain, styled)

	gutter := len([]rune(histGutter))
	at := func(row, day int) painted {
		return parsePainted(t, styled[row])[gutter+day]
	}

	// Peak column: plasma cap on the top row, then bright/mid/dim grain
	// as the bar descends through the bands.
	if p := at(0, 0); p.r != '█' || p.color != cliout.Plasma {
		t.Errorf("peak cap = %q/%q, want █/plasma", p.r, p.color)
	}
	if p := at(1, 0); p.r != barInterior || p.color != cliout.Mid {
		t.Errorf("peak middle = %q/%q, want %q/mid", p.r, p.color, barInterior)
	}
	if p := at(2, 0); p.r != barInterior || p.color != cliout.Dim {
		t.Errorf("peak base = %q/%q, want %q/dim", p.r, p.color, barInterior)
	}

	// A quiet day sits ember-dim on the baseline and doesn't steal the
	// peak's flare.
	if p := at(HistRows-1, 1); p.color != cliout.Dim {
		t.Errorf("one-cell bar coloured %q, want dim", p.color)
	}

	if styled[HistRows] != "" {
		t.Errorf("spacer = %q, want it left empty", styled[HistRows])
	}
	caption := parsePainted(t, styled[HistRows+1])
	wantColor(t, caption, 'a', cliout.Mid, "caption") // "activity · last N days"
}

// TestStyleHistogramANSIQuiet: the cold-state collapse is a single
// caption-ish line, so it takes the caption's colour.
func TestStyleHistogramANSIQuiet(t *testing.T) {
	plain := BuildActivityHistogram(make([]int, HistDays))
	styled := styleHistogramANSI(plain)
	assertPlainRoundTrip(t, plain, styled)
	wantAllNonSpace(t, parsePainted(t, styled[0]), cliout.Mid, "quiet line")
}

// TestStyleFactoryANSI pins the factory's heat mapping on a known-shape
// rail: a live station lit bright over plasma exhaust, a parked one in
// mid amber, goods mid, and the rail itself dim chrome. The two station
// glyphs differ, so bracket context is exercised alongside the
// base-row liveness read.
func TestStyleFactoryANSI(t *testing.T) {
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
	styled := styleFactoryANSI(plain)
	assertPlainRoundTrip(t, plain, styled)

	wantAllNonSpace(t, parsePainted(t, styled[0]), cliout.Mid, "drift row")
	wantAllNonSpace(t, parsePainted(t, styled[1]), cliout.Plasma, "base row")

	rail := parsePainted(t, styled[2])
	wantColor(t, rail, '⚒', cliout.Bright, "running station")
	wantColor(t, rail, '⚙', cliout.Mid, "parked station")
	wantColor(t, rail, '▦', cliout.Mid, "backlog input")
	wantColor(t, rail, '▣', cliout.Mid, "completed output")
	wantColor(t, rail, '━', cliout.Dim, "rail filler")
	wantColor(t, rail, '▶', cliout.Dim, "feed arrow")
	wantColor(t, rail, '[', cliout.Dim, "bracket")
}

// TestStyleFactoryANSIEmpty: the one-line empty state is all chrome —
// there's no heat to show.
func TestStyleFactoryANSIEmpty(t *testing.T) {
	plain := BuildFactoryArt(FactoryState{}, ArtWidth, rand.New(rand.NewSource(1)))
	if len(plain) != 1 {
		t.Fatalf("empty art line count = %d, want 1", len(plain))
	}
	styled := styleFactoryANSI(plain)
	assertPlainRoundTrip(t, plain, styled)
	wantAllNonSpace(t, parsePainted(t, styled[0]), cliout.Dim, "empty dots")
}

// TestStyleFactoryANSIOverloadedStationGlyph: ▶ is both the feed arrow
// (chrome) and the "awaiting merge" station glyph (a station). Nothing
// about the rune tells them apart — only the bracket to its left does.
// A rail carrying both readings at once pins that lookback.
func TestStyleFactoryANSIOverloadedStationGlyph(t *testing.T) {
	state := FactoryState{
		BacklogCount: 1,                                          // "▦ ▶" — input, then the feed arrow
		ActiveStages: []ActiveStation{{Stage: "awaiting merge"}}, // parked "[▶]"
	}
	plain := BuildFactoryArt(state, ArtWidth, rand.New(rand.NewSource(2)))
	styled := styleFactoryANSI(plain)
	assertPlainRoundTrip(t, plain, styled)

	rail := parsePainted(t, styled[2])
	var feeds, stations int
	for i, p := range rail {
		if p.r != '▶' {
			continue
		}
		if i > 0 && rail[i-1].r == '[' {
			stations++
			if p.color != cliout.Mid {
				t.Errorf("bracketed ▶ (parked station) coloured %q, want mid", p.color)
			}
			continue
		}
		feeds++
		if p.color != cliout.Dim {
			t.Errorf("bare ▶ (feed arrow) coloured %q, want dim", p.color)
		}
	}
	if feeds != 1 || stations != 1 {
		t.Fatalf("rail %q: %d feed arrows, %d stations; want 1 and 1", plain[2], feeds, stations)
	}
}
