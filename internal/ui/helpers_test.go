package ui

import "github.com/modulecollective/moe/internal/cliout"

func cliCSILen(p []byte) int { return cliout.CSILen(p) }

// fixedOverview is an Overview that ignores the width: lines and art as
// given, whatever the terminal.
func fixedOverview(lines []string, art Art) func(int) ([]string, Art) {
	return func(int) ([]string, Art) { return lines, art }
}
