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

// writeThread drops a minimal claude thread file into the run/stage's
// documents dir, with one user message and one assistant reply so the
// renderer has something to print.
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

func TestMoeLog_NoRuns(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"log"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no run found") {
		t.Errorf("expected 'no run found' in stderr, got %q", errb.String())
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
	code := Run([]string{"log"}, &out, &errb)
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

func TestMoeLog_StageArgPicksThatStage(t *testing.T) {
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
	code := Run([]string{"log", "code"}, &out, &errb)
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

func TestMoeLog_UnknownStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"x"}}`,
	})

	var out, errb bytes.Buffer
	code := Run([]string{"log", "code"}, &out, &errb)
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
	code := Run([]string{"log", "--agent", "claude", "design"}, &out, &errb)
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

func TestPickLogThread_PrefersNewerMtime(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "moe", "demo-run", "sdlc", run.StatusInProgress)

	writeThread(t, root, "moe", "demo-run", "design", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"old"}}`,
	})
	// Backdate the older one so the design-stage claude is clearly
	// older than the code-stage claude written next.
	older := filepath.Join(root, run.ThreadPathFor("claude", "moe", "demo-run", "design"))
	ago := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, ago, ago); err != nil {
		t.Fatal(err)
	}
	writeThread(t, root, "moe", "demo-run", "code", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T11:00:00Z","message":{"role":"user","content":"newer"}}`,
	})

	md, err := pickLogRun(root, "", "")
	if err != nil {
		t.Fatalf("pickLogRun: %v", err)
	}
	if md == nil {
		t.Fatal("pickLogRun returned nil run")
	}
	path, agent, err := pickLogThread(root, md, "", "")
	if err != nil {
		t.Fatalf("pickLogThread: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join("code", "thread-claude.jsonl")) {
		t.Errorf("expected newer code-stage path, got %q", path)
	}
	if agent != "claude" {
		t.Errorf("expected agent=claude, got %q", agent)
	}
}
