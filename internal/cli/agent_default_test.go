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
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/stylesheet"
	"github.com/modulecollective/moe/internal/ui"
)

func TestTemporarySelectionLadder(t *testing.T) {
	root := gittest.InitSeeded(t)
	now := time.Now().UTC()
	for _, backend := range []string{"claude", "codex"} {
		other := "claude"
		if backend == other {
			other = "codex"
		}
		state, err := agentdefault.Set(root, backend, nil, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		sheet, err := stylesheet.Parse([]byte("* {model-codex: codex-model; model-claude: claude-model;} sdlc.design {agent: " + other + ";}"))
		if err != nil {
			t.Fatal(err)
		}
		for _, rung := range []string{"temporary", "flag", "binding", "force", "expired"} {
			t.Run(backend+"/"+rung, func(t *testing.T) {
				t.Setenv("MOE_FORCE_AGENT", "")
				opts, md, at := stageSessionOpts{}, &run.Metadata{Workflow: "sdlc"}, now
				want := backend
				switch rung {
				case "flag":
					opts.Agent, want = other, other
				case "binding":
					md.Agent, want = other, other
				case "force":
					t.Setenv("MOE_FORCE_AGENT", other)
					opts.Agent, md.Agent, want = backend, backend, other
				case "expired":
					at, want = state.ExpiresAt, other
				}
				n, m, _, err := selectStageAgent(root, sheet, opts, md, "design", "", at)
				if err != nil || n != want || m != want+"-model" {
					t.Fatalf("selection %s %s %v", n, m, err)
				}
				_, m, _, err = selectStageAgent(root, sheet, opts, md, "design", "explicit", at)
				if err != nil || m != "explicit" {
					t.Fatalf("explicit model: %s %v", m, err)
				}
			})
		}
		ownMissing, _ := stylesheet.Parse([]byte("* {model-" + other + ": other-model;}"))
		n, m, _, err := selectStageAgent(root, ownMissing, stageSessionOpts{}, &run.Metadata{Workflow: "sdlc"}, "design", "", now)
		if err != nil || n != backend || m != "" {
			t.Fatalf("borrowed model: %s %s %v", n, m, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, agentdefault.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_FORCE_AGENT", "claude")
	if _, _, _, err := selectStageAgent(root, nil, stageSessionOpts{}, &run.Metadata{Workflow: "sdlc"}, "design", "", now); err == nil {
		t.Fatal("malformed state hidden by force")
	}
}

func TestTemporaryDefaultReachesSuccessiveStageDispatch(t *testing.T) {
	for _, headless := range []bool{false, true} {
		t.Run(map[bool]string{false: "interactive", true: "headless"}[headless], func(t *testing.T) {
			root, fake := setupResumeFixture(t, "unused-temporary")
			for _, name := range []string{"claude", "codex"} {
				original, err := agent.Get(name)
				if err != nil {
					t.Fatal(err)
				}
				agent.Unregister(name)
				agent.Register(name, fake)
				t.Cleanup(func() { agent.Unregister(name); agent.Register(name, original) })
			}
			now := time.Now().UTC()
			prev := agentDefaultNow
			agentDefaultNow = func() time.Time { return now }
			t.Cleanup(func() { agentDefaultNow = prev })
			factory := newCooldownGate
			t.Cleanup(func() { newCooldownGate = factory })
			newCooldownGate = func(resume string, w io.Writer) *cooldownGate {
				g := factory(resume, w)
				g.now = func() time.Time { return now }
				g.start = now
				return g
			}
			sheet := "* {model-claude: c1;model-codex: x1;} sdlc.design {agent: claude;}"
			gittest.WriteAndCommit(t, root, stylesheet.FileName, sheet, "initial sheet")
			set := func(name string) {
				t.Helper()
				if _, err := agentdefault.Set(root, name, nil, func() time.Time { return now }); err != nil {
					t.Fatal(err)
				}
			}
			turn := func(want string) {
				t.Helper()
				var out bytes.Buffer
				before := len(fake.execCalls)
				if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Headless: headless, SkipNextStage: true}, &out, &out); code != 0 {
					t.Fatalf("turn %d: %s", code, &out)
				}
				if len(fake.execCalls) != before+1 || fake.execCalls[before].model != want {
					t.Fatalf("dispatch: %+v", fake.execCalls)
				}
				md, err := run.Load(root, "tele", "fix-it")
				if err != nil || md.Agent != "" {
					t.Fatalf("temporary persisted: %+v %v", md, err)
				}
			}
			turn("c1")
			set("codex")
			turn("x1")
			gittest.WriteAndCommit(t, root, stylesheet.FileName, "* {model-claude:c2;model-codex:x2;} sdlc.design {agent:claude;}", "change current models")
			turn("x2")
			set("")
			turn("c2")
			set("codex")
			turn("x2")
			now = now.Add(agentdefault.Lifetime)
			turn("c2")
		})
	}
}

func TestAgentCLIAndUIWriterParity(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("MOE_FORCE_AGENT", "")
	var out bytes.Buffer
	if code := runAgent([]string{"codex"}, &out, &out); code != 0 || !strings.Contains(out.String(), "Default: Codex until") {
		t.Fatalf("CLI: %d %s", code, &out)
	}
	state, err := agentdefault.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	r := uiDo(context.Background(), root, ui.Action{Kind: ui.ActAgent, Agent: "claude", AgentRevision: 0})
	if r.OK || r.Uncertain || !strings.Contains(r.Message, "refresh") {
		t.Fatalf("stale UI: %+v", r)
	}
	r = uiDo(context.Background(), root, ui.Action{Kind: ui.ActAgent, Agent: "claude", AgentRevision: state.Revision})
	if !r.OK || !strings.Contains(r.Message, "Default: Claude until") {
		t.Fatalf("UI: %+v", r)
	}
	head := gittest.HeadSHA(t, root)
	out.Reset()
	if code := runAgent(nil, &out, &out); code != 0 || !strings.Contains(out.String(), "Default: Claude until") {
		t.Fatalf("status: %d %s", code, &out)
	}
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("status wrote")
	}
	src := newUISource(root, DashFilter{}, os.Stdout)
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, src)
	if !strings.Contains(strings.Join(b.Overview, "\n"), "Default: Claude until") || b.AgentRevision != 2 {
		t.Fatalf("overview: %+v", b)
	}
	if code := runAgent([]string{"reset"}, io.Discard, &out); code != 0 {
		t.Fatal(&out)
	}
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b = takeBoard(t, src)
	if b.AgentSummary != "Default: stylesheet" || b.AgentRevision != 3 {
		t.Fatalf("remote refresh: %+v", b)
	}
}

