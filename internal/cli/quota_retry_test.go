package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func TestClockCooldownReadsOnceWithoutWaiting(t *testing.T) {
	defer withClockInvoked()()
	for _, name := range []string{"94", "95", "weekly", "multiple", "missing", "past", "denied", "unresolved", "transport", "malformed", "bypass"} {
		t.Run(name, func(t *testing.T) {
			g, now, _ := testCooldown(t)
			q, wantCode, wantRetry := quota(95, now.Add(time.Hour)), exitQuotaDeferred, now.Add(time.Hour+cooldownResetSlack)
			var readErr error
			switch name {
			case "94":
				q.Windows[0].UsedPercent, wantCode = 94, 0
			case "weekly":
				q.Windows[0].ResetsAt, wantRetry = now.Add(7*24*time.Hour), now.Add(7*24*time.Hour+cooldownResetSlack)
			case "multiple":
				q.Windows = append(q.Windows, agent.QuotaWindow{ID: "weekly", UsedPercent: 98, ResetsAt: now.Add(48 * time.Hour)})
				wantRetry = now.Add(48*time.Hour + cooldownResetSlack)
			case "missing":
				q.Windows = append(q.Windows, agent.QuotaWindow{ID: "other", UsedPercent: 96})
				wantRetry = *now
			case "past":
				q.Windows[0].ResetsAt, wantRetry = now.Add(-time.Minute), *now
			case "denied":
				q = agent.Quota{Denied: true, Reason: "exhausted"}
				wantRetry = *now
			case "unresolved":
				q = agent.Quota{Unresolved: q.Windows}
				wantCode = exitNotStarted
			case "transport":
				readErr, wantCode = errors.New("offline"), exitNotStarted
			case "malformed":
				readErr, wantCode = agent.ErrQuotaMalformed, exitNotStarted
			case "bypass":
				cooldownSkipped.Store(true)
				wantCode = 0
			}
			reads := 0
			g.openControls = func() (<-chan cooldownControl, func()) { t.Fatal("clock opened terminal controls"); return nil, nil }
			g.after = func(time.Duration) <-chan time.Time { t.Fatal("clock slept"); return nil }
			g.read = func(ctx context.Context, name, model string) (agent.Quota, error) {
				reads++
				if name != "codex" || model != "model" {
					t.Fatalf("wrong selection: %s %s", name, model)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > cooldownReadTimeout {
					t.Fatal("quota read unbounded")
				}
				return q, readErr
			}
			a, m := "codex", "model"
			code := stageCooldown(g, &a, &m, func(*run.Metadata) (string, string, error) { t.Fatal("re-resolved after no wait"); return "", "", nil }, func() (*run.Metadata, error) { t.Fatal("reconciled after no wait"); return nil, nil }, io.Discard)
			wantReads := 1
			if name == "bypass" {
				wantReads = 0
			}
			if code != wantCode || reads != wantReads {
				t.Fatalf("code=%d reads=%d; want %d/%d", code, reads, wantCode, wantReads)
			}
			if code == exitQuotaDeferred && (g.deferred == nil || !g.deferred.retryAt.Equal(wantRetry)) {
				t.Fatalf("deferral=%+v want %s", g.deferred, wantRetry)
			}
		})
	}
}

func TestClockCooldownCancellation(t *testing.T) {
	g, _, _ := testCooldown(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.read = func(ctx context.Context, _, _ string) (agent.Quota, error) {
		cancel()
		<-ctx.Done()
		return agent.Quota{}, ctx.Err()
	}
	if got := g.yield(ctx, "claude", ""); got != cooldownCancel || g.deferred != nil {
		t.Fatalf("outcome=%v deferral=%v", got, g.deferred)
	}
}

func TestQuotaCleanupFailureIsAnError(t *testing.T) {
	root := newTestBureaucracy(t)
	var log bytes.Buffer
	code := runStageTurn(root, stageTurnInputs{
		Project: "tele", RunSlug: "r", DocID: "code", LockPurpose: "test",
		Admit: func(s *session.Session) int {
			writeFile(t, filepath.Join(s.WorktreePath, "unexpected.txt"), "preserve me")
			return exitQuotaDeferred
		},
		BuildSpec: func(string) (stageTurnSpec, error) { t.Fatal("unadmitted stage ran"); return stageTurnSpec{}, nil },
	}, io.Discard, &log)
	if code != 1 || !strings.Contains(log.String(), "preserved") {
		t.Fatalf("code=%d log=%s", code, &log)
	}
}

func TestClockSurveyQuotaDisposalAndRestart(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "unavailable"}[unavailable], func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedSdlcOneShotProject(t, root, "tele")
			t.Setenv("MOE_HOME", root)
			t.Setenv("MOE_SERVE_AGENT", "1")
			t.Chdir(root)
			defer withClockInvoked()()
			defer withRideMode(rideDynamic)()
			reset := time.Now().Add(48 * time.Hour).UTC()
			prev := readStageQuota
			t.Cleanup(func() { readStageQuota = prev })
			reads := 0
			readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
				reads++
				if unavailable {
					return agent.Quota{}, errors.New("offline")
				}
				return quota(99, reset), nil
			}
			var log bytes.Buffer
			code, interrupted := runPulse(root, "tele", "", io.Discard, &log)
			want := exitQuotaDeferred
			if unavailable {
				want = 1
			}
			if code != want || interrupted || reads != 1 {
				t.Fatalf("code=%d interrupted=%v reads=%d: %s", code, interrupted, reads, &log)
			}
			mds, err := run.Scan(root)
			if err != nil || len(mds) != 1 || mds[0].Status != run.StatusClosed {
				t.Fatalf("runs=%v err=%v: %s", mds, err, &log)
			}
			md := mds[0]
			body, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", md.ID, pulseDoc)))
			if err != nil || !strings.Contains(string(body), "No survey started") || strings.Contains(string(body), "agent fills") {
				t.Fatalf("canvas=%s err=%v", body, err)
			}
			sessions, err := session.List(root)
			if err != nil || len(sessions) != 0 {
				t.Fatalf("reservation leaked: %v %v", sessions, err)
			}
			idx, err := run.BuildJournalIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(idx.WorkTurnTime) != 0 {
				t.Fatal("pre-admission stage work committed")
			}
			if unavailable {
				if len(idx.QuotaRetries) != 0 {
					t.Fatal("unavailable quota invented a reset")
				}
				return
			}
			token := "tele/" + md.ID
			retry, ok := idx.QuotaRetries[token]
			if !ok || !retry.NotBefore.Equal(reset.Add(cooldownResetSlack)) || len(retry.Targets) != 1 || retry.Targets[0].Stage != pulseDoc {
				t.Fatalf("retry=%+v", retry)
			}
			g := newHeartbeatGate(root)
			sc := mustPulseScan(t, root)
			p, err := project.Load(root, "tele")
			if err != nil {
				t.Fatal(err)
			}
			if due, _, why := g.projectDue(sc, p, map[string]bool{}, 0, time.Now()); due || !strings.Contains(why, "deferred for quota") {
				t.Fatalf("future retry: due=%v %s", due, why)
			}
			// The retry leg outranks a clean cursor and needs no parked run.
			g.Swept("tele", true)
			if due, _, why := g.projectDue(sc, p, map[string]bool{}, 0, retry.NotBefore); !due {
				t.Fatalf("due retry hidden: %s", why)
			}
			if due, held, _ := g.projectDue(sc, p, map[string]bool{"tele": true}, 0, retry.NotBefore); due || !held {
				t.Fatal("retry ignored occupancy")
			}
			p.Mode = string(project.ModePaused)
			if due, _, _ := g.projectDue(sc, p, nil, 0, retry.NotBefore); due {
				t.Fatal("retry ignored pause")
			}
			// A failed retry leaves the token. A clean new survey discharges
			// only the obligations it observed, without needing a stage ride.
			prevOpen := openPulse
			openPulse = func(projectID, runID string, _ bool, _ *pulseInterrupt, _, _ io.Writer) surveyOutcome {
				return surveyOutcome{code: 1, agentStarted: true}
			}
			if code, _ := runPulse(root, "tele", "", io.Discard, &log); code != 1 {
				t.Fatalf("failed retry code=%d", code)
			}
			idx, _ = run.BuildJournalIndex(root)
			if _, ok := idx.QuotaRetries[token]; !ok {
				t.Fatal("failed survey discharged retry")
			}
			openPulse = func(projectID, runID string, _ bool, _ *pulseInterrupt, _, _ io.Writer) surveyOutcome {
				writePulseGate(t, root, projectID, runID, `{"status":"ok"}`)
				return surveyOutcome{agentStarted: true}
			}
			t.Cleanup(func() { openPulse = prevOpen })
			if code, _ := runPulse(root, "tele", "", io.Discard, &log); code != 0 {
				t.Fatalf("clean retry code=%d: %s", code, &log)
			}
			idx, _ = run.BuildJournalIndex(root)
			if len(idx.QuotaRetries) != 0 {
				t.Fatalf("clean survey retained retries: %+v", idx.QuotaRetries)
			}
			if len(dueProjectsPastTheWindow(t, newHeartbeatGate(root))) != 0 {
				t.Fatal("clean retry kept waking after restart")
			}
		})
	}
}

