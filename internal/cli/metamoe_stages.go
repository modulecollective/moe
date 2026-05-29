package cli

import "io"

// openMetaMoeStage is the Go-level seam behind the chain prompt's
// cascade driver (`!` / `!<stage>` / `!!` / `!!!`) for meta-moe runs. Same
// shape as openSdlcStage / openTwinStage / openKbStage. Single-case
// switch — meta-moe is a one-stage workflow — but the switch shape
// stays for symmetry with the other dispatchers, and so an unexpected
// stage name surfaces as a stderr line rather than silently routing
// somewhere wrong.
//
// Wiring the dispatcher even for a single-stage workflow lets the
// terminal-close prompt and `!!` / `!!!` auto-close land for meta-moe too:
// every workflow with a close command now follows exactly one rule
// for "what happens after the last stage commits."
var openMetaMoeStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openMetaMoeStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		// Cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession, matching openSdlcStage one workflow over.
		switch stage {
		case metaMoeReportDoc:
			return openMetaMoeReport(projectID, runID, headless, "", stdout, stderr)
		default:
			moePrintf(stderr, "meta-moe: openMetaMoeStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(metaMoeWorkflow, func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openMetaMoeStage(stage, projectID, runID, headless, stdout, stderr)
	})
}
