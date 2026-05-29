package cli

import "io"

// openKbStage is the Go-level seam behind the chain prompt's cascade
// driver (`!` / `!<stage>` / `!!` / `!!!`) for kb runs. Same shape as
// openSdlcStage / openTwinStage one workflow over: switch on the stage
// name, hand to the right helper, run headless. A `default` branch
// surfaces an unknown-stage call as a stderr line rather than silently
// routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain doesn't trip Go's init-order cycle checker — same shape
// openSdlcStage / openTwinStage use, same reason.
var openKbStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openKbStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		// Cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession, matching openSdlcStage one workflow over.
		switch stage {
		case "research":
			return openKbResearch(projectID, runID, headless, "", stdout, stderr)
		case "summarize":
			return openKbSummarize(projectID, runID, headless, "", stdout, stderr)
		default:
			moePrintf(stderr, "kb: openKbStage: unknown stage %q\n", stage)
			return 1
		}
	}
	// Register the dispatcher so the generic chain-prompt / cascade
	// machinery can drive kb stages via workflow name — no
	// workflow-specific switch in stage_next.go.
	registerCascadeDispatcher("kb", func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openKbStage(stage, projectID, runID, headless, stdout, stderr)
	})
}
