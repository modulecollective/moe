package cli

import (
	"encoding/json"
	"fmt"

	"github.com/modulecollective/moe/internal/run"
)

// maxReviewTurns is the hard cap on the headless review → build loop:
// once a run's journal holds this many review work-turns, the machine
// never kicks it back to build again. The deepest run on record needed
// six; the cap bounds the cost of a reviewer that never escalates.
const maxReviewTurns = 10

// reviewTurns reads the run's journal for its review work-turn count
// and whether review, rather than build, landed the newest of the two.
// A seam so a test can pin a count that never grows.
var reviewTurns = func(root string, md *run.Metadata) (count int, reviewLast bool, err error) {
	docs, err := run.WorkTurnDocs(root, md.Project, md.ID)
	if err != nil {
		return 0, false, err
	}
	for _, d := range docs {
		if d == "review" {
			count++
		}
	}
	for _, d := range docs {
		if d == "review" || d == "build" {
			reviewLast = d == "review"
			break
		}
	}
	return count, reviewLast, nil
}

// reviewLoopSpent reports why the headless loop must stop kicking a
// blocked review back to build, or "" when another round may run.
// canvas is the review canvas, already known to be blocked. The
// reviewer's escalation is read first — it is the judged exit, the cap
// the mechanical one. A journal read error counts as spent: park rather
// than spin.
func reviewLoopSpent(root string, md *run.Metadata, canvas string) string {
	if reviewGateEscalated(canvas) {
		return "the reviewer escalated — another build round won't clear it"
	}
	n, _, err := reviewTurns(root, md)
	if err != nil {
		return fmt.Sprintf("review turns could not be counted (%v)", err)
	}
	if n >= maxReviewTurns {
		return fmt.Sprintf("review has run %d times (cap %d)", n, maxReviewTurns)
	}
	return ""
}

// reviewGateEscalated reports whether a review canvas closed
// `{"status":"blocked","escalate":true}`. escalate only means something
// on a blocked gate; every other reader sees the status alone, so an
// escalated block still reads as blocked everywhere else.
func reviewGateEscalated(canvas string) bool {
	payload, ok := stageGateJSON(canvas)
	if !ok {
		return false
	}
	var p struct {
		Status   string `json:"status"`
		Escalate bool   `json:"escalate"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	return p.Status == "blocked" && p.Escalate
}

// reviewLoopHold is the kick floor's leg for a run parked at a blocked
// review the loop has given up on. Without it every armed sweep would
// re-run review on an unchanged tree and park again. Only a run whose
// newest build-or-review turn is the blocked review is held — once
// build has answered (an operator's hand kickback), the run waits for
// review, not at it. An undelivered note releases it for one turn, the
// same release a design-only park takes.
func reviewLoopHold(root, threadRoot string, md *run.Metadata, groomed groomResult) string {
	if md.Workflow != "sdlc" {
		return ""
	}
	canvas := readPrintableCanvas(root, md, "review")
	if status, ok := stageGateStatus(canvas); !ok || status != "blocked" {
		return ""
	}
	if _, reviewLast, err := reviewTurns(root, md); err == nil && !reviewLast {
		return ""
	}
	reason := reviewLoopSpent(root, md, canvas)
	if reason == "" || pendingInputOnThread(root, threadRoot, groomed) {
		return ""
	}
	return "is parked at a blocked review: " + reason + " — push a note, kick it back by hand, or close it"
}
