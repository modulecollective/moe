package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

const repairURL = "https://github.com/example/repo/pull/1"

func redPR(key, head string) ownedPRChecks {
	return ownedPRChecks{Run: key, PR: repairURL, State: "OPEN", Head: head, Checks: []prCheck{{Name: "CI / test", Verdict: "FAILURE", URL: "https://example.com/job/1"}}}
}

func stubPRChecks(t *testing.T, p *ownedPRChecks) {
	t.Helper()
	prev := runGH
	runGH = func(args ...string) ([]byte, error) {
		if len(args) != 5 || args[0] != "pr" || args[1] != "view" || args[2] != repairURL || args[3] != "--json" || args[4] != prCheckFields {
			return nil, fmt.Errorf("unexpected gh arguments: %v", args)
		}
		if p.Error != "" {
			return nil, errors.New(p.Error)
		}
		rows := []map[string]string{}
		for _, c := range p.Checks {
			rows = append(rows, map[string]string{"__typename": "CheckRun", "name": c.Name, "status": "COMPLETED", "conclusion": c.Verdict, "detailsUrl": c.URL})
		}
		return json.Marshal(map[string]any{"state": p.State, "url": p.PR, "headRefOid": p.Head, "statusCheckRollup": rows})
	}
	t.Cleanup(func() { runGH = prev })
}

func pushedRepairFixture(t *testing.T) (string, ownedPRChecks) {
	t.Helper()
	root := quietFixture(t)
	trailer := trailers.Block{Run: "fix-ci", Project: "moe", Workflow: "sdlc", PR: repairURL, Consent: "dynamic"}
	trailerstest.SeedRun(t, root, "moe", "fix-ci", sdlcWorkflow, run.StatusPushed)
	seedGitHubProject(t, root, "moe")
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "push: moe/fix-ci\n\n"+trailer.String())
	p := redPR("moe/fix-ci", "head-one")
	stubPRChecks(t, &p)
	return root, p
}

func TestPRCheckVerdicts(t *testing.T) {
	for _, typ := range []string{"CheckRun", "StatusContext"} {
		for _, verdict := range []string{"FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "SUCCESS", "PENDING", "CANCELLED", "NEUTRAL", "SKIPPED", "", "UNKNOWN"} {
			t.Run(typ+"/"+verdict, func(t *testing.T) {
				prev := runGH
				runGH = func(args ...string) ([]byte, error) {
					row := map[string]string{"__typename": typ, "name": "test", "context": "test", "status": "COMPLETED", "conclusion": verdict, "state": verdict, "targetUrl": "https://job", "detailsUrl": "https://job"}
					return json.Marshal(map[string]any{"state": "OPEN", "url": repairURL, "headRefOid": "sha", "statusCheckRollup": []any{row}})
				}
				t.Cleanup(func() { runGH = prev })
				p := gatherPRChecks("moe/fix", repairURL)
				want := slices.Contains([]string{"FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED"}, verdict)
				if p.actionable() != want || p.Checks[0].URL != "https://job" {
					t.Fatalf("got %+v, actionable want %t", p, want)
				}
			})
		}
	}
	t.Run("unfinished failure is not current", func(t *testing.T) {
		prev := runGH
		runGH = func(...string) ([]byte, error) {
			return []byte(`{"state":"OPEN","url":"` + repairURL + `","headRefOid":"sha","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":"FAILURE"}]}`), nil
		}
		t.Cleanup(func() { runGH = prev })
		if p := gatherPRChecks("moe/fix", repairURL); p.actionable() {
			t.Fatalf("unfinished check actionable: %+v", p)
		}
	})
}

func TestPRFailureSnapshotStable(t *testing.T) {
	p := redPR("moe/fix", "sha")
	original := prFailureSnapshot([]ownedPRChecks{p})
	p.Checks = append(p.Checks, prCheck{Name: "lint", Verdict: "PENDING"})
	slices.Reverse(p.Checks)
	if prFailureSnapshot([]ownedPRChecks{p}) != original {
		t.Fatal("pending check/order changed the snapshot")
	}
	p.Head = "next"
	if prFailureSnapshot([]ownedPRChecks{p}) == original {
		t.Fatal("head change invisible")
	}
	p.State = "CLOSED"
	if prFailureSnapshot([]ownedPRChecks{p}) != "" {
		t.Fatal("closed PR wakes repair")
	}
}

