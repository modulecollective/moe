package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func testCooldown(t *testing.T) (*cooldownGate, *time.Time, *bytes.Buffer) {
	t.Helper()
	cooldownSkipped.Store(false)
	t.Cleanup(func() { cooldownSkipped.Store(false) })
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	g := newCooldownGate("moe sdlc code tele/r", &out)
	g.now = func() time.Time { return now }
	g.start = now
	g.after = func(d time.Duration) <-chan time.Time {
		now = now.Add(d)
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	g.openControls = func() (<-chan cooldownControl, func()) { return nil, func() {} }
	return g, &now, &out
}

func quota(percent float64, reset time.Time) agent.Quota {
	return agent.Quota{Windows: []agent.QuotaWindow{{ID: "five_hour", UsedPercent: percent, ResetsAt: reset}}}
}

func TestCooldownThresholdAndRefresh(t *testing.T) {
	for _, percent := range []float64{94, 95} {
		t.Run(fmt.Sprint(percent), func(t *testing.T) {
			g, now, _ := testCooldown(t)
			reads := 0
			waits := 0
			g.read = func(context.Context, string, string) (agent.Quota, error) {
				reads++
				if reads == 1 {
					return quota(percent, now.Add(time.Hour)), nil
				}
				return quota(10, time.Time{}), nil
			}
			after := g.after
			g.after = func(d time.Duration) <-chan time.Time {
				waits++
				if d != time.Hour+5*time.Second {
					t.Errorf("wait %v", d)
				}
				return after(d)
			}
			got, waited := g.check(context.Background(), "claude", "opus")
			if got != cooldownAdmit || waited != (percent == 95) || waits != int(percent-94) || reads != 1+waits {
				t.Fatalf("outcome %v waited %v reads %d waits %d", got, waited, reads, waits)
			}
		})
	}
}

func TestCooldownWaitsForAllWindows(t *testing.T) {
	g, now, _ := testCooldown(t)
	reads := 0
	g.read = func(context.Context, string, string) (agent.Quota, error) {
		reads++
		if reads > 1 {
			return quota(1, time.Time{}), nil
		}
		q := quota(96, now.Add(time.Hour))
		q.Windows = append(q.Windows, agent.QuotaWindow{ID: "weekly", UsedPercent: 99, ResetsAt: now.Add(2 * time.Hour)})
		return q, nil
	}
	after := g.after
	g.after = func(d time.Duration) <-chan time.Time {
		if d != 2*time.Hour+5*time.Second {
			t.Fatalf("wait %v", d)
		}
		return after(d)
	}
	if got, _ := g.check(context.Background(), "claude", ""); got != cooldownAdmit {
		t.Fatal(got)
	}
}

func TestCooldownCapAndCumulativeBudget(t *testing.T) {
	for _, mode := range []string{"far reset", "no reset", "past reset", "denied", "transient after waiting"} {
		t.Run(mode, func(t *testing.T) {
			g, now, out := testCooldown(t)
			reads := 0
			g.read = func(context.Context, string, string) (agent.Quota, error) {
				reads++
				switch mode {
				case "far reset":
					return quota(95, now.Add(7*time.Hour)), nil
				case "no reset":
					return quota(95, time.Time{}), nil
				case "past reset":
					return quota(95, now.Add(-time.Hour)), nil
				case "denied":
					q := quota(1, time.Time{})
					q.Denied = true
					q.Reason = "reached"
					return q, nil
				default:
					if reads == 1 {
						return quota(96, now.Add(cooldownBudget-35*time.Second)), nil
					}
					return agent.Quota{}, errors.New("offline")
				}
			}
			got, _ := g.check(context.Background(), "claude", "")
			if got != cooldownStop || now.Sub(g.start) > cooldownBudget || !strings.Contains(out.String(), "--no-cooldown") {
				t.Fatalf("got %v elapsed %v output %s", got, now.Sub(g.start), out)
			}
			if mode == "far reset" && reads != 1 {
				t.Fatalf("reads %d", reads)
			}
			if (mode == "no reset" || mode == "past reset" || mode == "denied") && reads != 360 {
				t.Fatalf("reads %d, want 360 one-minute polls", reads)
			}
		})
	}
}

func TestCooldownReadFailuresAreBounded(t *testing.T) {
	for _, permanent := range []bool{true, false} {
		t.Run(fmt.Sprint(permanent), func(t *testing.T) {
			g, _, out := testCooldown(t)
			reads := 0
			g.read = func(context.Context, string, string) (agent.Quota, error) {
				reads++
				if permanent {
					return agent.Quota{}, agent.ErrQuotaMalformed
				}
				return agent.Quota{}, errors.New("offline")
			}
			got, _ := g.check(context.Background(), "codex", "")
			want := 3
			if permanent {
				want = 1
			}
			if got != cooldownStop || reads != want || !strings.Contains(out.String(), "not started: quota") {
				t.Fatalf("got %v reads %d output %s", got, reads, out)
			}
		})
	}
}

func TestCooldownOverridesAndCancellationJoinReadAndReader(t *testing.T) {
	for _, ctl := range []cooldownControl{controlContinue, controlSkipAll, controlCancel} {
		t.Run(fmt.Sprint(ctl), func(t *testing.T) {
			g, _, _ := testCooldown(t)
			controls := make(chan cooldownControl, 1)
			stopped := false
			readStopped := false
			reads := 0
			g.openControls = func() (<-chan cooldownControl, func()) { return controls, func() { stopped = true } }
			g.read = func(ctx context.Context, _, _ string) (agent.Quota, error) {
				reads++
				controls <- ctl
				<-ctx.Done()
				readStopped = true
				return agent.Quota{}, ctx.Err()
			}
			got, _ := g.check(context.Background(), "claude", "")
			want := cooldownAdmit
			if ctl == controlCancel {
				want = cooldownCancel
			}
			if got != want || !stopped || !readStopped || cooldownSkipped.Load() != (ctl == controlSkipAll) {
				t.Fatalf("got %v reader %v child %v skipped %v", got, stopped, readStopped, cooldownSkipped.Load())
			}
			// A new stage checks again after c, while a skips all later reads.
			next, _, _ := testCooldown(t)
			cooldownSkipped.Store(ctl == controlSkipAll)
			next.read = func(context.Context, string, string) (agent.Quota, error) { reads++; return quota(1, time.Time{}), nil }
			if got, _ := next.check(context.Background(), "claude", ""); got != cooldownAdmit {
				t.Fatal(got)
			}
			wantReads := 2
			if ctl == controlSkipAll {
				wantReads = 1
			}
			if reads != wantReads {
				t.Fatalf("reads %d", reads)
			}
		})
	}
}

func TestCooldownContextCancellationDuringWait(t *testing.T) {
	g, _, _ := testCooldown(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.read = func(context.Context, string, string) (agent.Quota, error) { return quota(99, time.Time{}), nil }
	g.after = func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) }
	if got, _ := g.check(ctx, "claude", ""); got != cooldownCancel {
		t.Fatal(got)
	}
}

