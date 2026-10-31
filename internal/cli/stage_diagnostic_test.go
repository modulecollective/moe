package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

type emptyCanvasAgent struct {
	fakeAgent
	empty        bool
	succeed      bool
	noTranscript bool
	transcript   []byte
}

func (f *emptyCanvasAgent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	f.writes++
	if f.succeed {
		if err := f.writeCanvas(r.Root); err != nil {
			return "", err
		}
		f.transcript = []byte(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"completed implementation"}]}}` + "\n")
		return "11111111-1111-4111-8111-111111111111", os.WriteFile(r.ThreadPath, f.transcript, 0o644)
	}
	if f.empty {
		if err := os.WriteFile(filepath.Join(r.Root, f.canvasRel), nil, 0o644); err != nil {
			return "", err
		}
	}
	f.transcript = []byte(fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"read context then stopped %d"}]}}`+"\n", f.writes))
	if !f.noTranscript {
		if err := os.WriteFile(r.ThreadPath, f.transcript, 0o644); err != nil {
			return "", err
		}
	}
	return "11111111-1111-4111-8111-111111111111", f.oneShotErr
}

func (f *emptyCanvasAgent) CopyTranscript(_ string, dest string) (bool, error) {
	if f.noTranscript {
		return false, nil
	}
	return true, os.WriteFile(dest, f.transcript, 0o644)
}

func diagnosticFixture(t *testing.T) (string, *emptyCanvasAgent) {
	t.Helper()
	root, base := setupResumeFixture(t, "fake-unused")
	original, err := agent.Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	fake := &emptyCanvasAgent{fakeAgent: *base}
	agent.Unregister("codex")
	agent.Register("codex", fake)
	t.Cleanup(func() { agent.Unregister("codex"); agent.Register("codex", original) })
	return root, fake
}

func diagnosticAttempts(t *testing.T, root string, want int) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, run.Dir("tele", "fix-it"), "diagnostics/design/*/failure.json"))
	if err != nil || len(paths) != want {
		t.Fatalf("attempts: %v; want %d (%v)", paths, want, err)
	}
	return paths
}

func readStageFailure(t *testing.T, path string) stageFailure {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var failure stageFailure
	if err := json.Unmarshal(b, &failure); err != nil {
		t.Fatal(err)
	}
	return failure
}

