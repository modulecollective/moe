package dash

import (
	"fmt"
	"math/rand"
	"strings"
	"unicode/utf8"
)

// Factory art: a peripheral-vision read of the bureaucracy's current
// shape. Backlog ideas are raw materials on the inbound conveyor;
// active runs are stations along the rail (glyph picked from stage);
// recent completions are finished goods on the outbound side. The
// art collapses to a single dotted line on an empty bureaucracy and
// expands to a three-line plume-over-rail when populated; section
// headers below carry the precise counts.

// ArtWidth is the target column width for the factory art row. The
// 61-column figure is historical (it matched the retired sentence-case
// title that used to sit above the art) and is kept as a stable budget
// for the rail so the section headers below line up consistently.
const ArtWidth = 61

// Visual budget per zone — fixed so the art doesn't blow past
// ArtWidth or compete with the title.
const (
	InputCap   = 4
	StationCap = 4
	OutputCap  = 5
)

// Glyphs used in the rail. Single-rune per cell except brackets
// around stations — the bracket pair is the visual "this is a
// station" anchor even when the centre glyph renders two-cell on
// legacy CJK terminals.
const (
	inputGlyph  = "▦"
	outputGlyph = "▣"
	feedArrow   = "▶"
	railFiller  = "━"
	emptyDot    = "·"
)

// stageGlyphs maps a stage name (or "awaiting merge" for pushed
// runs) to the glyph drawn inside the bracketed station. Anything
// not listed falls back to otherStageGlyph — the generic boiler.
var stageGlyphs = map[string]string{
	"design":         "⚒",
	"code":           "⚙",
	"test":           "✓",
	"awaiting merge": "▶",
	// Twin reflect ladder. The first six stages walk the six managed
	// docs against the events list; finalize seals the pass. Glyphs
	// pick legible silhouettes that read as "twin work" without
	// duplicating the sdlc set — design's ⚒ and code's ⚙ already
	// claim the obvious factory glyphs.
	"vision":       "◇",
	"architecture": "▦",
	"patterns":     "❖",
	"operations":   "◆",
	"roadmap":      "➜",
	"glossary":     "𝒢",
	"finalize":     "◎",
}

const otherStageGlyph = "◉"

// basePuff sits on the row immediately above the rail, anchored to a
// running station's chimney column. Dense, rounded glyphs — these are
// the "this station is alive" signal, drawn with p=1.0 per station.
var basePuff = []rune("°◦∘⊙")

// driftWisp sits one row higher than basePuff and drifts ±1 column
// off the chimney. Sparser, decorative — fires probabilistically so
// two consecutive renders aren't identical, but absence never hides
// liveness (that's basePuff's job).
var driftWisp = []rune("✦✧⋆◦∘")

// SmokeGlyphs is the union of basePuff and driftWisp, exported so
// tests can assert palette membership without caring which row a rune
// landed on. Deduplicated.
var SmokeGlyphs = []rune("°◦∘⊙✦✧⋆")

// ActiveStation is one station's worth of factory-art state. Stage
// is the parked next-stage name (drives the glyph when the run
// isn't live); RunningDoc names the doc with an open session that
// "wins" the liveness slot — empty when the run has no open session.
type ActiveStation struct {
	Stage      string
	RunningDoc string
}

// FactoryState is the data the art reads. Built once in cli's
// runDash from the same scan + activity map that feeds the dashboard
// rows. Pure over its inputs — no disk I/O at art-render time.
type FactoryState struct {
	BacklogCount   int
	ActiveStages   []ActiveStation // newest-first, ≤ StationCap+1 worth (overflow handled by renderer)
	CompletedCount int
}

// BuildFactoryArt renders the art beneath the title. Returns one
// line for the empty state (a row of spaced dots) or three lines for
// the populated case ([driftWisp row, basePuff row, rail]). r drives
// the smoke's decorative randomness; the rail itself and the basePuff
// row are deterministic-in-shape from state (basePuff fires per
// running station with p=1.0; only the glyph picked is randomised).
func BuildFactoryArt(state FactoryState, width int, r *rand.Rand) []string {
	if state.BacklogCount == 0 && len(state.ActiveStages) == 0 && state.CompletedCount == 0 {
		return []string{padRight(emptyArt(width), width)}
	}
	rail, smokeCols := buildRail(state)
	railWidth := utf8.RuneCountInString(rail)
	top, base := buildPlumes(smokeCols, railWidth, state.BacklogCount > 0, r)
	return []string{padRight(top, width), padRight(base, width), padRight(rail, width)}
}

