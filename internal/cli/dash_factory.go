package cli

import (
	"fmt"
	"math/rand"
	"strings"
	"unicode/utf8"
)

// Factory art: a peripheral-vision read of the bureaucracy's current
// shape. Backlog ideas are raw materials on the inbound conveyor;
// active runs are stations along the rail (glyph picked from stage);
// recent completions are finished goods on the outbound side. The art
// is always one or two lines beneath the title; section headers below
// carry the precise counts.

// artWidth is the target column width for the factory art row, sized
// to match the title line ("Ministry of Everything" plus the
// right-aligned timestamp).
const artWidth = 61

// Visual budget per zone — fixed so the art doesn't blow past artWidth
// or compete with the title. Tune after seeing real state.
const (
	inputCap   = 4
	stationCap = 4
	outputCap  = 5
)

// Glyphs used in the rail. Single-rune per cell except brackets around
// stations — the bracket pair is the visual "this is a station" anchor
// even when the centre glyph renders two-cell on legacy CJK terminals.
const (
	inputGlyph  = "▦"
	outputGlyph = "▣"
	feedArrow   = "▶"
	railFiller  = "━"
	emptyDot    = "·"
)

// stageGlyphs maps a stage name (or "awaiting merge" for pushed runs)
// to the glyph drawn inside the bracketed station. Anything not listed
// falls back to otherStageGlyph — the generic boiler.
var stageGlyphs = map[string]string{
	"design":         "⚒",
	"code":           "⚙",
	"awaiting merge": "▶",
}

const otherStageGlyph = "◉"

// smokeGlyphs is the palette for the smoke ribbon above in-progress
// stations. Picked per-glyph by RNG; deliberately sparse and decorative.
var smokeGlyphs = []rune("˙˚°⋅✦✧⋆◦")

// activeStation is one station's worth of factory-art state. Stage is
// the parked next-stage name (drives the glyph when the run isn't
// live); RunningDoc names the doc with an open session that "wins" the
// liveness slot — empty when the run has no open session. When set, it
// takes over the glyph (the art shows what's live) and earns the
// station a smoke fleck.
type activeStation struct {
	Stage      string
	RunningDoc string
}

// factoryState is the data the art reads. Built once in runDash from
// the same scan + activity map that feeds the dashboard rows. Pure
// over its inputs — no disk I/O at art-render time.
type factoryState struct {
	BacklogCount   int
	ActiveStages   []activeStation // newest-first, ≤ stationCap+1 worth (overflow handled by renderer)
	CompletedCount int
}

// buildFactoryArt renders the art beneath the title. Returns one line
// for the empty state (a row of spaced dots) or two lines for the
// populated case ([smoke, rail]). r drives the smoke ribbon's
// decorative randomness; the rail itself is deterministic from state.
func buildFactoryArt(state factoryState, width int, r *rand.Rand) []string {
	if state.BacklogCount == 0 && len(state.ActiveStages) == 0 && state.CompletedCount == 0 {
		return []string{padRight(emptyArt(width), width)}
	}
	rail, smokeCols := buildRail(state)
	smoke := buildSmoke(rail, smokeCols, state.BacklogCount > 0, r)
	return []string{padRight(smoke, width), padRight(rail, width)}
}

// emptyArt is the "factory is quiet" line — a sparse field of dots
// rather than a rail with no stations. Acknowledges presence without
// drawing a hollow workshop.
func emptyArt(width int) string {
	pairs := width / 2
	if pairs < 1 {
		pairs = 1
	}
	return "  " + strings.TrimRight(strings.Repeat(emptyDot+" ", pairs), " ")
}