func TestMissingCanvasRetainsDiagnostic(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "empty"}[empty], func(t *testing.T) {
			root, fake := diagnosticFixture(t)
			defer withRideMode(rideStatic)()
			fake.empty = empty
			var out, errb bytes.Buffer
			code := runStageSession("tele", "fix-it", "design", stageSessionOpts{
				Agent: "codex", Headless: true, SkipNextStage: true,
			}, &out, &errb)
			if code == 0 {
				t.Fatal("missing canvas advanced")
			}
			attempts, err := filepath.Glob(filepath.Join(root, run.Dir("tele", "fix-it"), "diagnostics/design/*/failure.json"))
			if err != nil || len(attempts) != 1 {
				t.Fatalf("want durable diagnostic, got %v (%v); stderr: %s", attempts, err, errb.String())
			}
			rel, _ := filepath.Rel(root, attempts[0])
			if _, err := git.Output(root, "show", "main:"+rel); err != nil {
				t.Fatal(err)
			}
			body := gitLogFormat(t, root, 1, "HEAD", "%B")
			if strings.Contains(body, "MoE-Document:") || strings.Contains(body, "work: update") {
				t.Fatalf("diagnostic counts as work: %s", body)
			}
			if !strings.Contains(body, "MoE-Consent: static") {
				t.Fatal(body)
			}
			if !strings.Contains(errb.String(), "diagnostics saved") {
				t.Fatal(errb.String())
			}
			out.Reset()
			errb.Reset()
			// Commit clocks and filesystem clocks both disagree with journal
			// order; neither may decide which attempt the operator sees.
			t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00Z")
			future := time.Now().Add(24 * time.Hour)
			if err := os.Chtimes(attempts[0], future, future); err != nil {
				t.Fatal(err)
			}
			if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: "codex", Headless: true, SkipNextStage: true}, &out, &errb); code == 0 {
				t.Fatal("retry advanced without a canvas")
			}
			attempts, _ = filepath.Glob(filepath.Join(root, run.Dir("tele", "fix-it"), "diagnostics/design/*/failure.json"))
			if len(attempts) != 2 {
				t.Fatalf("retry did not archive a second attempt: %v; stderr: %s", attempts, errb.String())
			}
			for _, path := range attempts {
				failure := readStageFailure(t, path)
				if failure.BackendSessionID != "11111111-1111-4111-8111-111111111111" || failure.Execution.ExitCode == nil || *failure.Execution.ExitCode != 0 || !failure.TranscriptAvailable || !failure.PromptAvailable || failure.ObservedAt.IsZero() {
					t.Fatalf("incomplete failure: %+v", failure)
				}
			}
			if sha, _, err := run.LatestWorkTurnSHA(root, "tele", "fix-it", "design"); err != nil || sha != "" {
				t.Fatalf("failure satisfied stage: %s %v", sha, err)
			}
			idx, err := run.BuildJournalIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			if !idx.WorkTurnTime[run.WorkTurnKey{Project: "tele", Run: "fix-it", Doc: "design"}].IsZero() {
				t.Fatal("indexed failure satisfied stage")
			}
			out.Reset()
			errb.Reset()
			if code := runLog("sdlc", "")([]string{"tele/fix-it", "design"}, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}
			if !strings.Contains(out.String(), "read context then stopped 2") || strings.Contains(out.String(), "read context then stopped 1") {
				t.Fatal(out.String())
			}
			// The actual reap destroys the only session worktree. Both attempts
			// and the default log must remain available through canonical main.
			sessions, err := session.List(root)
			if err != nil || len(sessions) != 1 {
				t.Fatalf("sessions: %v %v", sessions, err)
			}
			writeDeadClaim(t, sessions[0], true)
			newHeartbeatGate(root).reap(&errb)
			if _, err := os.Stat(sessions[0].WorktreePath); !os.IsNotExist(err) {
				t.Fatalf("worktree survived reap: %v; %s", err, errb.String())
			}
			for _, path := range diagnosticAttempts(t, root, 2) {
				rel, _ := filepath.Rel(root, path)
				if _, err := git.Output(root, "show", "main:"+rel); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			errb.Reset()
			if code := runLog("sdlc", "")([]string{"tele/fix-it", "design"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "read context then stopped 2") {
				t.Fatalf("log after reap: %s %s", out.String(), errb.String())
			}
			// A successful retry supersedes the failure even if it has no new
			// transcript; neither old diagnostic is removed.
			fake.succeed = true
			if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: "codex", Headless: true, SkipNextStage: true}, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}
			if _, failure, err := latestStageFailure(root, "tele", "fix-it", "design", ""); err != nil || failure != nil {
				t.Fatalf("successful turn didn't supersede failure: %+v %v", failure, err)
			}
			diagnosticAttempts(t, root, 2)
		})
	}
}

