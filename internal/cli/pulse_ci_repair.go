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
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/session"
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
	return ciRepairEligibilityExcept(sc, p, "")
}

func ciRepairEligibilityExcept(sc *pulseScan, p ownedPRChecks, ownBranch string) ([]int, string) {
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
	if stage := openSessionStageExcept(sc.root, md, ownBranch); stage != "" {
		return nil, "run has an open session at " + stage
	}
	if hold := runOwnerHold(sc.root, md); hold != "" {
		return nil, "run " + hold
	}
	if hold := controlHoldPhrase(sc.root, md); hold != "" {
		return nil, "run " + hold
	}
	return fresh, ""
}

func claimCIRepair(root string, observed ownedPRChecks, reserved ...*session.Session) (*run.Metadata, error) {
	ownBranch := ""
	if len(reserved) != 0 {
		ownBranch = reserved[0].Branch
	}
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
		ids, hold := ciRepairEligibilityExcept(sc, current, ownBranch)
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
		if openSessionProjects(root, time.Now(), ownBranch)[md.Project] {
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
	projectID, runID, err := splitProjectRun(key)
	if err != nil {
		moePrintf(stderr, "pulse: CI repair: %v\n", err)
		return 1
	}
	// Taken before the reload and held through the repair's push, so the
	// planner's busy read cannot be overtaken between scan and start.
	h, err := ownRun(root, projectID, runID, true)
	if runowner.IsBusy(err) {
		moePrintf(stderr, "pulse: CI repair %s skipped — %v\n", key, err)
		return 0
	}
	if err != nil {
		moePrintf(stderr, "pulse: CI repair: %v\n", err)
		return 1
	}
	defer func() {
		if err := h(); err != nil {
			moePrintf(stderr, "pulse: CI repair: %v\n", err)
		}
	}()
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "pulse: CI repair %s skipped — %v\n", key, err)
		return 0
	}
	code := driveCIRepair(root, md, observed, stdout, stderr)
	if code == exitYielded {
		moePrintf(stderr, "pulse: CI repair %s yielded to operator control\n", key)
	} else if code != 0 {
		moePrintf(stderr, "pulse: CI repair %s exited %d\n", key, code)
	} else {
		moePrintf(stderr, "pulse: CI repair %s updated existing PR %s\n", key, observed.PR)
	}
	return code
}

// A repair is one bounded same-run continuation. Ordinary cascade terminal
// guards stay intact. The repair is a build turn whose canvases must be
// stamped ready (the derived gate) before a fresh review runs; neither old
// passes nor a push recovery can skip those checks.
func driveCIRepair(root string, md *run.Metadata, p ownedPRChecks, stdout, stderr io.Writer) (code int) {
	claimed := false
	// stopped is the repair step under way: a stop is the outcome of a
	// finish the operator asked for during the repair.
	stopped := "build"
	if clockQuota != nil {
		previous := clockQuota.repairClaimed
		defer func() { clockQuota.repairClaimed = previous }()
	}
	defer func() {
		if claimed && code == exitQuotaDeferred {
			moePrintln(stderr, "CI repair: stopped for quota after claim; attempt remains spent and requires operator recovery or fresh input")
			code = 1
		} else if claimed && code != 0 {
			moePrintln(stderr, "CI repair: stopped; attempt remains spent")
		}
		finishWalkStopped(root, md, cascadeResult{ran: []cascadeStepResult{{stage: stopped}}}, code, stderr)
	}()
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
	kickoff.WriteString("\nInvestigate the linked failures, fix and commit. Record the verification in the build test.md and stamp both build canvases ready. A fresh review turn follows, then this PR is updated. Operator input is delivered through the normal run input channel.\n")
	if code := runStageSession(md.Project, md.ID, "build", stageSessionOpts{
		ValidateEntry: sdlcSessionEntry,
		NeedsSandbox:  true, Headless: true, InitialPrompt: kickoff.String(), NextStageOverride: "review",
		ExtraStagePaths: stageCommitPathsPresent,
		AfterAdmission: func(sess *session.Session) error {
			if _, err := claimCIRepair(root, p, sess); err != nil {
				return err
			}
			claimed = true
			if clockQuota != nil {
				clockQuota.repairClaimed = true
			}
			moePrintf(stderr, "pulse: CI repair %s claimed for head %s\n", p.Run, p.Head)
			return repolock.With(root, repolock.Options{Purpose: "ci-repair-refresh", Run: p.Run}, func() error { return session.Reconcile(sess) })
		},
	}, stdout, stderr); code != 0 {
		return code
	}
	after, err := git.RevParse(clone, "refs/heads/"+branchPrefix+md.ID)
	if err != nil || before == after || stageNominatedClose(root, md, "build") {
		moePrintln(stderr, "CI repair: build produced no repair commit or nominated close; keeping the pushed run and spent attempt")
		return 1
	}
	if code := requireStageReady(root, md, "build", stdout, stderr); code != 0 {
		return code
	}
	wf, _ := LookupWorkflow(md.Workflow)
	dispatcher := lookupCascadeDispatcher(md.Workflow)
	for _, stage := range []string{"review"} {
		stopped = stage
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
	stopped = "push"
	if cc := controlCheckpoint(root, md, "push", stderr); cc != 0 {
		return cc
	}
	// A finish asked during the repair ships through the repair's own PR
	// route: record it as applied before the push, done once it lands.
	finish, finishing := openFinish(root, md)
	if finishing {
		markFinishInFlight(finish.ID, true)
		defer markFinishInFlight(finish.ID, false)
		if err := runcontrol.Settle(root, md.Project, md.ID, finish.ID, runcontrol.Settlement{
			State: runcontrol.StateApplied, Boundary: "push",
		}, walkConsent()); err != nil {
			moePrintf(stderr, "control: finish %s: %v — not pushing\n", finish.ID, err)
			return 1
		}
	}
	// Force the PR route even if the project's default changed during the
	// ride. The push revalidates the recorded PR again after hooks, before
	// moving the remote, and cannot fall back to creating a replacement PR.
	code, err = pushFromCascade(md.Workflow, []string{"--pr", md.Project + "/" + md.ID}, pushRunOptions{
		HeadlessRecovery: true, SkipTerminalEdit: true, RepairPR: p.PR, RepairHead: p.Head,
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "CI repair: %v; attempt remains spent\n", err)
		if code == 0 {
			code = 1
		}
	}
	if finishing {
		s := runcontrol.Settlement{State: runcontrol.StateCompleted, Boundary: "push", Outcome: "shipped: CI repair updated the existing PR"}
		if code != 0 {
			s.State, s.Outcome = runcontrol.StateFailed, fmt.Sprintf("CI repair push stopped (exit %d)", code)
		}
		settleControl(root, md, finish.ID, s, stderr)
	}
	return code
}
