package dash

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Activity histogram: a peripheral read of the bureaucracy's tempo,
// sitting between the dash banner and the factory art. Where the
// factory art shows the *current* shape (what's queued, running, done),
// the histogram shows *how busy* the last HistDays have been — one thin
// bar per day, scaled to the window's peak.

const (
	// HistDays is the histogram window: the trailing N days of activity
	// the chart covers. Fixed (no config knob) — single-operator project.
	HistDays = 60
	// HistRows is the number of stacked bar rows. Each row resolves eight
	// eighths, so the chart's vertical resolution is HistRows*8.
	HistRows = 3
)

// histGutter is the 2-space left margin the factory rail uses; the
// histogram shares it so the caption, bars, and rail all start in the
// same column.
const histGutter = "  "

// barRunes indexes 0..8 eighths of a vertical block — index 0 blank,
// index 8 a full cell.
var barRunes = []rune(" ▁▂▃▄▅▆▇█")

// BuildActivityHistogram renders the daily run-activity chart. counts is
// HistDays values, oldest→newest; each is the number of distinct runs
// active that day (see run.JournalIndex.DailyRunCount). The result is
// HistRows bar rows, a blank spacer line, then a caption line —
// HistRows+2 lines. The bar rows and caption are histGutter-indented and
// len(counts) columns wide so the block stacks flush with the factory
// rail below it; the spacer is a truly empty line, neither indented nor
// field-width.
//
// Cold state — every count zero (a fresh bureaucracy, or no activity in
// the window) — collapses to a single "(quiet)" line, mirroring how the
// factory art collapses to one dotted line when empty rather than
// drawing an empty HistRows-high void.
func BuildActivityHistogram(counts []int) []string {
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	if max == 0 {
		return []string{fmt.Sprintf("%sactivity · last %d days  ·  (quiet)", histGutter, HistDays)}
	}

	lines := make([]string, 0, HistRows+2)

	full := HistRows * 8
	for row := 0; row < HistRows; row++ {
		// Rows emit top→bottom. base is the eighths the rows below this
		// one already account for, so this row only draws the slice of the
		// bar that rises into its band.
		base := (HistRows - 1 - row) * 8
		var b strings.Builder
		b.WriteString(histGutter)
		for _, c := range counts {
			eighths := int(math.Round(float64(c) * float64(full) / float64(max)))
			b.WriteRune(barRunes[clampEighth(eighths-base)])
		}
		lines = append(lines, b.String())
	}
	lines = append(lines, "")
	lines = append(lines, histCaption(len(counts), max))
	return lines
}

// histCaption renders the caption row: a left-justified label and a
// right-justified "peak N runs/day", padded to field columns (the bar
// width) inside the gutter so the two ends pin to the chart's edges.
func histCaption(field, max int) string {
	left := fmt.Sprintf("activity · last %d days", HistDays)
	right := fmt.Sprintf("peak %d runs/day", max)
	gap := field - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	if gap < 1 {
		gap = 1
	}
	return histGutter + left + strings.Repeat(" ", gap) + right
}

func clampEighth(n int) int {
	if n < 0 {
		return 0
	}
	if n > 8 {
		return 8
	}
	return n
}