func TestStageCooldownRechecksChangedSelection(t *testing.T) {
	g, _, _ := testCooldown(t)
	reads := []string{}
	refreshes := 0
	g.read = func(_ context.Context, name, model string) (agent.Quota, error) {
		reads = append(reads, name+"/"+model)
		if len(reads) == 1 {
			return quota(99, time.Time{}), nil
		}
		return quota(1, time.Time{}), nil
	}
	name, model := "claude", "opus"
	code := stageCooldown(g, &name, &model, func(*run.Metadata) (string, string, error) { return "codex", "gpt", nil }, func() (*run.Metadata, error) { refreshes++; return &run.Metadata{}, nil }, io.Discard)
	if code != 0 || refreshes != 1 || strings.Join(reads, ",") != "claude/opus,claude/opus,codex/gpt" {
		t.Fatalf("code %d refreshes %d reads %v", code, refreshes, reads)
	}
}

func TestNoCooldownFlagIsInvocationScoped(t *testing.T) {
	const cmd = "__test_cooldown"
	Register(&Command{Name: cmd, Run: func(args []string, _, _ io.Writer) int {
		if len(args) != 1 || args[0] != "arg" {
			t.Fatalf("args %v", args)
		}
		if cooldownSkipped.Load() {
			return 7
		}
		return 9
	}})
	defer delete(commands, cmd)
	if got := Run([]string{cmd, "--no-cooldown", "arg"}, io.Discard, io.Discard); got != 7 {
		t.Fatal(got)
	}
	if got := Run([]string{cmd, "arg"}, io.Discard, io.Discard); got != 9 {
		t.Fatal(got)
	}
}