func TestDiagnosticExecutionCauses(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 23").Run()
	for _, tc := range []struct {
		name     string
		err      error
		outcome  string
		exitCode int
	}{
		{name: "timeout", err: &agent.TimeoutError{Label: "codex: exec", Timeout: time.Minute}, outcome: "timeout", exitCode: -1},
		{name: "unknown", err: fmt.Errorf("could not start backend"), outcome: "error", exitCode: -1},
		{name: "interrupted", err: agent.ErrInterrupted, outcome: "interrupted", exitCode: -1},
		{name: "nonzero", err: fmt.Errorf("backend: %w", exitErr), outcome: "error", exitCode: 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, fake := diagnosticFixture(t)
			fake.oneShotErr, fake.noTranscript = tc.err, true
			driveDesignTurnCode(t, "codex", true)
			failure := readStageFailure(t, diagnosticAttempts(t, root, 1)[0])
			if failure.Execution.Outcome != tc.outcome || failure.Execution.Error != tc.err.Error() || failure.TranscriptAvailable {
				t.Fatalf("lost execution cause: %+v", failure)
			}
			if tc.exitCode < 0 && failure.Execution.ExitCode != nil {
				t.Fatalf("unknown exit recorded as known: %+v", failure.Execution)
			}
			if tc.exitCode >= 0 && (failure.Execution.ExitCode == nil || *failure.Execution.ExitCode != tc.exitCode) {
				t.Fatalf("known exit lost: %+v", failure.Execution)
			}
			var out, errb bytes.Buffer
			if code := runLog("sdlc", "")([]string{"tele/fix-it", "design"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Transcript unavailable") {
				t.Fatalf("missing transcript log: %s %s", out.String(), errb.String())
			}
		})
	}
}

func TestDiagnosticNormalTurnWithOnlyMetadata(t *testing.T) {
	root, _ := diagnosticFixture(t)
	driveDesignTurnCode(t, "codex", true)
	md := loadRun(t, root, "tele", "fix-it")
	md.Agent = "claude"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	// The agent can commit its canvas itself, leaving commitTurn with only
	// run.json. Such a work turn still supersedes a diagnostic in the log.
	msg := "work: update design\n\nMoE-Project: tele\nMoE-Run: fix-it\nMoE-Document: design\n"
	if err := run.StageAndCommit(root, msg, filepath.Join(run.Dir("tele", "fix-it"), "run.json")); err != nil {
		t.Fatal(err)
	}
	if _, failure, err := latestStageFailure(root, "tele", "fix-it", "design", ""); err != nil || failure != nil {
		t.Fatalf("metadata-only normal turn did not win: %+v %v", failure, err)
	}
}

