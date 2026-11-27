package dash

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Activity histogram: a peripheral read of the bureaucracy's tempo,
// sitting between the dash banner and the board sections — one thin bar
// per day, scaled to the window's peak. Today's bar (the last column)
// doubles as the liveness signal: while any run has an open session it
// is capped with a star, and that star is the only thing on the board
// that moves.

const (
	// HistDays is the window the web dash charts: the trailing
	// N days of activity. Fixed (no config knob) — single-operator project.
	HistDays = 60
	// HistMaxDays is how many daily counts a snapshot carries: moe ui
	// charts one column per day across the terminal, up to a year.
	HistMaxDays = 365
	// HistRows is the number of stacked bar rows. Each row resolves eight
	// eighths, so the chart's vertical resolution is HistRows*8.
	HistRows = 3
)

// histGutter is the 2-space left margin the board sections use; the
// histogram shares it so the caption and bars start in the same column
// as the rows below.
const histGutter = "  "

// barRunes indexes 0..8 eighths of a vertical block — index 0 blank,
// index 8 a full cell.
var barRunes = []rune(" ▁▂▃▄▅▆▇█")

// barInterior replaces the full block on a cell that isn't the bar's
// topmost non-blank cell. Caps stay crisp; bar bodies get half-tone
// grain, so a tall day reads as a glowing column rather than a solid
// slab. Deterministic and purely data-driven.
const barInterior = '▓'

// starRest is the live star's rest pose: the cell BuildActivityHistogram
// writes and every still surface shows. StyledHistogramFrames cycles it
// through starTwinkle.
const starRest = '*'

// BuildActivityHistogram renders the daily run-activity chart. counts is
// the window, oldest→newest, one value per day; each is the number of
// distinct runs active that day (see run.JournalIndex.DailyRunCount).
// The caption names len(counts) as the window. The result is HistRows
// bar rows, a blank spacer line, then a caption line — HistRows+2 lines.
// The bar rows and caption are histGutter-indented and len(counts)
// columns wide; the spacer is a truly empty line, neither indented nor
// field-width.
//
// live caps today's bar with starRest: the star replaces the topmost
// non-blank cell of the last column, or sits on the bottom row when
// today's count is zero. It wins over the peak's flare cap.
//
// Cold state — every count zero (a fresh bureaucracy, or no activity in
// the window) — collapses to a single "(quiet)" line rather than drawing
// an empty HistRows-high void, live or not.
func BuildActivityHistogram(counts []int, live bool) []string {
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	if max == 0 {
		return []string{fmt.Sprintf("%sactivity · last %d days  ·  (quiet)", histGutter, len(counts))}
	}

	lines := make([]string, 0, HistRows+2)

	full := HistRows * 8
	for row := range HistRows {
		// Rows emit top→bottom. base is the eighths the rows below this
		// one already account for, so this row only draws the slice of the
		// bar that rises into its band.
		base := (HistRows - 1 - row) * 8
		var b strings.Builder
		b.WriteString(histGutter)
		for _, c := range counts {
			eighths := int(math.Round(float64(c) * float64(full) / float64(max)))
			// rise is this cell's slice of the bar in eighths, before
			// clamping. A full cell is the bar's cap exactly when the bar
			// doesn't continue into the row above — i.e. rise == 8.
			rise := eighths - base
			if rise > 8 {
				b.WriteRune(barInterior)
				continue
			}
			b.WriteRune(barRunes[clampEighth(rise)])
		}
		lines = append(lines, b.String())
	}
	if live {
		placeStar(lines)
	}
	lines = append(lines, "")
	lines = append(lines, histCaption(len(counts), max))
	return lines
}

// placeStar writes starRest over the topmost non-blank cell of the last
// column of the bar rows, falling back to the bottom row.
func placeStar(bars []string) {
	row := len(bars) - 1
	for i, l := range bars {
		if r, _ := utf8.DecodeLastRuneInString(l); r != ' ' {
			row = i
			break
		}
	}
	rs := []rune(bars[row])
	rs[len(rs)-1] = starRest
	bars[row] = string(rs)
}

// histCaption renders the caption row: a left-justified label and a
// right-justified "peak N runs/day", padded to field columns (the bar
// width) inside the gutter so the two ends pin to the chart's edges. A
// window too narrow for both (moe ui on a small terminal) shortens the
// right end to "peak N" so the caption never outgrows the bars.
func histCaption(field, max int) string {
	left := fmt.Sprintf("activity · last %d days", field)
	right := fmt.Sprintf("peak %d runs/day", max)
	if utf8.RuneCountInString(left)+1+utf8.RuneCountInString(right) > field {
		right = fmt.Sprintf("peak %d", max)
	}
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
