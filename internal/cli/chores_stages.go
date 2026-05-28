package cli

import "io"

var openChoresStage func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openChoresStage = func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
		switch stage {
		case choresCodeDoc:
			return openChoresCode(projectID, runID, headless, suppressNextStage, "", stdout, stderr)
		default:
			moePrintf(stderr, "chores: openChoresStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(choresWorkflow, func(stage, projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openChoresStage(stage, projectID, runID, headless, suppressNextStage, stdout, stderr)
	})
}