func TestOwnedPRGatherUsesOnlyRecordedPushedSDLC(t *testing.T) {
	root := quietFixture(t)
	calls := 0
	prev := runGH
	runGH = func(...string) ([]byte, error) { calls++; return nil, errors.New("offline") }
	t.Cleanup(func() { runGH = prev })
	sc := mustPulseScan(t, root)
	if got := gatherOwnedPRChecks(sc, "moe", io.Discard); len(got) != 0 || calls != 0 {
		t.Fatalf("no owned PRs should read nothing: %+v, %d calls", got, calls)
	}
	trailerstest.SeedRun(t, root, "moe", "owned", sdlcWorkflow, run.StatusPushed)
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "record\n\n"+trailers.Block{Project: "moe", Run: "owned", PR: repairURL}.String())
	sc = mustPulseScan(t, root)
	got := gatherOwnedPRChecks(sc, "moe", io.Discard)
	if len(got) != 1 || calls != 1 || got[0].Error == "" || got[0].actionable() {
		t.Fatalf("offline read: %+v, calls=%d", got, calls)
	}
	if rendered := renderOwnedPRChecks(sc, got); !strings.Contains(rendered, "unavailable: offline") {
		t.Fatal(rendered)
	}
}

func TestRepairClaimBoundAndFreshNote(t *testing.T) {
	root, p := pushedRepairFixture(t)
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	setMode(t, root, "moe", project.ModeSafe)
	if _, err := claimCIRepair(root, p); err == nil || !strings.Contains(err.Error(), "safe mode") {
		t.Fatalf("safe claim: %v", err)
	}
	note, err := input.Add(root, "moe", "fix-ci", "Fix the failed PR checks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimCIRepair(root, p); err != nil {
		t.Fatal(err)
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); subj != "Claim CI repair for "+p.Run {
		t.Fatalf("claim headline = %q", subj)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	attempts := idx.CIRepairAttempts[p.Run]
	if len(attempts) != 1 || !slices.Equal(attempts[0].InputIDs, []int{note.ID}) || attempts[0].Head != p.Head {
		t.Fatalf("claim did not persist note identity: %+v", attempts)
	}
	// Simulate a worker dying before delivery. Its still-pending note is spent.
	if _, err := claimCIRepair(root, p); err == nil || !strings.Contains(err.Error(), "already spent") {
		t.Fatalf("duplicate claim: %v", err)
	}
	// Reopening the journal through a new scan gives the same refusal.
	sc := mustPulseScan(t, root)
	if _, hold := ciRepairEligibility(sc, p); !strings.Contains(hold, "already spent") {
		t.Fatal(hold)
	}
	if _, err := input.Add(root, "moe", "fix-ci", "Retry after I fixed the runner"); err != nil {
		t.Fatal(err)
	}
	if _, err := claimCIRepair(root, p); err != nil {
		t.Fatal(err)
	}
	idx, _ = run.BuildJournalIndex(root)
	if len(idx.CIRepairAttempts[p.Run]) != 2 {
		t.Fatalf("retry not claimed: %+v", idx.CIRepairAttempts)
	}
	md, _ := run.Load(root, "moe", "fix-ci")
	if md.Status != run.StatusPushed {
		t.Fatalf("repair changed lifecycle to %s", md.Status)
	}
}

func TestRepairClaimRechecksExternalState(t *testing.T) {
	for _, change := range []string{"head", "closed", "merged", "green", "unavailable"} {
		t.Run(change, func(t *testing.T) {
			root, observed := pushedRepairFixture(t)
			current := observed
			switch change {
			case "head":
				current.Head = "different"
			case "closed":
				current.State = "CLOSED"
			case "merged":
				current.State = "MERGED"
			case "green":
				current.Checks = nil
			case "unavailable":
				current.Error = "offline"
			}
			stubPRChecks(t, &current)
			if _, err := claimCIRepair(root, observed); err == nil {
				t.Fatal("stale nomination claimed")
			}
			idx, _ := run.BuildJournalIndex(root)
			if len(idx.CIRepairAttempts[observed.Run]) != 0 {
				t.Fatal("failed validation spent attempt")
			}
		})
	}
}

func TestHeartbeatWakesOnlyForNewOfferedPRFailures(t *testing.T) {
	root, p := pushedRepairFixture(t)
	current := p
	current.Checks[0].Verdict = "PENDING"
	stubPRChecks(t, &current)
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
		t.Fatalf("pending woke survey: %v", got)
	}
	current.Checks[0].Verdict = "FAILURE"
	decisions := dueDecisions(t, gate, 0)
	if len(sweepIDs(decisions)) != 1 || decisions[0].PRChecks == "" {
		t.Fatalf("new failure did not wake: %+v", decisions)
	}
	gate.Dispatching()
	var offered []ownedPRChecks
	if err := json.Unmarshal([]byte(decisions[0].PRChecks), &offered); err != nil || !offered[0].actionable() {
		t.Fatalf("missing offered observation: %+v %v", offered, err)
	}
	// A second failure arrives while the first survey runs. Even a Due
	// during that window must not replace the observation offered to it.
	current.Head = "head-two"
	dueDecisions(t, gate, 0)
	gate.Swept("moe", true)
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 1 {
		t.Fatalf("mid-survey failure swallowed: %v", got)
	}
	gate.Dispatching()
	gate.Swept("moe", true)
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
		t.Fatalf("unchanged red resurveyed: %v", got)
	}
	current.Error = "offline"
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
		t.Fatalf("fetch error invented a change: %v", got)
	}
	gate.Swept("moe", true)
	current.Error = ""
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
		t.Fatalf("fetch failure replaced good observation: %v", got)
	}
	current.Checks[0].Verdict = "SUCCESS"
	dueProjectsPastTheWindow(t, gate)
	current.Checks[0].Verdict = "FAILURE"
	if got := dueProjectsPastTheWindow(t, gate); len(got) != 1 {
		t.Fatalf("new red after green did not wake: %v", got)
	}
}

