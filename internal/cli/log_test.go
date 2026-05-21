package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// writeThread drops a minimal thread file into the run/stage's
// documents dir so the renderer has something to print.
func writeThread(t *testing.T, root, project, runID, stage, agent string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, run.DocDir(project, runID, stage))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "thread-"+agent+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLogRegisteredOnEveryWorkflow: every workflow whose runs land a
// thread-*.jsonl transcript grows a `log` subcommand on its group.
// Skipping a workflow drops the shared shape — this test is the
// tripwire, mirroring TestCatRegisteredOnEveryWorkflow.
func TestLogRegisteredOnEveryWorkflow(t *testing.T) {
	for _, wf := range []string{"idea", "sdlc", "kb", "meta-moe", "hooks", "twin"} {
		g, err := LookupGroup(wf)
		if err != nil {
			t.Fatalf("workflow %q not registered as a group: %v", wf, err)
		}
		if g.Lookup("log") == nil {
			t.Fatalf("workflow %q has no `log` subcommand registered", wf)
		}
	}
}

func TestMoeLog_RequiresStageForMultiStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for _, args := range [][]string{
		{"sdlc", "log"},
		{"sdlc", "log", "moe"},
		{"sdlc", "log", "moe", "demo-run", "design", "extra"},
	} {
		var out, errb bytes.Buffer
		code := Run(args, &out, &errb)
		if code != 2 {
			t.Errorf("args=%v: exit=%d, want 2; stderr=%q", args, code, errb.String())
		}
		if !strings.Contains(errb.String(), "usage:") {
			t.Errorf("args=%v: expected usage in stderr, got %q", args, errb.String())
		}
	}
}

func TestMoeLog_UnknownRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// project.json exists but the named run does not.
	trailerstest.SeedRun(t, root, "moe", "some-other-run", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "moe/missing-run", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Errorf("expected 'does not exist' in stderr, got %q", errb.String())
	}
}

func TestMoeLog_UnknownProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "nope/demo-run", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "not registered") {
		t.Errorf("expected 'not registered' in stderr, got %q", errb.String())
	}
}

func TestMoeLog_RendersClaudeThread(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","timestamp":"2026-05-16T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "moe/demo-run", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"user", "hello", "assistant", "hi back"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---output---\n%s", want, got)
		}
	}
}

func TestMoeLog_StagePicksThatStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"in design"}}`,
	})
	writeThread(t, root, "moe", "demo-run", "code", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"in code"}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "moe/demo-run", "code"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "in code") {
		t.Errorf("expected 'in code' from the code stage, got %q", got)
	}
	if strings.Contains(got, "in design") {
		t.Errorf("did not expect design content when 'code' stage was requested, got %q", got)
	}
}

func TestMoeLog_NoTranscriptForStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"x"}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "moe/demo-run", "code"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), `no transcript for stage "code"`) {
		t.Errorf("expected 'no transcript for stage \"code\"' in stderr, got %q", errb.String())
	}
}

func TestMoeLog_AgentFlagPins(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"claude-side"}}`,
	})
	writeThread(t, root, "moe", "demo-run", "design", "codex", []string{
		`{"timestamp":"2026-05-16T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex-side"}]}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "--agent", "claude", "moe/demo-run", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d; stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "claude-side") {
		t.Errorf("expected claude content, got %q", got)
	}
	if strings.Contains(got, "codex-side") {
		t.Errorf("did not expect codex content when --agent claude was pinned, got %q", got)
	}
}

func TestMoeLog_AmbiguousAgentRefuses(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"claude-side"}}`,
	})
	writeThread(t, root, "moe", "demo-run", "design", "codex", []string{
		`{"timestamp":"2026-05-16T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex-side"}]}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "moe/demo-run", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "both claude and codex transcripts") {
		t.Errorf("expected ambiguity refusal in stderr, got %q", errb.String())
	}
	if !strings.Contains(errb.String(), "--agent") {
		t.Errorf("expected --agent hint in stderr, got %q", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("expected no stdout on refusal, got %q", out.String())
	}
}

func TestMoeLog_BadAgentFlag(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "--agent", "gpt", "moe/demo-run", "design"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "--agent must be claude or codex") {
		t.Errorf("expected agent rejection, got %q", errb.String())
	}
}

// TestMoeLog_WrongWorkflow: pointing `moe <wf> log` at a run that
// belongs to another workflow refuses loudly and points at the right
// verb — mirrors TestCatWrongWorkflow.
func TestMoeLog_WrongWorkflow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "log", "tele/fix-it", "research"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on wrong-workflow, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "fix-it is a sdlc run, use 'moe sdlc log'") {
		t.Fatalf("expected wrong-workflow error pointing at sdlc, got: %q", errb.String())
	}
}

// TestMoeLog_UnknownStage: stage validation against the workflow's
// registered ladder; mirrors TestCatUnknownStage.
func TestMoeLog_UnknownStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "tele/fix-it", "bogus"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on unknown stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such stage: bogus") {
		t.Fatalf("expected unknown-stage error, got: %q", errb.String())
	}
}

// TestMoeLog_SingleStageDefaults: single-stage workflows accept the
// two-arg form and route to the only registered stage — mirrors
// TestCatSingleStageDefaultsStage.
func TestMoeLog_SingleStageDefaults(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "report-2026", "meta-moe", run.StatusInProgress)
	writeThread(t, root, "tele", "report-2026", "report", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"report body"}}`,
	})
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"meta-moe", "log", "tele/report-2026"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "report body") {
		t.Fatalf("expected report body, got %q", out.String())
	}
}

// TestMoeLog_LatestSentinel: `@latest` picks the run in (project,
// workflow) with the freshest journal activity — mirrors
// TestCatLatestSentinelResolvesMostRecent.
func TestMoeLog_LatestSentinel(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "older", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "newer", "sdlc", run.StatusInProgress)
	writeThread(t, root, "tele", "older", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"older transcript"}}`,
	})
	writeThread(t, root, "tele", "newer", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"newer transcript"}}`,
	})
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "older", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "newer", "sdlc", "design", t0.Add(time.Hour))
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "log", "tele/@latest", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "newer transcript") {
		t.Fatalf("expected @latest to resolve to newer, got: %q", out.String())
	}
	if strings.Contains(out.String(), "older transcript") {
		t.Fatalf("did not expect older content when @latest pinned newer, got: %q", out.String())
	}
}
