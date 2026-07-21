package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
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
		w, err := LookupWorkflow(md.Workflow)
		if err != nil {
			continue
		}
		stage, marked, err := w.AdvancedTo(root, md, idx)
		if err != nil || stage == "" {
			continue
		}
		// The live half of the double-run guard. AdvancedTo's run.json
		// check only sees a session that already merged back to main:
		// commitSessionStart writes run.json on the session branch, so
		// for the whole duration of an open stage run.Scan reads no
		// session id at all — the exact window the guard exists to
		// close. The branch is the signal that works here, and it is
		// the *only* one that does: this block renders against the
		// pulse's own session worktree (InitialPromptBuilder hands the
		// builder a workRoot), where session.List resolves its worktree
		// paths against the wrong directory and finds nothing. Refs
		// live in the common dir, so HasRef reads true from any
		// worktree, and Close/Abandon both delete the branch.
		if git.HasRef(root, "refs/heads/"+session.BranchName(md.Project, md.ID, stage)) {
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
		"has a session yet — it is waiting for someone to kick it. Grooming it is not kicking it.")
	return sb.String()
}