// The operator's criterion for a repair that found nothing to ship: no
// run of dead pulses, decided by the agent-free gate. A clean sweep
// acknowledges the offered failure; the repair's claim commit (ride work
// inside the sweep's window) costs at most one follow-up survey; then the
// spent, unchanged red PR stays quiet.
func TestHeartbeatQuietAfterSpentRepair(t *testing.T) {
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	root, p := pushedRepairFixture(t)
	t.Setenv("MOE_HOME", root)
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	decisions := dueDecisions(t, gate, 0)
	if len(sweepIDs(decisions)) != 1 || decisions[0].PRChecks == "" {
		t.Fatalf("red PR not offered: %+v", decisions)
	}
	gate.Dispatching()
	if _, err := claimCIRepair(root, p); err != nil {
		t.Fatal(err)
	}
	gate.Swept("moe", true)
	wakes := 0
	for range 3 {
		if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
			wakes++
			gate.Dispatching()
			gate.Swept("moe", true)
		}
	}
	if wakes > 1 {
		t.Fatalf("spent repair woke %d follow-up surveys, want at most one", wakes)
	}
	if _, err := claimCIRepair(root, p); err == nil {
		t.Fatal("spent repair re-admitted")
	}
	for range 2 {
		if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
			t.Fatalf("unchanged red PR with a spent attempt woke: %v", got)
		}
	}
}

const prWakeReason = "new failing checks on an owned PR"

