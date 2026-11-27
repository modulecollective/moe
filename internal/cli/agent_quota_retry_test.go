package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

func TestChangedDefaultWakesDeferredSurveyAcrossRestart(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_SERVE_AGENT", "1")
	t.Chdir(root)
	defer withClockInvoked()()
	defer withRideMode(rideDynamic)()
	prev := readStageQuota
	t.Cleanup(func() { readStageQuota = prev })
	var reads []string
	readStageQuota = func(_ context.Context, name, model string, _ []string) (agent.Quota, error) {
		reads = append(reads, name)
		return quota(99, time.Now().Add(48*time.Hour)), nil
	}
	var log bytes.Buffer
	if code, _ := runPulse(root, "", io.Discard, &log); code != exitQuotaDeferred {
		t.Fatalf("first %d %s", code, &log)
	}
	p, err := project.Load(root, project.RootID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		g := newHeartbeatGate(root)
		sc := mustPulseScan(t, root)
		g.Swept(project.RootID, true)
		due, _, why := g.projectDue(sc, p, time.Now())
		if due != want {
			t.Fatalf("due %v want %v: %s", due, want, why)
		}
	}
	check(false)
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The ordinary guards still bind a changed selection.
	sc := mustPulseScan(t, root)
	g := newHeartbeatGate(root)
	p.Mode = "paused"
	if due, _, _ := g.projectDue(sc, p, time.Now()); due {
		t.Fatal("ignored pause")
	}
	p.Mode = ""
	if code, _ := runPulse(root, "", io.Discard, &log); code != exitQuotaDeferred {
		t.Fatalf("replacement %d %s", code, &log)
	}
	if strings.Join(reads, ",") != "claude,codex" {
		t.Fatalf("reads: %v", reads)
	}
	check(false)
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.QuotaRetries) != 1 {
		t.Fatalf("replacement evidence: %+v", idx.QuotaRetries)
	}
	for _, r := range idx.QuotaRetries {
		if r.Targets[0].Backend != "codex" {
			t.Fatal(r)
		}
	}
	// Unavailable configuration is unknown and leaves the obligation untouched.
	if err := os.WriteFile(filepath.Join(root, agentdefault.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestDeferredRideSelectionUsesCurrentBindingAndStage(t *testing.T) {
	root := quietFixture(t)
	t.Setenv("MOE_FORCE_AGENT", "")
	md, err := run.New(root, "moe", run.Options{ID: "quota-ride", Workflow: sdlcWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	target := run.QuotaTarget{Project: "moe", Run: md.ID, Stage: "design", Backend: "claude", Reason: "spent"}
	idx.QuotaRetries = map[string]run.QuotaRetry{"_cabinet/pulse": {Project: project.RootID, Targets: []run.QuotaTarget{target}}}
	sc := &pulseScan{root: root, idx: idx, byKey: map[string]*run.Metadata{"moe/" + md.ID: md}}
	check := func(want bool) {
		t.Helper()
		got, err := quotaSelectionChanged(root, sc, project.RootID, time.Now())
		if err != nil || got != want {
			t.Fatalf("changed=%v want=%v err=%v", got, want, err)
		}
	}
	check(false)
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	check(true)
	md.Agent = "claude"
	check(false)
	md.Agent = ""
	md.Status = run.StatusClosed
	check(false)
	md.Status = run.StatusPushed
	check(false)
	md.Status = run.StatusInProgress
	target.Stage = "build"
	idx.QuotaRetries["_cabinet/pulse"] = run.QuotaRetry{Project: project.RootID, Targets: []run.QuotaTarget{target}}
	check(false)
	// A closed survey's historical binding must not pin its successor.
	md.Workflow = pulseWorkflow
	md.Status = run.StatusClosed
	md.Agent = "claude"
	target.Stage = pulseDoc
	idx.QuotaRetries["_cabinet/pulse"] = run.QuotaRetry{Project: project.RootID, Targets: []run.QuotaTarget{target}}
	check(true)
	gittest.WriteAndCommit(t, root, "model-stylesheet.css", "bad sheet", "malformed sheet")
	if got, err := quotaSelectionChanged(root, sc, project.RootID, time.Now()); err == nil || got {
		t.Fatalf("bad config woke: %v %v", got, err)
	}
}

func TestChangedDefaultWakesJournaledRideRetryAcrossRestart(t *testing.T) {
	root := quietFixture(t)
	t.Setenv("MOE_FORCE_AGENT", "")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	ride, err := run.New(root, "moe", run.Options{ID: "deferred-ride", Workflow: sdlcWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	pulse, err := run.New(root, project.RootID, run.Options{ID: "ride-survey", Workflow: pulseWorkflow, SeedDocs: map[string]string{pulseDoc: pulseCanvasSkeleton}})
	if err != nil {
		t.Fatal(err)
	}
	if err := disposePulseRunReason(root, project.RootID, pulse.ID, "test setup", io.Discard); err != nil {
		t.Fatal(err)
	}
	q, err := newPulseQuotaRetries(root, project.RootID)
	if err != nil {
		t.Fatal(err)
	}
	q.pulse = pulse.ID
	if err := q.record(quotaDeferral{target: run.QuotaTarget{Project: "moe", Run: ride.ID, Stage: "design", Backend: "claude", Reason: "spent"}, retryAt: time.Now().Add(48 * time.Hour)}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); subj != "Schedule quota retry for "+project.RootID+"/"+pulse.ID {
		t.Fatalf("quota retry headline = %q", subj)
	}
	p, err := project.Load(root, project.RootID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		g := newHeartbeatGate(root)
		due, _, why := g.projectDue(mustPulseScan(t, root), p, time.Now())
		if due != want {
			t.Fatalf("restart opportunity=%v want=%v: %s", due, want, why)
		}
	}
	check(false)
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	check(true)
	ride.Agent = "claude"
	if err := run.Save(root, ride); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "bind deferred ride")
	check(false)
	ride.Agent = ""
	if err := run.Save(root, ride); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "unbind deferred ride")
	check(true)
	t.Setenv("MOE_FORCE_AGENT", "claude")
	check(false)
	t.Setenv("MOE_FORCE_AGENT", "")
	if _, err := agentdefault.Set(root, "", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	check(false)
	idx, err := run.BuildJournalIndex(root)
	if err != nil || len(idx.QuotaRetries) != 1 {
		t.Fatalf("gate consumed obligation: %v %v", idx, err)
	}
}
