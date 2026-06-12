package cli

import "io"

// openChatStage is the Go-level seam behind the chain prompt's cascade
// driver (`!` / `!<stage>` / `!!` / `!!!`) for chat runs. Same shape as
// openSdlcStage: switch on the stage name, hand to the
// right helper, return its exit code; an unknown stage surfaces as a
// stderr line rather than routing somewhere wrong.
//
// chat is interactive-only, so this is deliberately thin. The dispatcher
// is registered for surface uniformity — the cascade machinery stays
// workflow-agnostic and doesn't special-case "no dispatcher" for chat —
// but there is no headless chat: the headless flag is ignored and
// openChat always opens an interactive REPL that always sets
// SkipNextStage. The design's "register the dispatcher ... but don't
// wire a yolo path" lands exactly here.
//
// Declared as a var and assigned in init() so the static reference
// chain stays clear of Go's package init-order cycle checker — same
// shape openSdlcStage uses.
var openChatStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openChatStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		switch stage {
		case chatDoc:
			return openChat(projectID, runID, "", stdout, stderr)
		default:
			moePrintf(stderr, "chat: openChatStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher(chatWorkflow, func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openChatStage(stage, projectID, runID, headless, stdout, stderr)
	})
}
