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
	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

func surveyFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_SERVE_AGENT", "1")
	t.Chdir(root)
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertNoSurveyRun(t *testing.T, root, emit string) {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Workflow == pulseWorkflow {
			t.Fatalf("phantom pulse run: %+v", md)
		}
	}
	sessions, err := session.List(root)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%v err=%v", sessions, err)
	}
	if _, err := os.Stat(emit); !os.IsNotExist(err) {
		t.Fatalf("emit-run exists: %v", err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil || len(idx.WorkTurnTime) != 0 {
		t.Fatalf("work turns: %+v %v", idx, err)
	}
	if dirs, _ := filepath.Glob(filepath.Join(root, ".moe", "run-owners", project.RootID, "*")); len(dirs) != 0 {
		t.Fatal(dirs)
	}
}

func TestSurveyQuotaBeforeMint(t *testing.T) {
	for _, scenario := range []string{"exhausted", "unavailable", "missing reset", "past reset", "typed budget"} {
		t.Run(scenario, func(t *testing.T) {
			root := surveyFixture(t)
			defer withRideMode(rideDynamic)()
			if scenario != "typed budget" {
				defer withClockInvoked()()
			}
			reset := time.Now().Add(48 * time.Hour).UTC()
			if scenario == "missing reset" {
				reset = time.Time{}
			}
			if scenario == "past reset" {
				reset = time.Now().Add(-time.Hour)
			}
			oldRead, oldGate := readStageQuota, newCooldownGate
			t.Cleanup(func() { readStageQuota, newCooldownGate = oldRead, oldGate })
			reads := 0
			readStageQuota = func(ctx context.Context, _, _ string, env []string) (agent.Quota, error) {
				reads++
				if len(env) != 0 {
					t.Fatalf("clone-free quota environment: %v", env)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > cooldownReadTimeout {
					t.Fatal("unbounded read")
				}
				if scenario == "unavailable" {
					return agent.Quota{}, errors.New("offline")
				}
				if scenario == "typed budget" {
					return agent.Quota{}, context.Canceled
				}
				return quota(99, reset), nil
			}
			if scenario == "typed budget" {
				newCooldownGate = func(resume string, w io.Writer) *cooldownGate {
					g := oldGate(resume, w)
					g.start = time.Now().Add(-cooldownBudget)
					return g
				}
			}
			emit := filepath.Join(root, ".moe", "emit")
			var log bytes.Buffer
			args := []string{"pulse", "new", "--dynamic", "--emit-run", emit}
			if scenario != "typed budget" {
				args = append(args, "--heartbeat")
			}
			code := Run(args, io.Discard, &log)
			want := exitQuotaDeferred
			if scenario == "unavailable" || scenario == "typed budget" {
				want = 1
			}
			if code != want {
				t.Fatalf("code=%d want=%d: %s", code, want, &log)
			}
			wantReads := 1
			if scenario == "typed budget" {
				wantReads = 0
			}
			if reads != wantReads {
				t.Fatalf("reads=%d", reads)
			}
			assertNoSurveyRun(t, root, emit)
			idx, _ := run.BuildJournalIndex(root)
			if want == 1 {
				if len(idx.QuotaRetries) != 0 {
					t.Fatal("invented retry")
				}
				return
			}
			if len(idx.QuotaRetries) != 1 {
				t.Fatalf("retries=%+v", idx.QuotaRetries)
			}
			for token, r := range idx.QuotaRetries {
				if !r.Survey || !strings.HasPrefix(token, "_cabinet/survey-") || r.Targets[0].Run != "" {
					t.Fatal(r)
				}
				if scenario == "exhausted" && !r.NotBefore.Equal(reset.Add(cooldownResetSlack)) {
					t.Fatal(r)
				}
			}
			msg := gittest.Output(t, root, "log", "-1", "--format=%B")
			if strings.Contains(msg, "MoE-Run:") || strings.Contains(msg, "MoE-Workflow:") || !strings.Contains(msg, "MoE-Consent: dynamic") {
				t.Fatal(msg)
			}
		})
	}
}

func TestSurveyHoldAcrossProjectsAndRestart(t *testing.T) {
	root := surveyFixture(t)
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	oldRead := readStageQuota
	t.Cleanup(func() { readStageQuota = oldRead })
	reads := 0
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		reads++
		return quota(99, time.Now().Add(time.Hour)), nil
	}
	if code, _ := runPulse(root, "", io.Discard, io.Discard); code != exitQuotaDeferred {
		t.Fatal(code)
	}
	// Every wake that used to escape the root project's retry remains pending.
	if _, err := input.AddPulse(root, "start the other project"); err != nil {
		t.Fatal(err)
	}
	journalCommit(t, root, "tele", "operator work", "")
	writeChoreDefinition(t, root, "tele", "due-now")
	idx, _ := run.BuildJournalIndex(root)
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	for _, g := range []*heartbeatGate{newHeartbeatGate(root), newHeartbeatGate(root)} {
		for i := 0; i < 3; i++ {
			decisions := g.Due(io.Discard)
			if len(decisions) < 2 || len(sweepIDs(decisions)) != 0 {
				t.Fatalf("global hold: %+v", decisions)
			}
			if len(g.pending) != 0 || len(g.tips) != 0 || len(g.surveyed) != 0 || len(g.prSeen) != 0 {
				t.Fatal("hold acknowledged observations")
			}
			if code, _ := runPulse(root, "", io.Discard, io.Discard); code != exitQuotaDeferred {
				t.Fatal(code)
			}
		}
	}
	if reads != 1 || gittest.Output(t, root, "rev-parse", "HEAD") != before {
		t.Fatalf("held ticks wrote/probed: reads=%d", reads)
	}
	notes, _ := input.LoadPulse(root)
	if len(notes.Pending()) != 1 {
		t.Fatal("hold consumed note")
	}
	assertNoSurveyRun(t, root, filepath.Join(root, "no-emit"))
	// Deadline is eligible; expiry is not a claim of capacity.
	for _, r := range idx.QuotaRetries {
		r.NotBefore = time.Now().Add(-time.Second)
		if err := commitSurveyRetry(root, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(sweepIDs(newHeartbeatGate(root).Due(io.Discard))) == 0 {
		t.Fatal("expiry did not offer sweep")
	}
	if code, _ := runPulse(root, "", io.Discard, io.Discard); code != exitQuotaDeferred {
		t.Fatal(code)
	}
	if reads != 2 {
		t.Fatal(reads)
	}
	assertNoSurveyRun(t, root, filepath.Join(root, "no-emit"))
}

func TestSurveyAdmissionPinsExecutorSelection(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "started failure"}[fail], func(t *testing.T) {
			root := surveyFixture(t)
			defer withClockInvoked()()
			defer withRideMode(rideDynamic)()
			calls, reads := 0, 0
			a := &quotaTestAgent{exec: func(r agent.OneShotRequest) (string, error) {
				calls++
				if r.Model != "admitted-model" {
					t.Fatalf("executed model %q", r.Model)
				}
				mds, _ := run.Scan(r.Root)
				for _, md := range mds {
					if md.Workflow == pulseWorkflow {
						writePulseGate(t, r.Root, md.Project, md.ID, `{"status":"ok"}`)
					}
				}
				if fail {
					return "", errors.New("agent failed after starting")
				}
				return "", nil
			}}
			agent.Register("survey-test", a)
			t.Cleanup(func() { agent.Unregister("survey-test") })
			prior := run.QuotaRetry{Survey: true, Project: project.RootID, Token: "_cabinet/prior", NotBefore: time.Now().Add(time.Hour), Targets: []run.QuotaTarget{{Project: project.RootID, Stage: pulseDoc, Backend: "claude", Reason: "old selection exhausted"}}}
			if err := commitSurveyRetry(root, prior); err != nil {
				t.Fatal(err)
			}
			gittest.WriteAndCommit(t, root, "model-stylesheet.css", "pulse.pulse { agent: survey-test; model-survey-test: admitted-model; }\n", "survey policy")
			oldRead := readStageQuota
			t.Cleanup(func() { readStageQuota = oldRead })
			readStageQuota = func(_ context.Context, name, model string, _ []string) (agent.Quota, error) {
				reads++
				if name != "survey-test" || model != "admitted-model" {
					t.Fatalf("admitted %s %s", name, model)
				}
				// A policy edit between admission and execution governs the NEXT survey.
				gittest.WriteAndCommit(t, root, "model-stylesheet.css", "pulse.pulse { agent: codex; model-codex: next-model; }\n", "next survey policy")
				return quota(1, time.Time{}), nil
			}
			var log bytes.Buffer
			emit := filepath.Join(root, ".moe", "emit")
			code := Run([]string{"pulse", "new", "--dynamic", "--heartbeat", "--emit-run", emit}, io.Discard, &log)
			want := 0
			if fail {
				want = 1
			}
			if code != want || reads != 1 || calls != 1 {
				t.Fatalf("code=%d reads=%d calls=%d: %s", code, reads, calls, &log)
			}
			mds, _ := run.Scan(root)
			if len(mds) != 1 {
				t.Fatal(mds)
			}
			status := run.StatusClosed
			if fail {
				status = run.StatusInProgress
			}
			if mds[0].Status != status {
				t.Fatal(mds[0])
			}
			if history := gittest.Output(t, root, "log", "--format=%s", "--grep=^MoE-Quota-Retry:"); strings.Count(history, "quota retry: survey admission") != 2 {
				t.Fatal("survey resolution duplicated or missing:", history)
			}

			if b, err := os.ReadFile(emit); err != nil || strings.TrimSpace(string(b)) != mds[0].ID {
				t.Fatalf("emit %s %v", b, err)
			}
		})
	}
}

