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
	"github.com/modulecollective/moe/internal/session"
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
			return []byte(`{"state":"OPEN","url":"` + repairURL + `","headRefOid":"sha","statusCheckRollup":[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":"FAILURE"}]}`), nil
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

func TestRepairGrammarAndPlacement(t *testing.T) {
	for _, raw := range []string{`{"run":"x","repair":"ci","ask":"why"}`, `{"run":"x","repair":"build"}`, `{"run":"x","repair":null}`, `{"run":"x","repair":"ci","slug":"other"}`, `{"run":"x","repair":"ci","typo":true}`} {
		var e pulseThreadEntry
		if err := json.Unmarshal([]byte(raw), &e); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	root, p := pushedRepairFixture(t)
	var gate pulseGate
	if err := decodeStrict([]byte(`{"status":"ok","threads":[{"runs":[{"run":"fix-ci","repair":"ci"}]}]}`), &gate); err != nil {
		t.Fatal(err)
	}
	before := liveEdges(t, root)
	groups := applyPulseGate(root, "moe", "pulse", gate, io.Discard, io.Discard)
	groomed := groomChains(root, "moe", "pulse", groups, nil, io.Discard)
	groomed.prChecks = []ownedPRChecks{p}
	plan := planKick(root, groomed)
	if len(plan.Steps) != 1 || !plan.Steps[0].Repair || plan.Steps[0].Root != p.Run || plan.Steps[0].Hold != "" {
		t.Fatalf("plan: %+v", plan)
	}
	if fmt.Sprint(before) != fmt.Sprint(liveEdges(t, root)) {
		t.Fatal("repair rewired chains")
	}
	for _, th := range []pulseThread{
		{Onto: "another", Runs: gate.Threads[0].Runs},
		{Head: "batch", Runs: gate.Threads[0].Runs},
		{Runs: append(gate.Threads[0].Runs, pulseThreadEntry{Existing: "other"})},
	} {
		if got := applyPulseGate(root, "moe", "pulse", pulseGate{Threads: []pulseThread{th}}, io.Discard, io.Discard); len(got) != 0 {
			t.Fatalf("accepted invalid placement: %+v", th)
		}
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

func TestRepairClaimRechecksLocalHolds(t *testing.T) {
	for _, hold := range []string{"paused", "occupied", "staging", "reaped", "commit failure"} {
		t.Run(hold, func(t *testing.T) {
			root, p := pushedRepairFixture(t)
			defer withClockInvoked()()
			switch hold {
			case "paused":
				setMode(t, root, "moe", project.ModePaused)
			case "occupied":
				gittest.Run(t, root, "branch", session.BranchName("moe", "fix-ci", "code"))
			case "staging":
				trailerstest.SeedRun(t, root, "moe", "batch", chainWorkflow, run.StatusInProgress)
				seedGitHubProject(t, root, "moe")
				gittest.Run(t, root, "commit", "--allow-empty", "-m", "stage\n\n"+trailers.Block{ChainedTo: []string{"moe/batch " + p.Run}}.String())
			case "reaped":
				md, _ := run.Load(root, "moe", "fix-ci")
				md.Reaped = &run.ReapNote{Doc: "code", At: "2999-01-01T00:00:00Z"}
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
			case "commit failure":
				hooks := t.TempDir()
				writeFile(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 1\n")
				if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "config", "core.hooksPath", hooks)
			}
			if _, err := claimCIRepair(root, p); err == nil {
				t.Fatalf("claim ignored %s", hold)
			} else {
				want := map[string]string{"paused": "paused mode", "occupied": "open session", "staging": "staging fence", "reaped": "reaped", "commit failure": "exit status"}[hold]
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("hold %s failed for wrong reason: %v", hold, err)
				}
			}
			idx, _ := run.BuildJournalIndex(root)
			if len(idx.CIRepairAttempts[p.Run]) != 0 {
				t.Fatal("hold wrote a claim")
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

func TestRepairDriveRunsFreshStagesAndPinsPR(t *testing.T) {
	for _, stop := range []string{"", "code", "test", "review", "no change", "deferred push"} {
		t.Run(stop, func(t *testing.T) {
			f := newPushFixture(t)
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
				if !opts.Headless || opts.NextStageOverride != "test" || !strings.Contains(opts.InitialPrompt, p.PR) {
					t.Fatalf("recovery options: %+v", opts)
				}
				if doc == stop {
					return 7
				}
				if stop != "no change" {
					gittest.Run(t, f.clonePath, "commit", "--allow-empty", "-m", "repair CI")
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
				if stage == stop {
					return 8
				}
				return 0
			}
			t.Cleanup(func() { openSdlcStage = priorStage })
			priorGate := checkCascadeStageGate
			checkCascadeStageGate = func(*Workflow, *run.Metadata, string, io.Writer) (bool, int) { return true, 0 }
			t.Cleanup(func() { checkCascadeStageGate = priorGate })
			var deferred *PushDeferredError
			if stop == "deferred push" {
				deferred = &PushDeferredError{Recovery: "hook-failure"}
			}
			pushes := stubPushFromCascade(t, 0, deferred)
			var log bytes.Buffer
			code := driveCIRepair(f.root, md, p, io.Discard, &log)
			if stop == "" {
				if code != 0 || !slices.Equal(stages, []string{"code", "test", "review"}) || len(*pushes) != 1 {
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
				if doc == "code" {
					writeFile(t, filepath.Join(f.clonePath, "ci-fix.txt"), "fixed\n")
					gittest.Run(t, f.clonePath, "add", "ci-fix.txt")
					gittest.Run(t, f.clonePath, "commit", "-m", "fix CI")
				}
				body := "# " + doc + "\n\nRepair work for this iteration.\n"
				switch doc {
				case "test":
					body += "\n## What was verified\n\nThe failing CI path passes in the fixture.\n\n## What wasn't verified\n\nLive GitHub runners are outside this fixture.\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n"
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
				if code != 0 || !slices.Equal(stages, []string{"code", "test", "review", "push"}) {
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

func TestRepairDuplicateNominationDoesNotChangeAnotherStep(t *testing.T) {
	root, p := pushedRepairFixture(t)
	groomed := groomChains(root, "moe", "pulse", []groomGroup{{Repair: "fix-ci"}}, nil, io.Discard)
	groomed.prChecks = []ownedPRChecks{p}
	repair := groomed.threads[0]
	groomed.threads = append(groomed.threads, groomedThread{Root: "moe/other"}, repair)
	plan := planKick(root, groomed)
	if len(plan.Steps) != 2 || !plan.Steps[0].Repair || plan.Steps[1].Repair {
		t.Fatalf("duplicate repair changed another root: %+v", plan)
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
