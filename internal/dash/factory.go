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
// art is always one or two lines beneath the title; section headers
// below carry the precise counts.

// ArtWidth is the target column width for the factory art row, sized
// to match the title line ("Ministry of Everything" plus the
// right-aligned timestamp).
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
	"awaiting merge": "▶",
}

const otherStageGlyph = "◉"

// SmokeGlyphs is the palette for the smoke ribbon above in-progress
// stations. Picked per-glyph by RNG; deliberately sparse and
// decorative. Exported so tests can inspect the alphabet.
var SmokeGlyphs = []rune("˙˚°⋅✦✧⋆◦")

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
// line for the empty state (a row of spaced dots) or two lines for
// the populated case ([smoke, rail]). r drives the smoke ribbon's
// decorative randomness; the rail itself is deterministic from
// state.
func BuildFactoryArt(state FactoryState, width int, r *rand.Rand) []string {
	if state.BacklogCount == 0 && len(state.ActiveStages) == 0 && state.CompletedCount == 0 {
		return []string{padRight(emptyArt(width), width)}
	}
	rail, smokeCols := buildRail(state)
	smoke := buildSmoke(rail, smokeCols, state.BacklogCount > 0, r)
	return []string{padRight(smoke, width), padRight(rail, width)}
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
		s := strings.Repeat(inputGlyph, visible)
		if state.BacklogCount > InputCap {
			s += fmt.Sprintf("+%d", state.BacklogCount-InputCap)
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
			s += fmt.Sprintf("+%d", state.CompletedCount-OutputCap)
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
		line[col] = SmokeGlyphs[r.Intn(len(SmokeGlyphs))]
	}
	if hasBacklog && r.Float64() < 0.5 {
		col := 2 + r.Intn(4)
		if col < len(line) && line[col] == ' ' {
			line[col] = SmokeGlyphs[r.Intn(len(SmokeGlyphs))]
		}
	}
	return string(line)
}

func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
