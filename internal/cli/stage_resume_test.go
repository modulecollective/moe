package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// fakeExecCall records one dispatch into the fake agent's executor
// methods. cwd is the value stage.go threaded into the request as the
// stable per-document working directory; sessionID / newSession are the
// resume-vs-mint decision the two-turn wiring produces.
type fakeExecCall struct {
	method     string // "Execute" or "ExecuteOneShot"
	sessionID  string
	newSession bool
	cwd        string
}

// fakeProbe records one TranscriptExists / RestoreTranscript pre-flight.
type fakeProbe struct {
	sessionID string
	cwd       string
}

// fakeAgent is a test double for agent.Agent. It records the executor
// dispatches and the resume pre-flight probes so a two-turn drive can
// assert the wiring around sessionDocCwd: that turn 2 pre-flights
// TranscriptExists against the same cwd turn 1 ran under, and that the
// dispatch takes the resume branch rather than re-minting. It writes the
// canvas on every turn so session.Close's canvas-unchanged guard passes
// (a no-op turn would refuse to close and never land run.json on main,
// so turn 2 would never see the committed session). The fixed agent
// stubs the five-method Agent interface — no claude/codex subprocess.
type fakeAgent struct {
	// canvasRel is the canvas path relative to the (worktree) root the
	// executor methods receive. Written each turn with unique content so
	// the canvas genuinely moves and Close's guard is satisfied.
	canvasRel string
	// transcriptFound is what TranscriptExists reports. The two-turn
	// resume tests want the hit branch (resume), so they set it true.
	transcriptFound bool

	writes int // bumps the canvas body each turn so it differs from main

	execCalls []fakeExecCall
	probes    []fakeProbe
	restores  []fakeProbe
}

func (f *fakeAgent) writeCanvas(root string) error {
	f.writes++
	p := filepath.Join(root, f.canvasRel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, fmt.Appendf(nil, "# fake turn %d\n", f.writes), 0o644)
}

func (f *fakeAgent) Execute(r agent.Request) (string, error) {
	f.execCalls = append(f.execCalls, fakeExecCall{
		method:     "Execute",
		sessionID:  r.SessionID,
		newSession: r.NewSession,
		cwd:        r.SessionCwd,
	})
	if err := f.writeCanvas(r.Root); err != nil {
		return "", err
	}
	// Interactive claude echoes the session id it was handed.
	return r.SessionID, nil
}

func (f *fakeAgent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	f.execCalls = append(f.execCalls, fakeExecCall{
		method: "ExecuteOneShot",
		cwd:    r.SessionCwd,
	})
	if err := f.writeCanvas(r.Root); err != nil {
		return "", err
	}
	// OneShotRequest carries no session id (claude headless mints its
	// own and stage.go reads it back). Returning "" exercises the
	// "no re-mint reported" branch, so the session keeps the UUID
	// commitSessionStart already committed — which is what turn 2
	// resumes against. The id-rediscovery path has its own coverage in
	// the claude executor package; this harness pins the cwd-and-resume
	// wiring, not id minting.
	return "", nil
}

func (f *fakeAgent) CopyTranscript(sessionID, dest string) (bool, error) {
	return false, nil
}

func (f *fakeAgent) TranscriptExists(sessionID, cwd string) (bool, error) {
	f.probes = append(f.probes, fakeProbe{sessionID: sessionID, cwd: cwd})
	return f.transcriptFound, nil
}

func (f *fakeAgent) RestoreTranscript(sessionID, cwd, mirrorPath string) (agent.RestoreOutcome, error) {
	f.restores = append(f.restores, fakeProbe{sessionID: sessionID, cwd: cwd})
	return agent.RestoreOutcome{Result: agent.RestoreMissing}, nil
}

// setupResumeFixture builds a bureaucracy with a seeded sdlc run and
// registers a fakeAgent under name. Returns the root and the fake so the
// caller can drive turns and inspect the recorded calls.
func setupResumeFixture(t *testing.T, name string) (string, *fakeAgent) {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "") // don't let a stray env override opts.Agent
	t.Setenv("NO_COLOR", "1")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	fake := &fakeAgent{
		canvasRel:       run.ContentPath("tele", "fix-it", "design"),
		transcriptFound: true,
	}
	agent.Register(name, fake)
	return root, fake
}