// spentQuietGate drives a red PR through one acknowledged wake, claims
// its repair attempt, and drains the claim's follow-up survey. The
// returned check state is what the gh stub serves; mutate it in place.
func spentQuietGate(t *testing.T) (string, *heartbeatGate, *ownedPRChecks) {
	t.Helper()
	root, p := pushedRepairFixture(t)
	t.Setenv("MOE_HOME", root)
	current := p
	current.Checks = slices.Clone(p.Checks)
	stubPRChecks(t, &current)
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	if got := reasonFor(dueDecisions(t, gate, 0), "moe"); got != prWakeReason {
		t.Fatalf("red PR first wake: %q", got)
	}
	gate.Swept("moe", true)
	// Control: on an unspent head a re-run's new failure wakes.
	current.Checks[0].URL = "https://example.com/job/2"
	if got := reasonFor(dueDecisions(t, gate, 0), "moe"); got != prWakeReason {
		t.Fatalf("unspent head re-run did not wake: %q", got)
	}
	gate.Swept("moe", true)
	if _, err := claimCIRepair(root, current); err != nil {
		t.Fatal(err)
	}
	wakes := 0
	for range 3 {
		if len(dueProjectsPastTheWindow(t, gate)) == 0 {
			break
		}
		wakes++
		gate.Swept("moe", true)
	}
	if wakes > 1 {
		t.Fatalf("claim drained in %d sweeps, want at most one", wakes)
	}
	return root, gate, &current
}

// The operator's criterion: no dead pulses. A spent head's changed
// failures cannot be repaired until a note lands, so the gate stays
// quiet for them — across a serve restart too — and the fingerprint it
// declined to acknowledge is still pending when the note arrives.
func TestHeartbeatSpentHeadStaysQuiet(t *testing.T) {
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	root, gate, current := spentQuietGate(t)
	current.Checks[0].URL = "https://example.com/job/3"
	for range 2 {
		if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
			t.Fatalf("re-run on a spent head woke: %v", got)
		}
	}
	current.Checks = append(current.Checks, prCheck{Name: "CI / lint", Verdict: "FAILURE", URL: "https://example.com/job/4"})
	for range 2 {
		if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
			t.Fatalf("new failing check on a spent head woke: %v", got)
		}
	}
	// prSeen is in-memory; a restart must not re-survey the spent head.
	g2 := newHeartbeatGate(root)
	g2.Swept("moe", true)
	if got := dueProjectsPastTheWindow(t, g2); len(got) != 0 {
		t.Fatalf("restart re-surveyed a spent head: %v", got)
	}
	if _, err := input.Add(root, "moe", "fix-ci", "Retry after I fixed the runner"); err != nil {
		t.Fatal(err)
	}
	// The PR reason outranks "the journal moved", so this proves the
	// suppressed fingerprint stayed pending and the fresh note lifted it.
	if got := reasonFor(dueDecisions(t, g2, 0), "moe"); got != prWakeReason {
		t.Fatalf("fresh note on a spent head: %q, want %q", got, prWakeReason)
	}
}

// Attempts are keyed on (PR, head): a push lands a new, unspent head.
func TestHeartbeatSpentPRNewHeadWakes(t *testing.T) {
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	_, gate, current := spentQuietGate(t)
	current.Head = "head-two"
	if got := reasonFor(dueDecisions(t, gate, 0), "moe"); got != prWakeReason {
		t.Fatalf("new head on a spent PR: %q", got)
	}
}

// Only the spent hold suppresses. Safe mode with no mark is durable too,
// but a first failure is news the survey's report delivers.
func TestHeartbeatSafeModeRedPRStillWakes(t *testing.T) {
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	root, p := pushedRepairFixture(t)
	t.Setenv("MOE_HOME", root)
	setMode(t, root, "moe", project.ModeSafe)
	if _, hold := ciRepairEligibility(mustPulseScan(t, root), p); !strings.Contains(hold, "safe mode") {
		t.Fatalf("fixture not held by safe mode: %q", hold)
	}
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	if got := reasonFor(dueDecisions(t, gate, 0), "moe"); got != prWakeReason {
		t.Fatalf("safe-mode red PR: %q, want %q", got, prWakeReason)
	}
}

