package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestMoeLog_RequiresThreePositionals(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for _, args := range [][]string{
		{"log"},
		{"log", "moe"},
		{"log", "moe", "demo-run"},
		{"log", "moe", "demo-run", "design", "extra"},
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
	code := Run([]string{"log", "moe", "missing-run", "design"}, &out, &errb)
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
	code := Run([]string{"log", "nope", "demo-run", "design"}, &out, &errb)
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
	code := Run([]string{"log", "moe", "demo-run", "design"}, &out, &errb)
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
	code := Run([]string{"log", "moe", "demo-run", "code"}, &out, &errb)
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
	code := Run([]string{"log", "moe", "demo-run", "code"}, &out, &errb)
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
	code := Run([]string{"log", "--agent", "claude", "moe", "demo-run", "design"}, &out, &errb)
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
	code := Run([]string{"log", "moe", "demo-run", "design"}, &out, &errb)
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
	code := Run([]string{"log", "--agent", "gpt", "moe", "demo-run", "design"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "--agent must be claude or codex") {
		t.Errorf("expected agent rejection, got %q", errb.String())
	}
}
