package cli

import "io"

// openPulseStage routes the cascade driver (`!` / `!<stage>` / `!!` /
// `!!!`) to the pulse workflow's single stage. Registered for surface
// uniformity with the other workflows' stage dispatchers.
func openPulseStage(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
	switch stage {
	case pulseDoc:
		// No skip latch on this seam: riding a chain into a pulse stage
		// is not a run-traffic tail, so it has no Ctrl-C-to-skip window.
		return openPulse(projectID, runID, headless, nil /*pi*/, stdout, stderr).code
	default:
		moePrintf(stderr, "pulse: openPulseStage: unknown stage %q\n", stage)
		return 1
	}
}

func init() {
	registerCascadeDispatcher(pulseWorkflow, openPulseStage)
}
