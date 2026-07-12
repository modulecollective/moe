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
// and the lower rows under the spike are full while the rest are empty.
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
				if c != '█' {
					t.Errorf("row %d spike cell = %q, want █ (max scales to full height)", r, c)
				}
			} else if c != ' ' {
				t.Errorf("row %d day %d = %q, want blank", r, day, c)
			}
		}
	}
}