func TestSurveyRetryPreservesRideAndConcurrentEvidence(t *testing.T) {
	root := surveyFixture(t)
	defer withClockInvoked()()
	ride := run.QuotaRetry{Project: project.RootID, Token: "_cabinet/ride", NotBefore: time.Now().Add(time.Hour), Targets: []run.QuotaTarget{{Project: "tele", Run: "work", Stage: "build", Backend: "claude", Reason: "ride quota"}}}
	if err := commitSurveyRetry(root, ride); err != nil {
		t.Fatal(err)
	}
	gate := newHeartbeatGate(root)
	gate.Due(io.Discard)
	journalCommit(t, root, "tele", "fresh work beside a deferred ride", "")
	if state := surveyAdmissionState(root, time.Now()); state.Held(time.Now()) {
		t.Fatal("ride became survey hold", state)
	}
	if got := gate.Due(io.Discard); len(sweepIDs(got)) == 0 {
		t.Fatal("ride-only deferral held fresh work", got)
	}
	oldRead := readStageQuota
	t.Cleanup(func() { readStageQuota = oldRead })
	concurrent := run.QuotaRetry{Survey: true, Project: project.RootID, Token: "_cabinet/newer", NotBefore: time.Now().Add(2 * time.Hour), Targets: []run.QuotaTarget{{Project: project.RootID, Stage: pulseDoc, Backend: "claude", Reason: "newer observation"}}}
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		if err := commitSurveyRetry(root, concurrent); err != nil {
			t.Fatal(err)
		}
		return quota(99, time.Now().Add(time.Hour)), nil
	}
	if _, code := admitPulseSurvey(root, io.Discard); code != exitQuotaDeferred {
		t.Fatal(code)
	}
	idx, _ := run.BuildJournalIndex(root)
	if len(idx.QuotaRetries) != 3 {
		t.Fatalf("lost evidence: %+v", idx.QuotaRetries)
	}
	if !idx.SurveyQuota(root, "claude", "").Held(time.Now().Add(90 * time.Minute)) {
		t.Fatal("earlier expiry overrode future hold")
	}
	// A ride alone cannot globally hold the survey.
	if err := commitSurveyRetry(root, run.QuotaRetry{Project: project.RootID, Resolves: map[string]string{concurrent.Token: concurrent.Fingerprint()}}); err != nil {
		t.Fatal(err)
	}
}