func TestRepairDriveRunsFreshStagesAndPinsPR(t *testing.T) {
	// A clean turn that stops at its gate parks (exitParked, folded to a
	// clean sweep by pulseRide), and so does a walk whose turns ran
	// cleanly and found nothing to ship; a failed turn keeps its own code
	// even when its gate would also report problems.
	for _, tc := range []struct {
		stop string
		code int
		park string // the gate named in the log, "" for no park
	}{
		{"", 0, ""},
		{"build", 7, ""},
		{"unstamped", exitParked, "build"},
		{"review", 8, ""},
		{"blocked review", exitParked, "review"},
		{"no change", exitParked, ""},
		{"build close", exitParked, ""},
		{"review close", exitParked, ""},
		{"lost branch", 1, ""},
		{"deferred push", 1, ""},
	} {
		stop := tc.stop
		t.Run(stop, func(t *testing.T) {
			f := newPushFixture(t)
			t.Chdir(f.root)
			t.Setenv("MOE_HOME", f.root)
			md, _ := run.Load(f.root, f.projectID, f.runID)
			md.Status = run.StatusPushed
			if err := run.Save(f.root, md); err != nil {
				t.Fatal(err)
			}
			p := redPR(md.Project+"/"+md.ID, "observed-head")
			var stages []string
			priorSession := runStageSession
			runStageSession = func(projectID, runID, doc string, opts stageSessionOpts, _, _ io.Writer) int {
				stages = append(stages, doc)
				if !opts.Headless || !opts.HarvestOnExit || opts.NextStageOverride != "review" || !strings.Contains(opts.InitialPrompt, p.PR) {
					t.Fatalf("recovery options: %+v", opts)
				}
				if doc == stop {
					return 7
				}
				if stop != "no change" {
					gittest.Run(t, f.clonePath, "commit", "--allow-empty", "-m", "repair CI")
				}
				// A repair that leaves build unstamped must stop before
				// review: the derived gate is checked on this exit too.
				switch stop {
				case "unstamped":
				case "build close":
					for _, name := range []string{"code.md", "test.md"} {
						writeBuildCanvas(t, f.root, md, name, strings.Replace(stampedBuildCanvas, "ready", "close", 1))
					}
				default:
					writeStampedBuild(t, f.root, md)
				}
				// An unreadable run branch after the turn is a real
				// error, not a nothing-to-ship stop.
				if stop == "lost branch" {
					gittest.Run(t, f.clonePath, "update-ref", "-d", "refs/heads/"+branchPrefix+md.ID)
				}
				return 0
			}
			t.Cleanup(func() { runStageSession = priorSession })
			priorStage := openSdlcStage
			openSdlcStage = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
				stages = append(stages, stage)
				if !headless {
					t.Fatal("interactive repair stage")
				}
				if stop == "review" || stop == "blocked review" {
					writeStageCanvas(t, f.root, md, "review", escalatedReviewCanvas)
				}
				if stop == "review close" {
					writeStageCanvas(t, f.root, md, "review", closedReviewCanvas)
				}
				if stage == stop {
					return 8
				}
				return 0
			}
			t.Cleanup(func() { openSdlcStage = priorStage })
			priorGate := checkCascadeStageGate
			if stop != "blocked review" {
				checkCascadeStageGate = func(*Workflow, *run.Metadata, string, io.Writer) (bool, int) { return true, 0 }
			}
			t.Cleanup(func() { checkCascadeStageGate = priorGate })
			var deferred *PushDeferredError
			if stop == "deferred push" {
				deferred = &PushDeferredError{Recovery: "hook-failure"}
			}
			pushes := stubPushFromCascade(t, 0, deferred)
			var log bytes.Buffer
			code := driveCIRepair(f.root, md, p, io.Discard, &log)
			if code != tc.code {
				t.Fatalf("%s: code=%d, want %d; stages=%v log=%s", stop, code, tc.code, stages, log.String())
			}
			parked := strings.Contains(log.String(), "parked at its "+tc.park+" gate")
			if tc.park != "" && !parked || tc.park == "" && strings.Contains(log.String(), "pulse: ride ") {
				t.Fatalf("%s: park log wrong (want %q): %s", stop, tc.park, log.String())
			}
			switch stop {
			case "unstamped":
				if !slices.Equal(stages, []string{"build"}) {
					t.Fatalf("unstamped build ran past its gate: %v", stages)
				}
			case "blocked review", "review close":
				if !slices.Equal(stages, []string{"build", "review"}) {
					t.Fatalf("%s stages: %v", stop, stages)
				}
			case "no change", "build close", "lost branch":
				if !slices.Equal(stages, []string{"build"}) {
					t.Fatalf("%s ran past build: %v", stop, stages)
				}
			}
			nothingLine := map[string]string{
				"no change":    "no repair commit or nominated close",
				"build close":  "no repair commit or nominated close",
				"review close": "stage nominated close",
			}[stop]
			if nothingLine != "" && !strings.Contains(log.String(), nothingLine) {
				t.Fatalf("%s: stop line %q missing: %s", stop, nothingLine, log.String())
			}
			if stop == "lost branch" && strings.Contains(log.String(), "no repair commit") {
				t.Fatalf("lost branch read as nothing to ship: %s", log.String())
			}
			if stop == "" {
				if code != 0 || !slices.Equal(stages, []string{"build", "review"}) || len(*pushes) != 1 {
					t.Fatalf("drive code=%d stages=%v pushes=%v: %s", code, stages, *pushes, log.String())
				}
				push := (*pushes)[0]
				if !slices.Contains(push.args, "--pr") || push.options.RepairPR != p.PR || push.options.RepairHead != p.Head {
					t.Fatalf("push not pinned: %+v", push)
				}
			} else if code == 0 {
				t.Fatalf("%s falsely succeeded: stages=%v log=%s", stop, stages, log.String())
			}
			if stop != "" && stop != "deferred push" && len(*pushes) != 0 {
				t.Fatalf("%s shipped: %v", stop, *pushes)
			}
		})
	}
}

