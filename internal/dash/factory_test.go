package dash

import (
	"math/rand"
	"testing"
)

// TestBuildFactoryFrames pins the contract the client cross-fade relies
// on: exactly n frames, every frame the same line count (so stacked
// layers don't jump), and smoke rows drawn only from the palette. It
// deliberately does NOT assert the rail differs or holds across frames —
// the loop makes no "only smoke moves" assumption.
func TestBuildFactoryFrames(t *testing.T) {
	populated := FactoryState{
		BacklogCount: 2,
		ActiveStages: []ActiveStation{
			{Stage: "design", RunningDoc: "design"},
			{Stage: "code", RunningDoc: "code"},
		},
		CompletedCount: 3,
	}

	palette := map[rune]struct{}{' ': {}}
	for _, g := range SmokeGlyphs {
		palette[g] = struct{}{}
	}

	cases := []struct {
		name      string
		state     FactoryState
		n         int
		wantLines int // lines per frame
	}{
		{"empty", FactoryState{}, factoryFramesTestN, 1},
		{"populated", populated, factoryFramesTestN, 3},
		{"single-frame", populated, 1, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := BuildFactoryFrames(tc.state, ArtWidth, tc.n, rand.New(rand.NewSource(1)))
			if len(frames) != tc.n {
				t.Fatalf("frame count = %d, want %d", len(frames), tc.n)
			}
			for i, f := range frames {
				if len(f) != tc.wantLines {
					t.Fatalf("frame %d line count = %d, want %d", i, len(f), tc.wantLines)
				}
			}
			// Smoke rows are every line above the rail (the last line). For
			// the 1-line empty state there is no smoke row to check.
			if tc.wantLines < 2 {
				return
			}
			for fi, f := range frames {
				for _, row := range f[:tc.wantLines-1] {
					for _, ru := range row {
						if _, ok := palette[ru]; !ok {
							t.Fatalf("frame %d smoke row has non-palette rune %q in %q", fi, ru, row)
						}
					}
				}
			}
		})
	}
}

const factoryFramesTestN = 10
