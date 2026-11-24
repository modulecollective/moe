package serve

// Hover help for the web UI's action chips: one sentence each on what a
// press does, rendered as a native title attribute so nothing shows
// until asked for. TestEveryActionChipHasATitle keeps every chip wired.

// tips is keyed by the chip's bare word. The three families — run
// controls, project modes, ship routes — share no words, and the
// controls arrive from the CLI as opaque strings, so an unknown word
// renders an empty title rather than failing the page.
var tips = map[string]string{
	// Run controls (internal/runcontrol's kinds).
	"pause":   "Hold this run. Its owner finishes the work in hand, then stops; it stays held until you resume.",
	"handoff": "The same hold as pause, marked as yours to pick up. The status block prints the stage command that continues the conversation at a terminal.",
	"park":    "Ask the current owner to finish the work in hand and let go, once. Nothing is held; a later sweep may pick the run up.",
	"finish":  "Ship this run. From its next boundary it carries on unattended (its own !!) through the remaining gates to the ship route. This run only.",
	"resume":  "Release the hold. Starts nothing; the run waits for the next eligible sweep.",

	// Project modes (project.Modes).
	"paused": "The survey leaves this project alone and starts nothing here.",
	"safe":   "Surveyed, but only runs you marked can start (an advance, a tag, a chore, an undelivered note or answer).",
	"auto":   "The next pick starts if it clears the floor. The default.",

	// Ship routes (project.Ships).
	"pr":    "Finished runs push a branch and open a PR; the run stays pushed until it merges.",
	"merge": "Finished runs fast-forward the default branch and drop the sandbox.",

	// Operator input; answer renders in two templates.
	"answer": "Answer the open question; delivered at the run's next turn.",
}

// tip is the template func behind title="{{tip .}}".
func tip(name string) string { return tips[name] }

// The run page's chips carry composed labels ("advance past build",
// "tag sdlc, design only"), so their titles are built beside the label
// at each runAction construction site rather than looked up.

func advanceTip(stage string) string {
	return "Approve the " + stage + " canvas. The next pickup starts at the following stage instead of reopening this one; starts nothing itself."
}

func tagTip(wf string) string {
	return "License the machine to start this idea as a " + wf + " run and ride it to ship."
}

const (
	closeRunTip      = "Close without shipping: release the workspace, harvest followups and feedback, mark the run closed."
	closeCaptureTip  = "Close this capture; it leaves the board. Starts nothing."
	reopenIdeaTip    = "Put this closed idea back on the board."
	editTip          = "Edit the text."
	untagTip         = "Withdraw the licence; the idea waits for you."
	tagDesignOnlyTip = "License one design turn, then hold the run for you."
)