const closedReviewCanvas = "# Review\n\n## Gate\n\n```json\n{\"status\":\"close\"}\n```\n\n## Findings\n\nThe repair already landed on default.\n"

// A repair that parks at a gate reaches the sweep as a clean exit: the
// kick says nothing past rideParked's line, and pulseRide's fold makes
// it 0 so the heartbeat's shared backoff stays cool.
func TestKickCIRepairParkedIsClean(t *testing.T) {
	f := newPushFixture(t)
	t.Chdir(f.root)
	t.Setenv("MOE_HOME", f.root)
	md, _ := run.Load(f.root, f.projectID, f.runID)
	md.Status = run.StatusPushed
	if err := run.Save(f.root, md); err != nil {
		t.Fatal(err)
	}
	prior := runStageSession
	runStageSession = func(_, _, _ string, _ stageSessionOpts, _, _ io.Writer) int {
		gittest.Run(t, f.clonePath, "commit", "--allow-empty", "-m", "repair CI")
		return 0 // build left unstamped
	}
	t.Cleanup(func() { runStageSession = prior })
	pushes := stubPushFromCascade(t, 0, nil)
	key := md.Project + "/" + md.ID
	var log bytes.Buffer
	code := kickCIRepair(f.root, key, []ownedPRChecks{redPR(key, "observed-head")}, io.Discard, &log)
	if code != exitParked || combinePulseExit(0, code) != 0 {
		t.Fatalf("kick exit %d (folded %d), want exitParked folding to 0: %s", code, combinePulseExit(0, code), log.String())
	}
	if strings.Contains(log.String(), "exited 78") || !strings.Contains(log.String(), "parked at its build gate") {
		t.Fatalf("parked kick log: %s", log.String())
	}
	if len(*pushes) != 0 {
		t.Fatalf("parked repair shipped: %v", *pushes)
	}
}