// buildRail lays out the rail line: input zone, then one bracketed
// station per active run (capped + overflow tag), then output zone.
// Empty zones drop out so an only-backlog state reads as "raw materials
// waiting" without inventing a hollow rail. Returns the rune-column of
// each running station's inner glyph alongside the rail string — the
// smoke ribbon scatters flecks above those columns and ignores parked
// stations entirely.
func buildRail(state factoryState) (string, []int) {
	type segment struct {
		text       string
		stationIdx int // -1 if not a station; otherwise index into state.ActiveStages
	}
	var segs []segment

	if state.BacklogCount > 0 {
		visible := state.BacklogCount
		if visible > inputCap {
			visible = inputCap
		}
		s := strings.Repeat(inputGlyph, visible)
		if state.BacklogCount > inputCap {
			s += fmt.Sprintf("+%d", state.BacklogCount-inputCap)
		}
		// Feed arrow follows the input glyphs to read as "raw
		// material entering the rail." Only present when backlog is.
		s += " " + feedArrow
		segs = append(segs, segment{text: s, stationIdx: -1})
	}

	visibleStations := len(state.ActiveStages)
	if visibleStations > stationCap {
		visibleStations = stationCap
	}
	for i := 0; i < visibleStations; i++ {
		segs = append(segs, segment{
			text:       "[" + glyphForStation(state.ActiveStages[i]) + "]",
			stationIdx: i,
		})
	}
	if len(state.ActiveStages) > stationCap {
		segs = append(segs, segment{
			text:       fmt.Sprintf("+%d", len(state.ActiveStages)-stationCap),
			stationIdx: -1,
		})
	}

	if state.CompletedCount > 0 {
		visible := state.CompletedCount
		if visible > outputCap {
			visible = outputCap
		}
		glyphs := make([]string, visible)
		for i := range glyphs {
			glyphs[i] = outputGlyph
		}
		s := strings.Join(glyphs, " ")
		if state.CompletedCount > outputCap {
			s += fmt.Sprintf("+%d", state.CompletedCount-outputCap)
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

	// Walk segments to find the inner-glyph column of each running
	// station. Stations are "[X]" with a single-rune inner glyph, so
	// the smoke target sits one rune right of the part's start.
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

// glyphForStation picks the bracketed-station inner glyph. A running
// session takes over: the art shows what's live, not what the parking
// rule says is next. Falls back to the parked stage when no session is
// open.
func glyphForStation(st activeStation) string {
	if st.RunningDoc != "" {
		return glyphForStage(st.RunningDoc)
	}
	return glyphForStage(st.Stage)
}

// glyphForStage returns the bracketed-station inner glyph for a stage
// name. Unrecognised stages fall back to the generic boiler so a new
// workflow doesn't silently render as nothing.
func glyphForStage(stage string) string {
	if g, ok := stageGlyphs[stage]; ok {
		return g
	}
	return otherStageGlyph
}

// buildSmoke renders the smoke-ribbon line above the rail. Each entry
// in smokeCols is a rune-column hosting a station with an open session;
// for each, place a smoke fleck at col±0..2 with p≈0.5. Stations with
// no open session stay quiet — smoke is the liveness signal, not a
// stage decoration. A single wildcard fleck drifts over the input zone
// when backlog is non-empty — suggests "something just dropped on the
// inbound conveyor."
func buildSmoke(rail string, smokeCols []int, hasBacklog bool, r *rand.Rand) string {
	runes := []rune(rail)
	line := make([]rune, len(runes))
	for i := range line {
		line[i] = ' '
	}
	for _, c := range smokeCols {
		if r.Float64() >= 0.5 {
			continue
		}
		col := c + r.Intn(5) - 2
		if col < 0 || col >= len(line) {
			continue
		}
		if line[col] != ' ' {
			continue
		}
		line[col] = smokeGlyphs[r.Intn(len(smokeGlyphs))]
	}
	if hasBacklog && r.Float64() < 0.5 {
		col := 2 + r.Intn(4)
		if col < len(line) && line[col] == ' ' {
			line[col] = smokeGlyphs[r.Intn(len(smokeGlyphs))]
		}
	}
	return string(line)
}

// padRight extends s with spaces until its rune-count reaches width.
// Lines shorter than width keep tabwriter alignment for downstream
// sections out of consideration — the art row stands alone.
func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