func TestSurveyRetryCommitFailureLeavesNoRunOrPromise(t *testing.T) {
	root := surveyFixture(t)
	defer withClockInvoked()()
	hooks := t.TempDir()
	writeFile(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	oldRead := readStageQuota
	t.Cleanup(func() { readStageQuota = oldRead })
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		return quota(99, time.Now().Add(time.Hour)), nil
	}
	var log bytes.Buffer
	if code, _ := runPulse(root, "", io.Discard, &log); code != 1 {
		t.Fatal(code)
	}
	assertNoSurveyRun(t, root, filepath.Join(root, "no-emit"))
	idx, _ := run.BuildJournalIndex(root)
	if len(idx.QuotaRetries) != 0 || strings.Contains(log.String(), "retry after") {
		t.Fatalf("false promise: %s", &log)
	}
	if status := gittest.Output(t, root, "status", "--porcelain"); status != "" {
		t.Fatal(status)
	}
}

func TestLegacySurveyRetryIsGlobalAndResolvesWithoutEditingRun(t *testing.T) {
	root := surveyFixture(t)
	md, err := run.New(root, project.RootID, run.Options{ID: "old-skipped", Workflow: pulseWorkflow, SeedDocs: map[string]string{pulseDoc: "# old quota skip\n"}})
	if err != nil {
		t.Fatal(err)
	}
	r := run.QuotaRetry{Project: project.RootID, Token: "_cabinet/old-skipped", NotBefore: time.Now().Add(time.Hour), Targets: []run.QuotaTarget{{Project: project.RootID, Run: md.ID, Stage: pulseDoc, Backend: "claude", Reason: "legacy exhausted"}}}
	block, _ := quotaRetryTrailer(r)
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "legacy quota skip\n\n"+(trailers.Block{Project: project.RootID, Run: md.ID, Workflow: pulseWorkflow, Closed: "true", QuotaRetry: block.QuotaRetry}).String())
	before := gittest.Output(t, root, "rev-parse", "HEAD:projects/_cabinet/runs/old-skipped")
	journalCommit(t, root, "tele", "fresh work", "")
	if len(sweepIDs(newHeartbeatGate(root).Due(io.Discard))) != 0 {
		t.Fatal("legacy hold escaped globally")
	}
	// A typed invocation may recheck early.
	if _, code := admitPulseSurvey(root, io.Discard); code != 0 {
		t.Fatal(code)
	}
	idx, _ := run.BuildJournalIndex(root)
	if len(idx.QuotaRetries) != 0 {
		t.Fatal(idx.QuotaRetries)
	}
	if after := gittest.Output(t, root, "rev-parse", "HEAD:projects/_cabinet/runs/old-skipped"); after != before {
		t.Fatal("legacy history edited")
	}
	// Changing the selected backend also releases a future hold without provider I/O.
	if err := commitSurveyRetry(root, r); err != nil {
		t.Fatal(err)
	}
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if len(sweepIDs(newHeartbeatGate(root).Due(io.Discard))) == 0 {
		t.Fatal("changed selection did not wake")
	}
}