// A repair whose build finds nothing to ship reaches the sweep as a clean
// exit too, with no "exited" line past the stop's own.
func TestKickCIRepairNothingToShipIsClean(t *testing.T) {
	f := newPushFixture(t)
	t.Chdir(f.root)
	t.Setenv("MOE_HOME", f.root)
	md, _ := run.Load(f.root, f.projectID, f.runID)
	md.Status = run.StatusPushed
	if err := run.Save(f.root, md); err != nil {
		t.Fatal(err)
	}
	prior := runStageSession
	runStageSession = func(_, _, _ string, _ stageSessionOpts, _, _ io.Writer) int {
		writeStampedBuild(t, f.root, md)
		return 0 // no repair commit
	}
	t.Cleanup(func() { runStageSession = prior })
	pushes := stubPushFromCascade(t, 0, nil)
	key := md.Project + "/" + md.ID
	var log bytes.Buffer
	code := kickCIRepair(f.root, key, []ownedPRChecks{redPR(key, "observed-head")}, io.Discard, &log)
	if code != exitParked || combinePulseExit(0, code) != 0 {
		t.Fatalf("kick exit %d (folded %d), want exitParked folding to 0: %s", code, combinePulseExit(0, code), log.String())
	}
	if strings.Contains(log.String(), "exited ") || !strings.Contains(log.String(), "no repair commit or nominated close") {
		t.Fatalf("nothing-to-ship kick log: %s", log.String())
	}
	if len(*pushes) != 0 {
		t.Fatalf("nothing-to-ship repair shipped: %v", *pushes)
	}
}

func TestRepairClaimsAcrossProcesses(t *testing.T) {
	root, _ := pushedRepairFixture(t)
	var children []*exec.Cmd
	var outputs []*bytes.Buffer
	for range 2 {
		out := new(bytes.Buffer)
		cmd := exec.Command(os.Args[0], "-test.run=^TestRepairClaimChild$")
		cmd.Env = append(os.Environ(), "MOE_CI_CLAIM_TEST_ROOT="+root)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
		outputs = append(outputs, out)
	}
	claimed := 0
	for i, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child: %v: %s", err, outputs[i])
		}
		if strings.Contains(outputs[i].String(), "CLAIMED") {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, outputs=%v", claimed, outputs)
	}
	idx, _ := run.BuildJournalIndex(root)
	if len(idx.CIRepairAttempts["moe/fix-ci"]) != 1 {
		t.Fatalf("concurrent claims: %+v", idx.CIRepairAttempts)
	}
}

func TestRepairClaimChild(t *testing.T) {
	root := os.Getenv("MOE_CI_CLAIM_TEST_ROOT")
	if root == "" {
		return
	}
	p := redPR("moe/fix-ci", "head-one")
	stubPRChecks(t, &p)
	if _, err := claimCIRepair(root, p); err != nil {
		if !strings.Contains(err.Error(), "already spent") {
			t.Fatal(err)
		}
		fmt.Println("HELD")
	} else {
		fmt.Println("CLAIMED")
	}
}