func TestTemporaryDefaultInterruptsOccupiedQuotaWait(t *testing.T) {
	for _, scenario := range []string{"switch", "reset", "expiry", "binding"} {
		t.Run(scenario, func(t *testing.T) {
			root, fake := setupResumeFixture(t, "unused-quota-default")
			for _, name := range []string{"claude", "codex"} {
				original, _ := agent.Get(name)
				agent.Unregister(name)
				agent.Register(name, fake)
				t.Cleanup(func() { agent.Unregister(name); agent.Register(name, original) })
			}
			now := time.Now().UTC()
			start := now
			prevNow := agentDefaultNow
			agentDefaultNow = func() time.Time { return now }
			t.Cleanup(func() { agentDefaultNow = prevNow })
			gittest.WriteAndCommit(t, root, stylesheet.FileName, "* {agent:claude; model-claude:c; model-codex:x;}", "initial selection")
			oldName, wantName := "claude", "codex"
			if scenario != "switch" {
				created := now
				if scenario == "expiry" {
					created = now.Add(-agentdefault.Lifetime + 30*time.Second)
				}
				if _, err := agentdefault.Set(root, "codex", nil, func() time.Time { return created }); err != nil {
					t.Fatal(err)
				}
				oldName, wantName = "codex", "claude"
			}
			if scenario == "binding" {
				md := loadRun(t, root, "tele", "fix-it")
				md.Agent = "codex"
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				gittest.Commit(t, root, "explicit binding")
				wantName = "codex"
			}
			oldFactory, oldRead := newCooldownGate, readStageQuota
			t.Cleanup(func() { newCooldownGate = oldFactory; readStageQuota = oldRead })
			var reads []string
			reset := now.Add(3 * time.Hour)
			readStageQuota = func(_ context.Context, name, model string, _ []string) (agent.Quota, error) {
				reads = append(reads, name+"/"+model)
				if name == oldName && now.Before(reset) {
					return quota(99, reset), nil
				}
				return quota(1, time.Time{}), nil
			}
			mutated := false
			newCooldownGate = func(resume string, w io.Writer) *cooldownGate {
				g := oldFactory(resume, w)
				g.start = start
				g.now = func() time.Time { return now }
				g.after = func(d time.Duration) <-chan time.Time {
					if d > time.Minute {
						t.Fatalf("selection refresh delayed %s", d)
					}
					if scenario == "expiry" && !mutated && d != 30*time.Second {
						t.Fatalf("missed expiry boundary: %s", d)
					}
					now = now.Add(d)
					if !mutated {
						mutated = true
						desired := "codex"
						if scenario == "reset" {
							desired = ""
						}
						if scenario == "binding" {
							desired = "claude"
						}
						if scenario != "expiry" {
							if _, err := agentdefault.Set(root, desired, nil, func() time.Time { return now }); err != nil {
								t.Fatal(err)
							}
						}
					}
					ch := make(chan time.Time, 1)
					ch <- now
					return ch
				}
				return g
			}
			var log bytes.Buffer
			if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Headless: true, SkipNextStage: true}, io.Discard, &log); code != 0 {
				t.Fatalf("code %d %s", code, &log)
			}
			if len(reads) != 2 || reads[0] != oldName+"/"+map[string]string{"claude": "c", "codex": "x"}[oldName] || reads[1] != wantName+"/"+map[string]string{"claude": "c", "codex": "x"}[wantName] {
				t.Fatalf("provider reads: %v", reads)
			}
			if len(fake.execCalls) != 1 || fake.execCalls[0].model != map[string]string{"claude": "c", "codex": "x"}[wantName] {
				t.Fatalf("dispatch: %+v", fake.execCalls)
			}
			if scenario != "binding" && now.Sub(start) > time.Minute {
				t.Fatalf("wait pinned old plan for %s", now.Sub(start))
			}
			if scenario == "binding" && now.Before(reset) {
				t.Fatal("masked change bypassed exhausted binding")
			}
		})
	}
}

