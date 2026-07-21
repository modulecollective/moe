package dash

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildActivityHistogramPopulated pins the shape of a non-empty
// chart: HistRows+2 lines — bar rows, a blank spacer, then a caption.
// Every line but the spacer is the same column width (gutter + bar
// field) so the block stacks flush with the factory rail; the caption
// names the window and the window's peak.
func TestBuildActivityHistogramPopulated(t *testing.T) {
	counts := make([]int, HistDays)
	for i := range counts {
		counts[i] = i % 5 // some variation, peak 4
	}
	counts[HistDays-1] = 12 // a clear single peak

	lines := BuildActivityHistogram(counts)
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
	lines := BuildActivityHistogram(make([]int, HistDays))
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

	lines := BuildActivityHistogram(counts)
	if len(lines) != HistRows+2 {
		t.Fatalf("line count = %d, want %d", len(lines), HistRows+2)
	}

	for r := 0; r < HistRows; r++ {
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

	lines := BuildActivityHistogram(counts)
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