func TestSurveyHoldPreservesPRObservations(t *testing.T) {
	root, p := pushedRepairFixture(t)
	t.Setenv("MOE_FORCE_AGENT", "")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	current := p
	stubPRChecks(t, &current)
	e := run.QuotaRetry{Survey: true, Project: project.RootID, Token: "_cabinet/hold", NotBefore: time.Now().Add(time.Hour), Targets: []run.QuotaTarget{{Backend: "claude", Reason: "spent"}}}
	if err := commitSurveyRetry(root, e); err != nil {
		t.Fatal(err)
	}
	g := newHeartbeatGate(root)
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		current.Head = "new-head"
		current.State = state
		if got := g.Due(io.Discard); len(sweepIDs(got)) != 0 {
			t.Fatalf("PR bypassed hold: %+v", got)
		}
		if len(g.prSeen) != 0 || len(g.tickPRs) != 0 || len(g.pending) != 0 {
			t.Fatal("acknowledged held PR")
		}
	}
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if got := g.Due(io.Discard); len(sweepIDs(got)) == 0 || !strings.Contains(reasonFor(got, "moe"), "finished") {
		t.Fatalf("PR wake lost: %+v", got)
	}
}

func TestTypedSurveyWaitAndCancellationBeforeMint(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait and recover", true: "cancel"}[cancel], func(t *testing.T) {
			root := surveyFixture(t)
			oldGate, oldRead := newCooldownGate, readStageQuota
			t.Cleanup(func() { newCooldownGate, readStageQuota = oldGate, oldRead })
			now := time.Now()
			reads, waits := 0, 0
			readStageQuota = func(ctx context.Context, _, _ string, _ []string) (agent.Quota, error) {
				reads++
				assertNoSurveyRun(t, root, filepath.Join(root, "no-emit"))
				if cancel {
					process, err := os.FindProcess(os.Getpid())
					if err != nil {
						t.Fatal(err)
					}
					if err := process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
					<-ctx.Done()
					return agent.Quota{}, ctx.Err()
				}
				if reads == 1 {
					return quota(99, now.Add(time.Minute)), nil
				}
				return quota(1, time.Time{}), nil
			}
			newCooldownGate = func(resume string, w io.Writer) *cooldownGate {
				g := oldGate(resume, w)
				g.now = func() time.Time { return now }
				g.start = now
				g.after = func(d time.Duration) <-chan time.Time {
					waits++
					now = now.Add(d)
					ch := make(chan time.Time, 1)
					ch <- now
					return ch
				}
				return g
			}
			admission, code := admitPulseSurvey(root, io.Discard)
			if cancel {
				if code != exitInterrupted || admission != nil {
					t.Fatalf("cancel: %d %+v", code, admission)
				}
			} else if code != 0 || admission == nil || waits == 0 || reads != 2 {
				t.Fatalf("wait: code=%d waits=%d reads=%d", code, waits, reads)
			}
			assertNoSurveyRun(t, root, filepath.Join(root, "no-emit"))
		})
	}
}