func emptyArt(width int) string {
	pairs := width / 2
	if pairs < 1 {
		pairs = 1
	}
	return "  " + strings.TrimRight(strings.Repeat(emptyDot+" ", pairs), " ")
}

func buildRail(state FactoryState) (string, []int) {
	type segment struct {
		text       string
		stationIdx int // -1 if not a station; otherwise index into state.ActiveStages
	}
	var segs []segment

	if state.BacklogCount > 0 {
		visible := state.BacklogCount
		if visible > InputCap {
			visible = InputCap
		}
		glyphs := make([]string, visible)
		for i := range glyphs {
			glyphs[i] = inputGlyph
		}
		s := strings.Join(glyphs, " ")
		if state.BacklogCount > InputCap {
			s += fmt.Sprintf(" +%d", state.BacklogCount-InputCap)
		}
		s += " " + feedArrow
		segs = append(segs, segment{text: s, stationIdx: -1})
	}

	visibleStations := len(state.ActiveStages)
	if visibleStations > StationCap {
		visibleStations = StationCap
	}
	for i := 0; i < visibleStations; i++ {
		segs = append(segs, segment{
			text:       "[" + glyphForStation(state.ActiveStages[i]) + "]",
			stationIdx: i,
		})
	}
	if len(state.ActiveStages) > StationCap {
		segs = append(segs, segment{
			text:       fmt.Sprintf("+%d", len(state.ActiveStages)-StationCap),
			stationIdx: -1,
		})
	}

	if state.CompletedCount > 0 {
		visible := state.CompletedCount
		if visible > OutputCap {
			visible = OutputCap
		}
		glyphs := make([]string, visible)
		for i := range glyphs {
			glyphs[i] = outputGlyph
		}
		s := strings.Join(glyphs, " ")
		if state.CompletedCount > OutputCap {
			s += fmt.Sprintf(" +%d", state.CompletedCount-OutputCap)
		}
		segs = append(segs, segment{text: s, stationIdx: -1})
	}

	sep := " " + strings.Repeat(railFiller, 3) + " "
	sepRunes := utf8.RuneCountInString(sep)

	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = s.text
	}
	rail := "  " + strings.Join(parts, sep)

	var smokeCols []int
	col := 2 // leading "  "
	for i, s := range segs {
		if i > 0 {
			col += sepRunes
		}
		if s.stationIdx >= 0 && state.ActiveStages[s.stationIdx].RunningDoc != "" {
			smokeCols = append(smokeCols, col+1)
		}
		col += utf8.RuneCountInString(s.text)
	}
	return rail, smokeCols
}

func glyphForStation(st ActiveStation) string {
	if st.RunningDoc != "" {
		return glyphForStage(st.RunningDoc)
	}
	return glyphForStage(st.Stage)
}

func glyphForStage(stage string) string {
	if g, ok := stageGlyphs[stage]; ok {
		return g
	}
	return otherStageGlyph
}

// buildPlumes paints the two smoke rows above the rail. The base row
// is the load-bearing liveness signal: every running station gets a
// basePuff glyph at its chimney column, no probability gate. The top
// (drift) row is decorative — wisps fire per-station with p≈0.6 and
// jitter ±1 column off the chimney, plus an optional backlog fleck
// when raw material is queued. Collisions are skipped, not retried.
func buildPlumes(smokeCols []int, width int, hasBacklog bool, r *rand.Rand) (top, base string) {
	topRow := make([]rune, width)
	baseRow := make([]rune, width)
	for i := range topRow {
		topRow[i] = ' '
		baseRow[i] = ' '
	}
	for _, c := range smokeCols {
		if c >= 0 && c < width && baseRow[c] == ' ' {
			baseRow[c] = basePuff[r.Intn(len(basePuff))]
		}
		if r.Float64() < 0.6 {
			col := c + r.Intn(3) - 1
			if col >= 0 && col < width && topRow[col] == ' ' {
				topRow[col] = driftWisp[r.Intn(len(driftWisp))]
			}
		}
	}
	if hasBacklog && r.Float64() < 0.5 {
		col := 2 + r.Intn(4)
		if col < width && topRow[col] == ' ' {
			topRow[col] = driftWisp[r.Intn(len(driftWisp))]
		}
	}
	return string(topRow), string(baseRow)
}

func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
