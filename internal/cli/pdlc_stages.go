package cli

import "io"

// openPdlcStage is the Go-level seam behind the chain prompt's cascade
// driver (`!` / `!<stage>` / `!!` / `!!!`) for pdlc runs. Same shape as
// openAuditStage / openSdlcStage: switch on the stage name, hand to the
// right helper, return its exit code; an unknown stage surfaces as a
// stderr line rather than routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain stays clear of Go's package init-order cycle checker — same
// shape openSdlcStage / openAuditStage use.
var openPdlcStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openPdlcStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		// Cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession, matching openAuditStage one workflow over.
		switch stage {
		case pdlcFrameDoc:
			return openPdlcFrame(projectID, runID, headless, "", stdout, stderr)
		case pdlcPrdDoc:
			return openPdlcPrd(projectID, runID, headless, "", stdout, stderr)
		case pdlcChunkDoc:
			return openPdlcChunk(projectID, runID, headless, "", stdout, stderr)
		default:
			moePrintf(stderr, "pdlc: openPdlcStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(pdlcWorkflow, func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openPdlcStage(stage, projectID, runID, headless, stdout, stderr)
	})
}