func TestSurveySameEvidenceDoesNotCommitAndTemporaryExpiryReleases(t *testing.T) {
	root := surveyFixture(t)
	t.Setenv("MOE_FORCE_AGENT", "")
	now := time.Now()
	if _, err := agentdefault.Set(root, "codex", nil, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	defer withClockInvoked()()
	oldRead := readStageQuota
	t.Cleanup(func() { readStageQuota = oldRead })
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		return quota(99, now.Add(25*time.Hour)), nil
	}
	if _, code := admitPulseSurvey(root, io.Discard); code != exitQuotaDeferred {
		t.Fatal(code)
	}
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	if _, code := admitPulseSurvey(root, io.Discard); code != exitQuotaDeferred {
		t.Fatal(code)
	}
	if after := gittest.Output(t, root, "rev-parse", "HEAD"); after != before {
		t.Fatal("identical evidence committed")
	}
	if state := surveyAdmissionState(root, now.Add(23*time.Hour)); !state.Held(now.Add(23 * time.Hour)) {
		t.Fatal(state)
	}
	if state := surveyAdmissionState(root, now.Add(24*time.Hour)); state.Held(now.Add(24*time.Hour)) || state.Summary(now.Add(24*time.Hour)) != "" {
		t.Fatal("expired default still blocked new selection", state)
	}
}