func TestQuotaRetryAggregationAndObservedTokens(t *testing.T) {
	root := quietFixture(t)
	defer withRideMode(rideDynamic)()
	seed := func(slug string) *pulseQuotaRetries {
		trailerstest.SeedRun(t, root, "moe", slug, pulseWorkflow, run.StatusClosed)
		writePulseGate(t, root, "moe", slug, `{"status":"ok"}`)
		q, err := newPulseQuotaRetries(root, "moe")
		if err != nil {
			t.Fatal(err)
		}
		q.pulse, q.survey = slug, false
		return q
	}
	def := func(stage string, after time.Duration) quotaDeferral {
		return quotaDeferral{target: run.QuotaTarget{Project: "moe", Run: "work", Stage: stage, Backend: "codex", Reason: "weekly 99%"}, retryAt: time.Now().Add(after).UTC()}
	}
	first := seed("pulse-a")
	if err := first.record(def("code", 48*time.Hour), io.Discard); err != nil {
		t.Fatal(err)
	}
	observed := seed("pulse-observer")
	newer := seed("pulse-newer")
	if err := newer.record(def("test", 24*time.Hour), io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := observed.finish(); err != nil {
		t.Fatal(err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil || len(idx.QuotaRetries) != 1 || idx.QuotaRetries["moe/pulse-newer"].Token == "" {
		t.Fatalf("newer token lost: %+v err=%v", idx, err)
	}
	earlier := def("review", time.Hour)
	if err := newer.record(earlier, io.Discard); err != nil {
		t.Fatal(err)
	}
	idx, err = run.BuildJournalIndex(root)
	r := idx.QuotaRetries["moe/pulse-newer"]
	if err != nil || len(r.Targets) != 2 || !r.NotBefore.Equal(earlier.retryAt) {
		t.Fatalf("aggregate=%+v err=%v", r, err)
	}
	// Future quota pacing does not suppress a fresh operator edit.
	g := newHeartbeatGate(root)
	dueProjectsPastTheWindow(t, g)
	journalCommit(t, root, "moe", "operator: new direction", "")
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 {
		t.Fatalf("future retry swallowed new work: %v", got)
	}
	if !heldFor(dueDecisions(t, newHeartbeatGate(root), testTick), "moe") {
		t.Fatal("retry bypassed quiet window")
	}
}

func TestClockAdmissionPreservesInputAndResumedDirtyWork(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Chdir(root)
	md, err := run.New(root, "tele", run.Options{ID: "work", Workflow: sdlcWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Add(root, "tele", md.ID, "keep this note"); err != nil {
		t.Fatal(err)
	}
	in := stageTurnInputs{Project: "tele", RunSlug: md.ID, DocID: "code", LockPurpose: "test"}
	sess, _, cancel, err := openStageSession(root, in, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sess.WorktreePath, "saved.txt"), "saved")
	if err := cancel(); err == nil {
		t.Fatal("fresh dirty reservation unexpectedly removed")
	}
	defer withClockInvoked()()
	prev := readStageQuota
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		return quota(99, time.Now().Add(time.Hour)), nil
	}
	t.Cleanup(func() { readStageQuota = prev })
	before, _ := git.HEAD(root)
	started := false
	code := runStageSession("tele", md.ID, "code", stageSessionOpts{Headless: true, OnAgentStart: func() { started = true }}, io.Discard, io.Discard)
	after, _ := git.HEAD(root)
	if code != exitQuotaDeferred || started || before != after {
		t.Fatalf("code=%d started=%v journal changed=%v", code, started, before != after)
	}
	if body, err := os.ReadFile(filepath.Join(sess.WorktreePath, "saved.txt")); err != nil || string(body) != "saved" {
		t.Fatalf("lost resumed work: %s %v", body, err)
	}
	f, err := input.Load(root, "tele", md.ID)
	if err != nil || len(f.Pending()) != 1 {
		t.Fatal("quota deferral consumed operator input")
	}
}

func TestQuotaRecordFailureRestoresJournal(t *testing.T) {
	root := quietFixture(t)
	trailerstest.SeedRun(t, root, "moe", "pulse", pulseWorkflow, run.StatusClosed)
	writePulseGate(t, root, "moe", "pulse", `{"status":"ok"}`)
	q, err := newPulseQuotaRetries(root, "moe")
	if err != nil {
		t.Fatal(err)
	}
	q.pulse, q.survey = "pulse", false
	hooks := t.TempDir()
	writeFile(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	before, _ := git.HEAD(root)
	err = q.record(quotaDeferral{target: run.QuotaTarget{Project: "moe", Run: "r", Stage: "code", Backend: "claude", Reason: "spent"}, retryAt: time.Now().UTC()}, io.Discard)
	after, _ := git.HEAD(root)
	status, _ := git.Status(root)
	if err == nil || before != after || len(status) != 0 || q.event.Token != "" {
		t.Fatalf("err=%v status=%v event=%+v", err, status, q.event)
	}
}

type quotaTestAgent struct {
	fakeAgent
	exec func(agent.OneShotRequest) (string, error)
}

func (a *quotaTestAgent) ExecuteOneShot(r agent.OneShotRequest) (string, error) { return a.exec(r) }

func TestClockRideDefersOneThreadAndRunsAnotherBackend(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "deferred", true: "mixed failure"}[failure], func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedSdlcOneShotProject(t, root, "moe")
			t.Setenv("MOE_HOME", root)
			t.Setenv("MOE_FORCE_AGENT", "")
			t.Chdir(root)
			defer withClockInvoked()()
			defer withRideMode(rideDynamic)()
			minted := groomFixture(t, root, "blocked", "behind-blocked", "healthy")
			for _, slug := range []string{"blocked", "behind-blocked", "healthy"} {
				md, _ := run.Load(root, "moe", minted[slug])
				md.Agent = "quota-blocked"
				if slug == "healthy" {
					md.Agent = "quota-healthy"
				}
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
			}
			gittest.Run(t, root, "add", "projects/moe/runs")
			gittest.Run(t, root, "commit", "-m", "choose backends")
			groomed := groomChains(root, "moe", "pulse-groom", []groomGroup{{Runs: runsFrom("blocked", "behind-blocked")}, {Runs: runsFrom("healthy")}}, nil, io.Discard)
			var executed []string
			a := &quotaTestAgent{exec: func(r agent.OneShotRequest) (string, error) {
				doc := filepath.Base(r.SessionCwd)
				executed = append(executed, doc)
				if failure {
					return "", errors.New("real agent failure")
				}
				return "", os.WriteFile(filepath.Join(r.Root, run.ContentPath("moe", minted["healthy"], doc)), []byte("# Completed "+doc+"\n"), 0644)
			}}
			agent.Register("quota-healthy", a)
			agent.Register("quota-blocked", a)
			t.Cleanup(func() { agent.Unregister("quota-healthy"); agent.Unregister("quota-blocked") })
			prev := readStageQuota
			var reads []string
			readStageQuota = func(_ context.Context, name, _ string, _ []string) (agent.Quota, error) {
				reads = append(reads, name)
				if name == "quota-blocked" {
					return quota(99, time.Now().Add(time.Hour)), nil
				}
				return quota(94, time.Time{}), nil
			}
			t.Cleanup(func() { readStageQuota = prev })
			prevGate := checkCascadeStageGate
			checkCascadeStageGate = func(*Workflow, *run.Metadata, string, io.Writer) (bool, int) { return true, 0 }
			t.Cleanup(func() { checkCascadeStageGate = prevGate })
			stubPushFromCascade(t, 0, nil)
			var out bytes.Buffer
			code := pulseSelfKick(root, groomed, &out, &out)
			want, turns := exitQuotaDeferred, 4
			if failure {
				want, turns = 1, 1
			}
			if code != want || len(executed) != turns || len(reads) != turns+1 || reads[0] != "quota-blocked" || reads[1] != "quota-healthy" {
				t.Fatalf("code=%d executed=%v reads=%v: %s", code, executed, reads, &out)
			}
			if !strings.Contains(out.String(), "deferred for quota") {
				t.Fatalf("cascade misreported deferral: %s", &out)
			}
			if stage := openSessionStage(root, groomed.byKey["moe/"+minted["blocked"]]); stage != "" {
				t.Fatalf("blocked reservation survived at %s", stage)
			}
		})
	}
}

