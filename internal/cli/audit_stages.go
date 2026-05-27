package cli

import "io"

// openAuditStage is the Go-level seam behind the chain prompt's
// cascade driver (`!` / `!<stage>` / `!!`) for audit runs. Same shape
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
var openAuditStage func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openAuditStage = func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		switch stage {
		case auditPlanDoc:
			return openAuditPlan(projectID, runID, true, suppressNextStage, stdout, stderr)
		case auditReportDoc:
			return openAuditReport(projectID, runID, true, suppressNextStage, stdout, stderr)
		default:
			moePrintf(stderr, "audit: openAuditStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerHeadlessDispatcher(auditWorkflow, func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openAuditStage(stage, projectID, runID, suppressNextStage, stdout, stderr)
	})
}
