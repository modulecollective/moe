package cli

import "io"

// openHooksStage is the Go-level seam behind the chain prompt's
// cascade driver (`!` / `!<stage>` / `!!` / `!!!`) for hooks runs. Same shape
// as openSdlcStage / openTwinStage / openKbStage / openMetaMoeStage.
// Single-case switch — hooks is a one-stage workflow — but the switch
// shape stays for symmetry with the other dispatchers, and so an
// unexpected stage name surfaces as a stderr line rather than silently
// routing somewhere wrong.
//
// Wiring the dispatcher even for a single-stage workflow lets the
// terminal-close prompt and `!!` / `!!!` auto-close land for hooks too: every
// workflow with a close command now follows exactly one rule for "what
// happens after the last stage commits."
var openHooksStage func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openHooksStage = func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
		// Cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession, matching openSdlcStage one workflow over.
		switch stage {
		case hooksCodeDoc:
			return openHooksCode(projectID, runID, headless, suppressNextStage, "", stdout, stderr)
		default:
			moePrintf(stderr, "hooks: openHooksStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(hooksWorkflow, func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openHooksStage(stage, projectID, runID, headless, suppressNextStage, stdout, stderr)
	})
}