func TestTemporaryDefaultChangesNextCascadeStageOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Chdir(root)
	md, err := run.New(root, "tele", run.Options{ID: "switch-cascade", Workflow: sdlcWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	gittest.WriteAndCommit(t, root, stylesheet.FileName, "* {agent:claude;model-claude:c;model-codex:x;}", "cascade policy")
	var calls []string
	for _, name := range []string{"claude", "codex"} {
		original, _ := agent.Get(name)
		a := &quotaTestAgent{exec: func(r agent.OneShotRequest) (string, error) {
			calls = append(calls, name+"/"+r.Model)
			if name == "claude" {
				// Change the canonical default while this admitted turn is executing.
				// Its request retains Claude's model; only the next stage changes.
				if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
					return "", err
				}
				return "", os.WriteFile(filepath.Join(r.Root, run.ContentPath("tele", md.ID, "design")), []byte("# Design\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\nSettled contract.\n"), 0o644)
			}
			writeStampedBuild(t, r.Root, md)
			return "", nil
		}}
		agent.Unregister(name)
		agent.Register(name, a)
		t.Cleanup(func() { agent.Unregister(name); agent.Register(name, original) })
	}
	var log bytes.Buffer
	res, code := cascadeFromGate("design", "review", false, md, io.Discard, &log)
	if code != 0 || len(res.ran) != 2 || strings.Join(calls, ",") != "claude/c,codex/x" {
		t.Fatalf("cascade %d %+v calls=%v\n%s", code, res, calls, &log)
	}
	fresh, err := run.Load(root, "tele", md.ID)
	if err != nil || fresh.Agent != "" {
		t.Fatalf("cascade persisted default: %+v %v", fresh, err)
	}
}

func TestTemporaryDefaultPollPreservesRecoveredSessionSelection(t *testing.T) {
	root, fake := setupResumeFixture(t, "unused-recovered")
	for _, name := range []string{"claude", "codex"} {
		original, _ := agent.Get(name)
		agent.Unregister(name)
		agent.Register(name, fake)
		t.Cleanup(func() { agent.Unregister(name); agent.Register(name, original) })
	}
	gittest.WriteAndCommit(t, root, stylesheet.FileName, "* {agent:claude;model-claude:c;model-codex:x;}", "canonical policy")
	sess, _, cancel, err := openStageSession(root, stageTurnInputs{Project: "tele", RunSlug: "fix-it", DocID: "design", LockPurpose: "test"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	gittest.WriteAndCommit(t, sess.WorktreePath, stylesheet.FileName, "* {agent:codex;model-codex:recovered;}", "retained session policy")
	if err := cancel(); err == nil {
		t.Fatal("changed reservation discarded")
	}
	writeDeadClaim(t, sess, false)
	previous := readStageQuota
	t.Cleanup(func() { readStageQuota = previous })
	reads := 0
	readStageQuota = func(_ context.Context, name, model string, _ []string) (agent.Quota, error) {
		reads++
		if reads > 2 {
			return agent.Quota{}, agent.ErrQuotaMalformed
		}
		if name != "codex" || model != "recovered" {
			t.Fatalf("lost session policy: %s %s", name, model)
		}
		return quota(1, time.Time{}), nil
	}
	var log bytes.Buffer
	if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Headless: true, SkipNextStage: true}, io.Discard, &log); code != 0 {
		t.Fatalf("code %d %s", code, &log)
	}
	if reads != 1 || len(fake.execCalls) != 1 || fake.execCalls[0].model != "recovered" {
		t.Fatalf("reads=%d dispatch=%+v", reads, fake.execCalls)
	}
}
