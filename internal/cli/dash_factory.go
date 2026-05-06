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

// inProgressGlyphs are the station glyphs that earn smoke. Awaiting-
// merge stations don't smoke (the work's done), so they're not here.
var inProgressGlyphs = map[rune]struct{}{
	'⚒': {},
	'⚙': {},
}

// factoryState is the data the art reads. Built once in runDash from
// the same scan + activity map that feeds the dashboard rows. Pure
// over its inputs — no disk I/O at art-render time.
type factoryState struct {
	BacklogCount   int
	ActiveStages   []string // newest-first, ≤ stationCap+1 worth (overflow handled by renderer)
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
	rail := buildRail(state)
	smoke := buildSmoke(rail, state.BacklogCount > 0, r)
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
// waiting" without inventing a hollow rail.
func buildRail(state factoryState) string {
	var parts []string

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
		parts = append(parts, s)
	}

	visibleStations := len(state.ActiveStages)
	if visibleStations > stationCap {
		visibleStations = stationCap
	}
	for i := 0; i < visibleStations; i++ {
		parts = append(parts, "["+glyphForStage(state.ActiveStages[i])+"]")
	}
	if len(state.ActiveStages) > stationCap {
		parts = append(parts, fmt.Sprintf("+%d", len(state.ActiveStages)-stationCap))
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
		parts = append(parts, s)
	}

	sep := " " + strings.Repeat(railFiller, 3) + " "
	return "  " + strings.Join(parts, sep)
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

// buildSmoke renders the smoke-ribbon line above the rail. For each
// rune position in the rail that holds an in-progress station glyph,
// place a smoke fleck at col±0..2 with p≈0.5. A single wildcard fleck
// drifts over the input zone when backlog is non-empty — suggests
// "something just dropped on the inbound conveyor."
func buildSmoke(rail string, hasBacklog bool, r *rand.Rand) string {
	runes := []rune(rail)
	line := make([]rune, len(runes))
	for i := range line {
		line[i] = ' '
	}
	for i, ru := range runes {
		if _, ok := inProgressGlyphs[ru]; !ok {
			continue
		}
		if r.Float64() >= 0.5 {
			continue
		}
		col := i + r.Intn(5) - 2
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
