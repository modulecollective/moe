package dash

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildActivityHistogramPopulated pins the shape of a non-empty
// chart: HistRows+2 lines — bar rows, a blank spacer, then a caption.
// Every line but the spacer is the same column width (gutter + bar
// field); the caption names the window and the window's peak.
func TestBuildActivityHistogramPopulated(t *testing.T) {
	counts := make([]int, HistDays)
	for i := range counts {
		counts[i] = i % 5 // some variation, peak 4
	}
	counts[HistDays-1] = 12 // a clear single peak

	lines := BuildActivityHistogram(counts, false)
	if len(lines) != HistRows+2 {
		t.Fatalf("line count = %d, want %d", len(lines), HistRows+2)
	}

	if lines[HistRows] != "" {
		t.Errorf("spacer line = %q, want empty", lines[HistRows])
	}

	caption := lines[HistRows+1]
	if !strings.Contains(caption, "activity · last 60 days") {
		t.Errorf("caption missing label: %q", caption)
	}
	if !strings.Contains(caption, "peak 12 runs/day") {
		t.Errorf("caption missing peak: %q", caption)
	}

	wantWidth := len(histGutter) + HistDays // 2 + 60
	for i, l := range lines {
		if i == HistRows {
			continue // the blank spacer is intentionally not field-width
		}
		if w := utf8.RuneCountInString(l); w != wantWidth {
			t.Errorf("line %d width = %d, want %d: %q", i, w, wantWidth, l)
		}
	}

	// The peak day is the last column; on the top row it must be a full
	// block, since it's the window max scaled to HistRows*8 eighths.
	top := []rune(lines[0])
	if got := top[len(top)-1]; got != '█' {
		t.Errorf("peak day top cell = %q, want █", got)
	}
}

// TestBuildActivityHistogramQuiet: an all-zero window (fresh bureaucracy
// or no activity in HistDays) collapses to a single "(quiet)" line
// rather than a HistRows-high void.
func TestBuildActivityHistogramQuiet(t *testing.T) {
	lines := BuildActivityHistogram(make([]int, HistDays), false)
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1 (quiet collapse)", len(lines))
	}
	if !strings.Contains(lines[0], "(quiet)") {
		t.Errorf("quiet line = %q, want it to mention (quiet)", lines[0])
	}
}

// TestBuildActivityHistogramSingleSpike: one non-zero day scales to a
// full-height bar (it's the window max), every other day stays blank,
// and the spike's own column is a crisp █ cap over ▓ interior — the
// texture rule, which only bites on bars tall enough to span rows.
func TestBuildActivityHistogramSingleSpike(t *testing.T) {
	counts := make([]int, HistDays)
	spike := 7
	counts[spike] = 3

	lines := BuildActivityHistogram(counts, false)
	if len(lines) != HistRows+2 {
		t.Fatalf("line count = %d, want %d", len(lines), HistRows+2)
	}

	for r := range HistRows {
		cells := []rune(lines[r])
		// Drop the gutter to index by day.
		cells = cells[utf8.RuneCountInString(histGutter):]
		for day, c := range cells {
			if day == spike {
				want := barInterior
				if r == 0 {
					want = '█' // topmost non-blank cell: the cap stays crisp
				}
				if c != want {
					t.Errorf("row %d spike cell = %q, want %q", r, c, want)
				}
			} else if c != ' ' {
				t.Errorf("row %d day %d = %q, want blank", r, day, c)
			}
		}
	}
}

// TestBuildActivityHistogramTexture pins the cap-vs-interior rule
// directly: a one-row bar is a lone cap with no grain beneath it, and a
// bar that spans rows caps with a block (or a partial eighth) and fills
// the rows below with ▓.
func TestBuildActivityHistogramTexture(t *testing.T) {
	counts := make([]int, HistDays)
	counts[0] = 24 // the window max: a full HistRows-high column
	counts[1] = 1  // 1/24 of the peak → a single eighth on the bottom row
	counts[2] = 16 // two rows high: a cap on the middle row, grain below

	lines := BuildActivityHistogram(counts, false)
	cell := func(row, day int) rune {
		cells := []rune(lines[row])[utf8.RuneCountInString(histGutter):]
		return cells[day]
	}

	// Full-height column: crisp cap on top, grain all the way down.
	if got := cell(0, 0); got != '█' {
		t.Errorf("full column cap = %q, want █", got)
	}
	for r := 1; r < HistRows; r++ {
		if got := cell(r, 0); got != barInterior {
			t.Errorf("full column row %d = %q, want %q", r, got, barInterior)
		}
	}

	// One-cell bar: a partial eighth on the baseline, blank above. A cap
	// with nothing under it never grains.
	if got := cell(HistRows-1, 1); got == ' ' || got == barInterior {
		t.Errorf("one-cell bar baseline = %q, want a partial eighth", got)
	}
	for r := range HistRows - 1 {
		if got := cell(r, 1); got != ' ' {
			t.Errorf("one-cell bar row %d = %q, want blank", r, got)
		}
	}

	// Two-row bar: blank top row, a cap in the middle, grain below.
	if got := cell(0, 2); got != ' ' {
		t.Errorf("two-row bar top row = %q, want blank", got)
	}
	if got := cell(1, 2); got != '█' {
		t.Errorf("two-row bar cap = %q, want █", got)
	}
	if got := cell(2, 2); got != barInterior {
		t.Errorf("two-row bar interior = %q, want %q", got, barInterior)
	}
}