func TestDiagnosticDoesNotReusePriorMirror(t *testing.T) {
	root, fake := diagnosticFixture(t)
	driveDesignTurnCode(t, "codex", true)
	fake.noTranscript = true
	driveDesignTurnCode(t, "codex", true)
	rel, failure, err := latestStageFailure(root, "tele", "fix-it", "design", "")
	if err != nil || failure == nil || failure.TranscriptAvailable {
		t.Fatalf("stale mirror archived: %+v %v", failure, err)
	}
	if _, err := os.Stat(filepath.Join(root, rel, "thread-codex.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("stale transcript file: %v", err)
	}
	var out, errb bytes.Buffer
	if code := runLog("sdlc", "")([]string{"tele/fix-it", "design"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Transcript unavailable") || strings.Contains(out.String(), "read context") {
		t.Fatalf("log reused stale transcript: %s %s", out.String(), errb.String())
	}
}

// A separate process is required: repolock intentionally refuses nested
// acquisition by the same process instead of waiting for itself.
func TestDiagnosticLockWriterHelper(t *testing.T) {
	root := os.Getenv("MOE_DIAGNOSTIC_LOCK_HELPER")
	if root == "" {
		return
	}
	err := repolock.With(root, repolock.Options{Purpose: "ordinary-test-writer"}, func() error {
		fmt.Println("locked")
		if _, err := os.Stdin.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "ordinary.txt"), []byte("ordinary writer"), 0o644); err != nil {
			return err
		}
		return run.StageAndCommit(root, "ordinary locked writer", "ordinary.txt")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticSuccessfulResume(t *testing.T) {
	root, fake := diagnosticFixture(t)
	if code := driveDesignTurnCode(t, "codex", true); code == 0 {
		t.Fatal("empty turn advanced")
	}
	path := diagnosticAttempts(t, root, 1)[0]
	original := append([]byte(nil), fake.transcript...)
	fake.succeed = true
	driveDesignTurn(t, "codex", true)
	var out, errb bytes.Buffer
	if code := runLog("sdlc", "")([]string{"tele/fix-it", "design"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "completed implementation") || strings.Contains(out.String(), "without the required canvas") {
		t.Fatalf("success log: %s %s", out.String(), errb.String())
	}
	archived, err := os.ReadFile(filepath.Join(filepath.Dir(path), "thread-codex.jsonl"))
	if err != nil || !bytes.Equal(archived, original) {
		t.Fatalf("immutable transcript changed: %s %v", archived, err)
	}
	if sessions, err := session.List(root); err != nil || len(sessions) != 0 {
		t.Fatalf("successful resume did not close: %v %v", sessions, err)
	}
}

func TestDiagnosticSerializesWithJournalWriter(t *testing.T) {
	root, fake := diagnosticFixture(t)
	fake.noTranscript = true
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestDiagnosticLockWriterHelper$")
	cmd.Env = append(os.Environ(), "MOE_DIAGNOSTIC_LOCK_HELPER="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var childErr bytes.Buffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("writer did not acquire lock: %q %v", line, err)
	}
	md := loadRun(t, root, "tele", "fix-it")
	timer := time.AfterFunc(100*time.Millisecond, func() { _ = stdin.Close() })
	defer timer.Stop()
	_, err = saveStageFailure(root, stageTurnInputs{Project: "tele", RunSlug: "fix-it", DocID: "design"}, stageTurnSpec{Metadata: md, DocID: "design"}, fake, "codex", "", "prompt", nil, &canvasRefusalError{Reason: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ordinary writer: %v %s", err, childErr.String())
	}
	subjects := gittest.Output(t, root, "log", "-2", "--format=%s")
	if !strings.HasPrefix(subjects, "diagnostic:") || !strings.Contains(subjects, "\nordinary locked writer") {
		t.Fatalf("diagnostic bypassed ordinary writer's lock: %s", subjects)
	}
	diagnosticAttempts(t, root, 1)
}

func TestDiagnosticCommitIsolationAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail=", fail), func(t *testing.T) {
			root, _ := diagnosticFixture(t)
			gittest.WriteAndCommit(t, root, "unrelated.txt", "base", "seed unrelated")
			if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("staged"), 0o644); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "add", "unrelated.txt")
			if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("unstaged"), 0o644); err != nil {
				t.Fatal(err)
			}
			if fail {
				hooks := filepath.Join(root, ".git", "hooks")
				if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$PWD\" = %q ]; then echo archive-denied >&2; exit 1; fi\n", root)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before := gittest.Output(t, root, "diff", "--cached")
			var out, errb bytes.Buffer
			code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: "codex", Headless: true, SkipNextStage: true}, &out, &errb)
			if code == 0 {
				t.Fatal("failure became success")
			}
			if after := gittest.Output(t, root, "diff", "--cached"); before != after {
				t.Fatalf("unrelated index changed:\n%s\n%s", before, after)
			}
			if got, _ := os.ReadFile(filepath.Join(root, "unrelated.txt")); string(got) != "unstaged" {
				t.Fatal("unrelated worktree changed")
			}
			if fail {
				diagnosticAttempts(t, root, 0)
				for _, want := range []string{"archive-denied", "evidence remains in", "commit turn:", "session close:"} {
					if !strings.Contains(errb.String(), want) {
						t.Fatal(errb.String())
					}
				}
				if strings.Contains(errb.String(), "diagnostics saved:") {
					t.Fatal("false durability claim")
				}
			} else {
				diagnosticAttempts(t, root, 1)
				names := gittest.Output(t, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
				for name := range strings.SplitSeq(strings.TrimSpace(names), "\n") {
					if !strings.HasPrefix(name, diagnosticStageDir("tele", "fix-it", "design")+"/") {
						t.Fatalf("foreign committed path: %s", name)
					}
				}
			}
		})
	}
}