func TestCIRepairClaimsAfterQuotaAdmission(t *testing.T) {
	for _, scenario := range []string{"blocked", "test blocked", "occupied", "moved"} {
		t.Run(scenario, func(t *testing.T) {
			realStageSession := runStageSession
			f := newPushFixture(t)
			runStageSession = realStageSession
			t.Chdir(f.root)
			t.Setenv("MOE_HOME", f.root)
			t.Setenv("MOE_FORCE_AGENT", "")
			defer withClockInvoked()()
			defer withRideMode(rideDynamic)()
			md, _ := run.Load(f.root, f.projectID, f.runID)
			md.Status, md.Agent = run.StatusPushed, "quota-repair"
			if err := run.Save(f.root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, f.root, "add", run.Dir(md.Project, md.ID))
			gittest.Run(t, f.root, "commit", "-m", "push PR\n\n"+trailers.Block{Project: md.Project, Run: md.ID, Workflow: sdlcWorkflow, PR: repairURL, Consent: "dynamic"}.String())
			p := redPR(md.Project+"/"+md.ID, "observed")
			current := p
			stubPRChecks(t, &current)
			pulse, err := run.New(f.root, md.Project, run.Options{ID: "quota-pulse", Workflow: pulseWorkflow, SeedDocs: map[string]string{pulseDoc: pulseCanvasSkeleton}})
			if err != nil {
				t.Fatal(err)
			}
			if err := disposePulseRunReason(f.root, md.Project, pulse.ID, "test setup", io.Discard); err != nil {
				t.Fatal(err)
			}
			q, err := newPulseQuotaRetries(f.root, md.Project)
			if err != nil {
				t.Fatal(err)
			}
			q.pulse, q.survey = pulse.ID, false
			previousQuota := clockQuota
			clockQuota = q
			t.Cleanup(func() { clockQuota = previousQuota })
			if scenario == "occupied" {
				gittest.Run(t, f.root, "branch", session.BranchName(md.Project, "other-run", "design"))
			}
			prev := readStageQuota
			reads, executions := 0, 0
			readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
				reads++
				idx, err := run.BuildJournalIndex(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if reads == 1 && len(idx.CIRepairAttempts[p.Run]) != 0 {
					t.Fatal("quota read spent repair claim")
				}
				if scenario == "moved" {
					current.Head = "new-head"
				}
				if scenario == "blocked" || reads > 1 {
					return quota(99, time.Now().Add(time.Hour)), nil
				}
				return quota(1, time.Time{}), nil
			}
			t.Cleanup(func() { readStageQuota = prev })
			a := &quotaTestAgent{exec: func(r agent.OneShotRequest) (string, error) {
				executions++
				idx, err := run.BuildJournalIndex(r.Root)
				if err != nil || len(idx.CIRepairAttempts[p.Run]) != 1 {
					t.Fatalf("executor did not see exactly one claim: %v %v", idx, err)
				}
				if err := repolock.With(f.root, repolock.Options{Purpose: "test-outside-lock", Budget: time.Millisecond}, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, f.clonePath, "commit", "--allow-empty", "-m", "repair CI")
				return "", os.WriteFile(filepath.Join(r.Root, run.ContentPath(md.Project, md.ID, "code")), []byte("# CI repaired\n"), 0644)
			}}
			agent.Register("quota-repair", a)
			t.Cleanup(func() { agent.Unregister("quota-repair") })
			var log bytes.Buffer
			code := driveCIRepair(f.root, md, p, io.Discard, &log)
			idx, err := run.BuildJournalIndex(f.root)
			if err != nil {
				t.Fatal(err)
			}
			wantCode, wantClaims, wantExec, wantReads, wantRetries := 1, 0, 0, 1, 0
			if scenario == "blocked" {
				wantCode = exitQuotaDeferred
				wantRetries = 1
			}
			if scenario == "test blocked" {
				wantClaims, wantExec, wantReads = 1, 1, 2
			}
			if code != wantCode || len(idx.CIRepairAttempts[p.Run]) != wantClaims || executions != wantExec || reads != wantReads || len(idx.QuotaRetries) != wantRetries {
				t.Fatalf("code=%d claims=%v executions=%d reads=%d retries=%v: %s", code, idx.CIRepairAttempts[p.Run], executions, reads, idx.QuotaRetries, &log)
			}
			if scenario == "test blocked" {
				if !strings.Contains(log.String(), "attempt remains spent") {
					t.Fatal(log.String())
				}
				if _, err := claimCIRepair(f.root, p); err == nil {
					t.Fatal("spent repair automatically admitted again")
				}
			}
		})
	}
}