func TestRepairShipUpdatesSamePRWithoutMerging(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MOVED"} {
		t.Run(state, func(t *testing.T) {
			f := newPushFixture(t)
			t.Chdir(f.root)
			t.Setenv("MOE_HOME", f.root)
			const remote = "https://github.com/example/repo.git"
			addInsteadOfRewrite(t, remote, f.origin)
			writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"), `{"id":"tele","ship":"merge","submodule":"projects/tele/src","remote":"`+remote+`","default_branch":"main"}`)
			md, _ := run.Load(f.root, f.projectID, f.runID)
			md.Status = run.StatusPushed
			if err := run.Save(f.root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, f.root, "add", "-A")
			gittest.Run(t, f.root, "commit", "-m", "push\n\n"+trailers.Block{Project: md.Project, Run: md.ID, PR: repairURL}.String())
			gittest.Run(t, f.clonePath, "remote", "set-url", "origin", f.origin)
			gittest.Run(t, f.clonePath, "push", "-u", "origin", f.branch)
			mainBefore := gittest.Output(t, f.origin, "rev-parse", "refs/heads/main")
			p := redPR(md.Project+"/"+md.ID, f.tipSHA)
			current := p
			stubPRChecks(t, &current)
			var stages []string
			prev := runStageSession
			runStageSession = func(projectID, runID, doc string, opts stageSessionOpts, _, _ io.Writer) int {
				stages = append(stages, doc)
				if !opts.Headless {
					t.Fatal("repair became interactive")
				}
				if doc == "build" {
					writeFile(t, filepath.Join(f.clonePath, "ci-fix.txt"), "fixed\n")
					gittest.Run(t, f.clonePath, "add", "ci-fix.txt")
					gittest.Run(t, f.clonePath, "commit", "-m", "fix CI")
					writeStampedBuild(t, f.root, md)
					gittest.Run(t, f.root, "add", run.ContentDir(projectID, runID, doc))
					gittest.Run(t, f.root, "commit", "-m", "work: update build\n\n"+trailers.Block{Project: projectID, Run: runID, Document: doc, Workflow: sdlcWorkflow, Consent: "dynamic"}.String())
					return 0
				}
				body := "# " + doc + "\n\nRepair work for this iteration.\n"
				switch doc {
				case "review":
					body += "\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n"
					if state == "CLOSED" {
						current.State = "CLOSED"
					}
					if state == "MOVED" {
						current.Head = "another-head"
					}
				case "push":
					body += "\n## PR body\n\nFix CI on the existing PR.\n"
				}
				writeContent(t, f.root, projectID, runID, doc, body)
				gittest.Run(t, f.root, "add", run.ContentPath(projectID, runID, doc))
				gittest.Run(t, f.root, "commit", "-m", "work: update "+doc+"\n\n"+trailers.Block{Project: projectID, Run: runID, Document: doc, Workflow: sdlcWorkflow, Consent: "dynamic"}.String())
				return 0
			}
			t.Cleanup(func() { runStageSession = prev })
			var log bytes.Buffer
			code := driveCIRepair(f.root, md, p, io.Discard, &log)
			if state == "OPEN" {
				if code != 0 || !slices.Equal(stages, []string{"build", "review", "push"}) {
					t.Fatalf("code=%d stages=%v log=%s", code, stages, log.String())
				}
				remoteHead := strings.TrimSpace(gittest.Output(t, f.origin, "rev-parse", "refs/heads/"+f.branch))
				if remoteHead == f.tipSHA {
					t.Fatal("existing branch was not updated")
				}
			} else {
				if code == 0 {
					t.Fatalf("%s PR shipped: %s", state, log.String())
				}
				remoteHead := strings.TrimSpace(gittest.Output(t, f.origin, "rev-parse", "refs/heads/"+f.branch))
				if remoteHead != f.tipSHA {
					t.Fatalf("%s PR changed remote: %s", state, remoteHead)
				}
			}
			if got := gittest.Output(t, f.origin, "rev-parse", "refs/heads/main"); got != mainBefore {
				t.Fatal("repair followed merge default")
			}
			fresh, _ := run.Load(f.root, md.Project, md.ID)
			if fresh.Status != run.StatusPushed {
				t.Fatalf("repair changed lifecycle: %s", fresh.Status)
			}
			idx, _ := run.BuildJournalIndex(f.root)
			if idx.PRURL[p.Run] != repairURL {
				t.Fatal("repair replaced the recorded PR")
			}
		})
	}
}

func TestRepairDeliveredNoteDoesNotLicenseNextHead(t *testing.T) {
	root, p := pushedRepairFixture(t)
	defer withClockInvoked()()
	setMode(t, root, "moe", project.ModeSafe)
	note, err := input.Add(root, "moe", "fix-ci", "Fix CI")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimCIRepair(root, p); err != nil {
		t.Fatal(err)
	}
	if err := input.MarkDelivered(root, "moe", "fix-ci", "code", []int{note.ID}, "dynamic"); err != nil {
		t.Fatal(err)
	}
	p.Head = "next-head"
	stubPRChecks(t, &p)
	if _, err := claimCIRepair(root, p); err == nil || !strings.Contains(err.Error(), "safe mode") {
		t.Fatalf("consumed note licensed next head: %v", err)
	}
}

func TestRepairManualDynamicConsentIgnoresPausedMode(t *testing.T) {
	root, p := pushedRepairFixture(t)
	setMode(t, root, "moe", project.ModePaused)
	prior := clockInvoked
	clockInvoked = false
	t.Cleanup(func() { clockInvoked = prior })
	defer withRideMode(rideDynamic)()
	if _, err := claimCIRepair(root, p); err != nil {
		t.Fatalf("manual consent held: %v", err)
	}
}
