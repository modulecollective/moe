package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

func repairEvidence(prs []ownedPRChecks, key string) (ownedPRChecks, bool) {
	for _, p := range prs {
		if p.Run == key && p.actionable() {
			return p, true
		}
	}
	return ownedPRChecks{}, false
}

// This predicate is shared by the survey's evidence, kick plan and locked
// claim. It does not authorize execution without a live GitHub recheck.
func ciRepairEligibility(sc *pulseScan, p ownedPRChecks) ([]int, string) {
	md := sc.byKey[p.Run]
	if md == nil || md.Workflow != sdlcWorkflow || md.Status != run.StatusPushed || sc.idx.PRURL[p.Run] != p.PR {
		return nil, "run is no longer a pushed sdlc run with this recorded PR"
	}
	if !p.actionable() {
		return nil, "PR has no current actionable failure"
	}
	f, err := input.Load(sc.root, md.Project, md.ID)
	if err != nil {
		return nil, "could not read operator input: " + err.Error()
	}
	spent := map[int]bool{}
	attempted := false
	for _, a := range sc.idx.CIRepairAttempts[p.Run] {
		if a.PR == p.PR && a.Head == p.Head {
			attempted = true
		}
		for _, id := range a.InputIDs {
			spent[id] = true
		}
	}
	var fresh []int
	for _, e := range f.Pending() {
		if !spent[e.ID] {
			fresh = append(fresh, e.ID)
		}
	}
	if attempted && len(fresh) == 0 {
		return nil, "CI repair attempt already spent for this head; add a fresh operator note to retry"
	}
	if clockInvoked {
		mode, err := project.ReadMode(sc.root, md.Project)
		if err != nil {
			return nil, "could not read project mode: " + err.Error()
		}
		if mode == project.ModePaused {
			return nil, "held by paused mode"
		}
		if mode == project.ModeSafe && len(fresh) == 0 && !operatorMarked(sc.root, md, sc.mds, sc.idx) {
			return nil, "held by safe mode; an undelivered operator note can authorize repair"
		}
	}
	// Pushed children are normally absent from the live chain graph. Restore
	// only this endpoint for the staging/reap checks, without rewiring it.
	byKey := maps.Clone(sc.byKey)
	live := *md
	live.Status = run.StatusInProgress
	byKey[p.Run] = &live
	graph := run.NewChainGraph(sc.idx, byKey)
	sw := groomSweep{graph: graph, byKey: byKey}
	if fence := sw.stagingFenced(p.Run); fence != "" {
		return nil, "held by staging fence " + fence
	}
	if hold := reapHeldThread(graph.Root(p.Run), byKey, graph, sc.idx); hold != "" {
		return nil, hold
	}
	if stage := openSessionStage(sc.root, md); stage != "" {
		return nil, "run has an open session at " + stage
	}
	return fresh, ""
}