// driveDesignTurn runs one design-stage turn end-to-end against the
// fake agent. SkipNextStage suppresses the interactive post-turn prompt
// (orthogonal to the resume wiring under test) so the turn never blocks
// on stdin; headless turns skip it structurally regardless.
func driveDesignTurn(t *testing.T, fakeName string, headless bool) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runStageSession("tele", "fix-it", "design", stageSessionOpts{
		Agent:         fakeName,
		Headless:      headless,
		SkipNextStage: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("design turn exited %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

// TestTwoTurnInteractiveResumePreflightsSameCwd is the headline
// regression for this run: drive two interactive design turns against a
// fake agent and assert the resume wiring around sessionDocCwd holds
// end-to-end. Turn 1 is a fresh session (mint, no pre-flight). Turn 2
// must pre-flight TranscriptExists against the *same* cwd turn 1
// dispatched under, find the transcript, and resume the same session id
// rather than re-mint. The cwd-stability invariant is unit-pinned by
// TestSessionDocCwdIsStableAcrossTurns; this test pins that BuildSpec /
// dispatch actually thread that stable cwd through a real two-turn
// sequence — the wiring that churned in the reverted "re-entry turn"
// commit and the recent stable-cwd fix.
func TestTwoTurnInteractiveResumePreflightsSameCwd(t *testing.T) {
	root, fake := setupResumeFixture(t, "fake-two-turn-resume")

	driveDesignTurn(t, "fake-two-turn-resume", false)
	driveDesignTurn(t, "fake-two-turn-resume", false)

	if len(fake.execCalls) != 2 {
		t.Fatalf("want 2 executor dispatches, got %d: %+v", len(fake.execCalls), fake.execCalls)
	}
	t1, t2 := fake.execCalls[0], fake.execCalls[1]

	if t1.method != "Execute" || t2.method != "Execute" {
		t.Fatalf("want both turns interactive Execute, got %q then %q", t1.method, t2.method)
	}
	if !t1.newSession {
		t.Errorf("turn 1 should be a fresh session (NewSession=true)")
	}
	if t2.newSession {
		t.Errorf("turn 2 should resume (NewSession=false), not re-mint")
	}

	wantCwd := sessionDocCwd(root, "tele", "fix-it", "design")
	if t1.cwd != wantCwd {
		t.Errorf("turn 1 dispatch cwd = %q, want %q", t1.cwd, wantCwd)
	}
	if t2.cwd != t1.cwd {
		t.Errorf("dispatch cwd churned across turns: turn1=%q turn2=%q", t1.cwd, t2.cwd)
	}
	if t2.sessionID == "" || t2.sessionID != t1.sessionID {
		t.Errorf("turn 2 resumed session %q, want same as turn 1 %q", t2.sessionID, t1.sessionID)
	}

	// The pre-flight fires only on the resume turn, against the same
	// (session, cwd) the dispatch uses.
	if len(fake.probes) != 1 {
		t.Fatalf("want exactly 1 TranscriptExists pre-flight (turn 2 only), got %d: %+v", len(fake.probes), fake.probes)
	}
	if fake.probes[0].cwd != wantCwd {
		t.Errorf("pre-flight cwd = %q, want %q (the cwd turn 1 wrote under)", fake.probes[0].cwd, wantCwd)
	}
	if fake.probes[0].sessionID != t1.sessionID {
		t.Errorf("pre-flight probed session %q, want turn-1 session %q", fake.probes[0].sessionID, t1.sessionID)
	}
	if len(fake.restores) != 0 {
		t.Errorf("transcript was found; RestoreTranscript should not have been called: %+v", fake.restores)
	}
}

// TestHeadlessThenInteractiveResumesSameCwd pins the headless →
// interactive transition: a cascade-driven headless turn 1 (dispatched
// via ExecuteOneShot) followed by an operator-driven interactive turn 2
// (Execute) must land in the same encoded-cwd bucket and resume the
// session committed at turn 1 rather than re-mint. Both executor entry
// points thread SessionCwd identically; this is the test that proves it
// across the mode switch.
func TestHeadlessThenInteractiveResumesSameCwd(t *testing.T) {
	root, fake := setupResumeFixture(t, "fake-headless-to-interactive")

	driveDesignTurn(t, "fake-headless-to-interactive", true)  // headless
	driveDesignTurn(t, "fake-headless-to-interactive", false) // interactive

	if len(fake.execCalls) != 2 {
		t.Fatalf("want 2 executor dispatches, got %d: %+v", len(fake.execCalls), fake.execCalls)
	}
	t1, t2 := fake.execCalls[0], fake.execCalls[1]

	if t1.method != "ExecuteOneShot" {
		t.Errorf("turn 1 should dispatch headless via ExecuteOneShot, got %q", t1.method)
	}
	if t2.method != "Execute" {
		t.Errorf("turn 2 should dispatch interactive via Execute, got %q", t2.method)
	}

	wantCwd := sessionDocCwd(root, "tele", "fix-it", "design")
	if t1.cwd != wantCwd {
		t.Errorf("headless turn cwd = %q, want %q", t1.cwd, wantCwd)
	}
	if t2.cwd != t1.cwd {
		t.Errorf("cwd churned across the headless→interactive switch: headless=%q interactive=%q", t1.cwd, t2.cwd)
	}
	if t2.newSession {
		t.Errorf("interactive turn 2 should resume (NewSession=false), not re-mint")
	}

	// Turn 2 must resume the session that landed on main after turn 1.
	md, err := run.Load(root, "tele", "fix-it")
	if err != nil {
		t.Fatalf("reload run after headless turn: %v", err)
	}
	committed := md.Documents["design"].Session
	if t2.sessionID != committed {
		t.Errorf("turn 2 resumed %q, want the session committed after turn 1 %q", t2.sessionID, committed)
	}

	if len(fake.probes) != 1 {
		t.Fatalf("want exactly 1 TranscriptExists pre-flight (turn 2 only), got %d: %+v", len(fake.probes), fake.probes)
	}
	if fake.probes[0].cwd != wantCwd {
		t.Errorf("pre-flight cwd = %q, want %q", fake.probes[0].cwd, wantCwd)
	}
}
