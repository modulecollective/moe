package cli

import "io"

// openReviewStage is the Go-level seam behind the chain prompt's
// cascade driver (`!` / `!<stage>` / `!!`) for review runs. Same shape
// as openSdlcStage / openTwinStage / openKbStage / openMetaMoeStage:
// switch on the stage name, hand to the right helper, run headless,
// carry suppressNextStage through to stageSessionOpts.SkipNextStage.
// An unknown stage surfaces as a stderr line rather than silently
// routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain stays clear of Go's package init-order cycle checker — same
// reason openSdlcStage / openTwinStage use the var-with-no-initializer
// shape.
var openReviewStage func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openReviewStage = func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		switch stage {
		case reviewPlanDoc:
			return openReviewPlan(projectID, runID, true, suppressNextStage, stdout, stderr)
		case reviewReportDoc:
			return openReviewReport(projectID, runID, true, suppressNextStage, stdout, stderr)
		default:
			moePrintf(stderr, "review: openReviewStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerHeadlessDispatcher(reviewWorkflow, func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openReviewStage(stage, projectID, runID, suppressNextStage, stdout, stderr)
	})
}