// histFixtures are the count windows the live-star tests run against:
// today tall, today short, today the peak, and today zero.
func histFixtures() map[string][]int {
	mk := func(today int) []int {
		c := make([]int, HistDays)
		for i := range c {
			c[i] = (i * 7) % 9
		}
		c[len(c)-1] = today
		return c
	}
	return map[string][]int{
		"tall":  mk(6),
		"short": mk(1),
		"peak":  mk(20),
		"zero":  mk(0),
	}
}

// TestBuildActivityHistogramLiveStar: live differs from idle in exactly
// one cell — the topmost non-blank cell of today's column, or the bottom
// row when today is zero — and that cell is the rest-pose star. Cells
// below it keep their bar.
func TestBuildActivityHistogramLiveStar(t *testing.T) {
	for name, counts := range histFixtures() {
		idle := BuildActivityHistogram(counts, false)
		live := BuildActivityHistogram(counts, true)
		if len(idle) != len(live) {
			t.Fatalf("%s: %d lines live, %d idle", name, len(live), len(idle))
		}
		wantRow := HistRows - 1
		for r := range HistRows {
			if c, _ := utf8.DecodeLastRuneInString(idle[r]); c != ' ' {
				wantRow = r
				break
			}
		}
		diffs := 0
		for r := range idle {
			a, b := []rune(idle[r]), []rune(live[r])
			if len(a) != len(b) {
				t.Fatalf("%s line %d: width %d live, %d idle", name, r, len(b), len(a))
			}
			for c := range a {
				if a[c] == b[c] {
					continue
				}
				diffs++
				if r != wantRow || c != len(a)-1 || b[c] != starRest {
					t.Errorf("%s: line %d col %d = %q, want only %q at row %d last col", name, r, c, b[c], starRest, wantRow)
				}
			}
		}
		if diffs != 1 {
			t.Errorf("%s: %d cells differ, want 1", name, diffs)
		}
	}
}

// TestBuildActivityHistogramPeakStar: a live peak day gives up its
// flare cap to the star.
func TestBuildActivityHistogramPeakStar(t *testing.T) {
	lines := BuildActivityHistogram(histFixtures()["peak"], true)
	if r, _ := utf8.DecodeLastRuneInString(lines[0]); r != starRest {
		t.Errorf("peak top cell = %q, want %q", r, starRest)
	}
}

// TestBuildActivityHistogramQuietIgnoresLive: the cold state has one
// shape, star or no.
func TestBuildActivityHistogramQuietIgnoresLive(t *testing.T) {
	idle := BuildActivityHistogram(make([]int, HistDays), false)
	live := BuildActivityHistogram(make([]int, HistDays), true)
	if !slices.Equal(idle, live) {
		t.Errorf("quiet live = %q, idle = %q", live, idle)
	}
}

// TestBuildActivityHistogramWindow: the caption names the window the
// counts cover, and a window too narrow for the full caption shortens
// it rather than outgrowing the bars.
func TestBuildActivityHistogramWindow(t *testing.T) {
	for _, days := range []int{36, 116, 365} {
		counts := make([]int, days)
		counts[0] = 3
		lines := BuildActivityHistogram(counts, true)
		caption := lines[len(lines)-1]
		if !strings.Contains(caption, fmt.Sprintf("last %d days", days)) {
			t.Errorf("%d days: caption %q", days, caption)
		}
		for i, l := range lines {
			if w := utf8.RuneCountInString(l); w > len(histGutter)+days {
				t.Errorf("%d days: line %d is %d wide: %q", days, i, w, l)
			}
		}
	}
	quiet := BuildActivityHistogram(make([]int, 116), false)
	if !strings.Contains(quiet[0], "last 116 days") {
		t.Errorf("quiet caption %q", quiet[0])
	}
}
