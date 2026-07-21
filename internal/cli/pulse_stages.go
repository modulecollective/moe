package cli

import "io"

// openPulseStage routes the cascade driver (`!` / `!<stage>` / `!!` /
// `!!!`) to the pulse workflow's single stage. Assigned in init() as a
// var (not a plain func) to sidestep Go's init-order cycle checker,
// mirroring the other workflows' stage dispatchers.
var openPulseStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openPulseStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		switch stage {
		case pulseDoc:
			// No skip latch on this seam: reopening a pulse stage by hand
			// (or riding a chain into one) is not a run-traffic tail, so it
			// has no Ctrl-C-to-skip window.
			code, _ := openPulse(projectID, runID, headless, "", nil /*pi*/, stdout, stderr)
			return code
		default:
			moePrintf(stderr, "pulse: openPulseStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(pulseWorkflow, func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openPulseStage(stage, projectID, runID, headless, stdout, stderr)
	})
}
