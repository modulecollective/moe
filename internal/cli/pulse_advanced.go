package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// The settled-runs block covers decisions already made; the chain-state
// block covers work already sequenced. Both are about runs that are
// moving. This is the third case: a run that stopped moving *with the
// operator's permission to continue* — they hit `a` at a chain prompt,
// which records "this stage is done, I'm just not running the next one
// now."
//
// Nothing carried those runs forward: on 2026-07-19 two runs sat
// design-complete for an evening with nothing to chain them. The marker
// is the one signal on disk that distinguishes "parked forward" from
// "in flight" without guessing, so the sweep gets it as context.
//
// `a` deliberately does not fire a pulse of its own — it means "park it
// for later", not "queue it now", and the operator has verbs for the
// latter. So an `a` that is the last event of the day waits for the
// next pulse fired off some other run's traffic. Accepted: pickup is
// not meant to be immediate.

// operatorAdvancedStage reports the stage md is parked at *with the
// operator's recorded permission to continue*, and the marker's time.
// Two facts, both on disk: an `advance:` marker AdvancedTo still
// honours, and no live session at the stage it points to.
//
// The live half of the double-run guard is the second check.
// AdvancedTo's run.json check only sees a session that already merged
// back to main: commitSessionStart writes run.json on the session
// branch, so for the whole duration of an open stage run.Scan reads no
// session id at all — the exact window the guard exists to close. The
// branch is the signal that works here, and it is the *only* one that
// does: advancedRunsBlock renders against the pulse's own session
// worktree (InitialPromptBuilder hands the builder a workRoot), where
// session.List resolves its worktree paths against the wrong directory
// and finds nothing. Refs live in the common dir, so HasRef reads true
// from any worktree, and Close/Abandon both delete the branch.
//
// One caller: the block below, telling the survey which runs are worth
// grooming. pulseSelfKick used to take the same answer as license to
// *start* one; it now admits on a settled design (rootDesignSettled),
// which subsumes this shape — a valid marker satisfies the stage, so an
// advanced run reads as past its first stage.
func operatorAdvancedStage(root string, md *run.Metadata, idx *run.JournalIndex) (string, time.Time, bool) {
	var zero time.Time
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", zero, false
	}
	stage, marked, err := w.AdvancedTo(root, md, idx)
	if err != nil || stage == "" {
		return "", zero, false
	}
	if git.HasRef(root, "refs/heads/"+session.BranchName(md.Project, md.ID, stage)) {
		return "", zero, false
	}
	return stage, marked, true
}

// advancedRun is one row of the block.
type advancedRun struct {
	id     string
	wf     string
	stage  string
	marked time.Time
	title  string
}

// advancedRunsBlock renders the advanced-runs context block, or "" when
// no run in the project is waiting on an advance marker. Best-effort
// like its siblings in pulseKickoffWithContext: a block that finds
// nothing renders nothing rather than failing the sweep.
//
// No age window and no cap, unlike settledRunsBlock: an advanced run is
// a live obligation that does not expire, and the list is bounded by
// how many runs the operator has personally clicked forward. Oldest
// marker first — the longest-stranded run is the one most worth a
// thread.
func advancedRunsBlock(sc *pulseScan, projectID string) string {
	root, mds, idx := sc.root, sc.mds, sc.idx
	var rows []advancedRun
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		stage, marked, ok := operatorAdvancedStage(root, md, idx)
		if !ok {
			continue
		}
		rows = append(rows, advancedRun{
			id:     md.ID,
			wf:     md.Workflow,
			stage:  stage,
			marked: marked,
			title:  settledRunTitle(root, md),
		})
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].marked.Equal(rows[j].marked) {
			return rows[i].marked.Before(rows[j].marked)
		}
		return rows[i].id < rows[j].id
	})

	now := time.Now()
	var sb strings.Builder
	sb.WriteString("Runs the operator advanced and left (oldest first) — parked mid-workflow with " +
		"explicit permission to carry on:\n\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "- `%s` (%s) — waiting at **%s**, advanced %s — %s\n",
			r.id, r.wf, r.stage, dash.HumanAgo(now, r.marked), r.title)
	}
	sb.WriteString("\nEach of these reached a chain prompt and the operator chose \"advance, don't run now\". " +
		"That marker is consent to carry the run forward, which is more than a machine-spawned fix run has: " +
		"an advanced run clears the lane bar's ordering question on its own, so grooming one onto a thread " +
		"(`chain`, `onto` an existing lane or its own group) is the normal move, not a stretch. Nothing here " +
		"has a session yet — it is waiting for someone to kick it. Grooming one is not kicking it, but a " +
		"thread rooted at one of these may carry `\"kick\": true` like any other — the marker is the consent " +
		"that admits it, and the kick bar is unchanged.")
	return sb.String()
}