func claimCIRepair(root string, observed ownedPRChecks) (*run.Metadata, error) {
	// Network is outside the short journal lock. The locked half validates
	// everything local against the new snapshot, including competing claims.
	current := gatherPRChecks(observed.Run, observed.PR)
	if current.Error != "" {
		return nil, errors.New(current.Error)
	}
	if current.Head != observed.Head || !current.actionable() {
		return nil, errors.New("PR moved, closed, merged, or no longer has actionable failures")
	}
	var md *run.Metadata
	err := repolock.With(root, repolock.Options{Purpose: "ci-repair", Run: observed.Run}, func() error {
		sc, ok := newPulseScan(root)
		if !ok {
			return errors.New("could not read current run state")
		}
		ids, hold := ciRepairEligibility(sc, current)
		if hold != "" {
			return errors.New(hold)
		}
		entries, err := git.Status(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Untracked() {
				return fmt.Errorf("journal has uncommitted changes at %s", entry.Path)
			}
		}
		md = sc.byKey[current.Run]
		if openSessionProjects(root, time.Now())[md.Project] {
			return errors.New("somebody is already inside the project")
		}
		payload, _ := json.Marshal(run.CIRepairAttempt{PR: current.PR, Head: current.Head, InputIDs: ids})
		msg := fmt.Sprintf("claim CI repair: %s\n\n", current.Run) + trailers.Block{
			Run: md.ID, Project: md.Project, Workflow: md.Workflow, Consent: walkConsent(), CIRepair: string(payload),
		}.String()
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	return md, err
}

func kickCIRepair(root, key string, groomed groomResult, stdout, stderr io.Writer) int {
	observed, ok := repairEvidence(groomed.prChecks, key)
	if !ok {
		moePrintf(stderr, "pulse: CI repair %s skipped — no actionable survey evidence\n", key)
		return 0
	}
	md, err := claimCIRepair(root, observed)
	if err != nil {
		moePrintf(stderr, "pulse: CI repair %s skipped — %v\n", key, err)
		return 0
	}
	moePrintf(stderr, "pulse: CI repair %s claimed for head %s\n", key, observed.Head)
	code := driveCIRepair(root, md, observed, stdout, stderr)
	if code != 0 {
		moePrintf(stderr, "pulse: CI repair %s exited %d; attempt remains spent\n", key, code)
	} else {
		moePrintf(stderr, "pulse: CI repair %s updated existing PR %s\n", key, observed.PR)
	}
	return code
}

// A repair is one bounded same-run continuation. Ordinary cascade terminal
// guards stay intact. Registered dispatch and gate checks still own test
// and review; neither old passes nor a push recovery can skip those turns.
func driveCIRepair(root string, md *run.Metadata, p ownedPRChecks, stdout, stderr io.Writer) int {
	clone, err := sandboxClonePath(root, md)
	if err != nil {
		moePrintf(stderr, "CI repair: %v\n", err)
		return 1
	}
	before, err := git.RevParse(clone, "refs/heads/"+branchPrefix+md.ID)
	if err != nil {
		moePrintf(stderr, "CI repair: %v\n", err)
		return 1
	}
	var kickoff strings.Builder
	fmt.Fprintf(&kickoff, "Repair failing CI on the existing PR %s at head %s. Continue this run and branch.\n", p.PR, p.Head)
	for _, c := range p.Checks {
		if c.failed() {
			fmt.Fprintf(&kickoff, "- %s: %s — %s\n", c.Name, c.Verdict, c.URL)
		}
	}
	kickoff.WriteString("\nInvestigate the linked failures, fix and commit. Fresh test and review turns follow, then this PR is updated. Operator input is delivered through the normal run input channel.\n")
	if code := openRecoveryStageSession(md, "code", "test", true, kickoff.String(), stdout, stderr); code != 0 {
		return code
	}
	after, err := git.RevParse(clone, "refs/heads/"+branchPrefix+md.ID)
	if err != nil || before == after || stageNominatedClose(root, md, "code") {
		moePrintln(stderr, "CI repair: code produced no repair commit or nominated close; keeping the pushed run and spent attempt")
		return 1
	}
	wf, _ := LookupWorkflow(md.Workflow)
	dispatcher := lookupCascadeDispatcher(md.Workflow)
	for _, stage := range []string{"test", "review"} {
		if code := dispatcher(stage, md.Project, md.ID, true, stdout, stderr); code != 0 {
			return code
		}
		if ok, code := checkCascadeStageGate(wf, md, stage, stderr); !ok {
			return code
		}
		if stageNominatedClose(root, md, stage) {
			moePrintln(stderr, "CI repair: stage nominated close; keeping the existing PR")
			return 1
		}
	}
	// Force the PR route even if the project's default changed during the
	// ride. The push revalidates the recorded PR again after hooks, before
	// moving the remote, and cannot fall back to creating a replacement PR.
	code, err := pushFromCascade(md.Workflow, []string{"--pr", md.Project + "/" + md.ID}, pushRunOptions{
		HeadlessRecovery: true, SkipTerminalEdit: true, RepairPR: p.PR, RepairHead: p.Head,
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "CI repair: %v; attempt remains spent\n", err)
		if code == 0 {
			return 1
		}
	}
	return code
}