func TestCooldownReservationStopsBeforeCanvasMutations(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_SERVE_AGENT", "1")
	t.Chdir(root)
	md, err := run.New(root, "tele", run.Options{ID: "quota", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	original := readStageQuota
	t.Cleanup(func() { readStageQuota = original })
	reads := 0
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		reads++
		claims, err := session.List(root)
		if err != nil || len(claims) != 1 {
			t.Fatalf("sessions %v err %v", claims, err)
		}
		if _, ok := session.ReadClaim(root, "tele", md.ID, "design"); !ok {
			t.Error("no occupancy claim")
		}
		if err := repolock.With(root, repolock.Options{Purpose: "probe", Budget: 100 * time.Millisecond}, func() error { return nil }); err != nil {
			t.Fatalf("quota read holds repo lock: %v", err)
		}
		return quota(99, time.Now().Add(7*time.Hour)), nil
	}
	var out bytes.Buffer
	started := false
	code := runStageSession("tele", md.ID, "design", stageSessionOpts{Headless: true, OnAgentStart: func() { started = true }}, io.Discard, &out)
	after, _ := git.HEAD(root)
	sessions, _ := session.List(root)
	if code != exitNotStarted || started || reads != 1 || before != after || len(sessions) != 0 {
		t.Fatalf("code %d started %v reads %d head %s/%s sessions %d output %s", code, started, reads, before, after, len(sessions), &out)
	}
	if _, ok := session.ReadClaim(root, "tele", md.ID, "design"); ok {
		t.Error("leaked claim")
	}
	if _, err := os.Stat(filepath.Join(root, run.ContentPath("tele", md.ID, "design"))); !os.IsNotExist(err) {
		t.Fatalf("canvas mutated before admission: %v", err)
	}
	if strings.Contains(out.String(), "unchanged from main") {
		t.Fatalf("canvas close gate ran: %s", &out)
	}
}

func TestCooldownCancellationPreservesResumedDirtySession(t *testing.T) {
	root := newTestBureaucracy(t)
	in := stageTurnInputs{Project: "tele", RunSlug: "r", DocID: "code", LockPurpose: "test"}
	sess, _, cancel, err := openStageSession(root, in, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Release the initial claim, retaining work as a resumed branch would.
	rel := "saved.txt"
	if err := os.WriteFile(filepath.Join(sess.WorktreePath, rel), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cancel(); err == nil {
		t.Fatal("changed fresh reservation was deleted")
	}
	in.Admit = func(*session.Session) int { return exitInterrupted }
	in.BuildSpec = func(string) (stageTurnSpec, error) {
		t.Fatal("BuildSpec ran before admission")
		return stageTurnSpec{}, nil
	}
	if code := runStageTurn(root, in, io.Discard, io.Discard); code != exitInterrupted {
		t.Fatal(code)
	}
	body, err := os.ReadFile(filepath.Join(sess.WorktreePath, rel))
	if err != nil || string(body) != "saved" {
		t.Fatalf("saved work %q err %v", body, err)
	}
	if _, ok := session.ReadClaim(root, "tele", "r", "code"); ok {
		t.Error("claim retained after cancellation")
	}
}

func TestCooldownRefreshesSelectionAndPromptFromMain(t *testing.T) {
	root, fake := setupResumeFixture(t, "quota-before")
	agent.Register("quota-after", fake)
	t.Cleanup(func() { agent.Unregister("quota-after") })
	fake.canvasRel = run.ContentPath("tele", "fix-it", "code")
	t.Chdir(root)
	oldSheet := "* { agent: quota-before; model: old-model; }\n"
	gittest.WriteAndCommit(t, root, "model-stylesheet.css", oldSheet, "initial selection")
	originalFactory := newCooldownGate
	originalRead := readStageQuota
	t.Cleanup(func() { newCooldownGate = originalFactory; readStageQuota = originalRead })
	now := time.Now()
	reads := []string{}
	slept := false
	readStageQuota = func(_ context.Context, name, model string, _ []string) (agent.Quota, error) {
		reads = append(reads, name+"/"+model)
		if !slept {
			return quota(99, now.Add(time.Minute)), nil
		}
		return quota(1, time.Time{}), nil
	}
	newCooldownGate = func(resume string, w io.Writer) *cooldownGate {
		g := originalFactory(resume, w)
		g.start = now
		g.now = func() time.Time { return now }
		g.after = func(d time.Duration) <-chan time.Time {
			slept = true
			now = now.Add(d)
			// Move canonical main while the reserved session waits.
			gittest.WriteAndCommit(t, root, "model-stylesheet.css", "* { agent: quota-after; model: new-model; }\n", "operator changes backend")
			gittest.WriteAndCommit(t, root, run.ContentPath("tele", "fix-it", "design"), "# Design\n\nFresh contract after quota wait.\n", "operator updates design")
			if _, err := input.Add(root, "tele", "fix-it", "Fresh contract after quota wait."); err != nil {
				t.Fatal(err)
			}
			ch := make(chan time.Time, 1)
			ch <- now
			return ch
		}
		return g
	}
	var out bytes.Buffer
	code := runStageSession("tele", "fix-it", "code", stageSessionOpts{Headless: true}, io.Discard, &out)
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, &out)
	}
	if strings.Join(reads, ",") != "quota-before/old-model,quota-before/old-model,quota-after/new-model" {
		t.Fatalf("reads %v", reads)
	}
	if len(fake.execCalls) != 1 || fake.execCalls[0].model != "new-model" {
		t.Fatalf("dispatches %+v", fake.execCalls)
	}
	prompt, err := os.ReadFile(filepath.Join(root, run.Dir("tele", "fix-it"), "documents/code/prompt-quota-after.md"))
	if err != nil || !strings.Contains(string(prompt), "Fresh contract after quota wait.") {
		t.Fatalf("stale prompt (read err %v)", err)
	}
}
