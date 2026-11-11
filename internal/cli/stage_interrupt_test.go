package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// Run in a child test process so actual SIGINT never targets the test runner.
// Stdin/stdout provide barriers at real Git boundaries, not timing guesses.
func TestStageInterruptHelper(t *testing.T) {
	root := os.Getenv("MOE_STAGE_INTERRUPT_ROOT")
	if root == "" {
		return
	}
	boundary := os.Getenv("MOE_STAGE_INTERRUPT_BOUNDARY")
	received := make(chan os.Signal, 2)
	signal.Notify(received, os.Interrupt)
	defer signal.Stop(received)
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_SERVE_AGENT", "")
	t.Setenv("MOE_SERVE_DYNAMIC", "")
	cooldownSkipped.Store(true)
	fake := &fakeAgent{canvasRel: run.ContentPath("tele", "fix-it", "design"), transcriptFound: true}
	agent.Register("interrupt-test", fake)
	barrier := func() {
		fmt.Println("boundary")
		if boundary == "double" {
			<-received
			fmt.Println("first interrupt")
		}
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
	}
	reached := false
	var worktree string
	git.Hook = func(dir string, args []string, _ time.Duration, err error) {
		if err != nil || reached {
			return
		}
		command := strings.Join(args, " ")
		match := false
		switch boundary {
		case "agent-return":
			match = dir != root && strings.HasPrefix(command, "add ") && len(fake.execCalls) == 1
		case "commit":
			match = dir != root && strings.HasPrefix(command, "commit ") && len(fake.execCalls) == 1
		case "merge", "double", "hard-kill":
			match = dir == root && strings.HasPrefix(command, "merge --ff-only session/")
		case "removal":
			match = dir == root && strings.HasPrefix(command, "worktree remove --force ")
		}
		if !match {
			return
		}
		reached = true
		if dir != root {
			worktree = dir
		}
		barrier()
		if boundary == "merge" {
			// Force a close failure after landing, so a new identical stage command
			// has to settle the pending close before starting its next turn.
			entries, err := session.List(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("session at merge: %v %v", entries, err)
			}
			worktree = entries[0].WorktreePath
			p := filepath.Join(worktree, run.ContentPath("tele", "fix-it", "design"))
			if err := os.WriteFile(p, []byte("uncommitted after landing"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: "interrupt-test", SkipNextStage: true}, io.Discard, os.Stderr)
	git.Hook = nil
	if !reached || code != exitInterrupted || len(fake.execCalls) != 1 {
		t.Fatalf("interrupted turn: boundary=%v code=%d launches=%d", reached, code, len(fake.execCalls))
	}
	fmt.Println("unwound")
	if boundary == "merge" {
		c, ok := session.ReadClaim(root, "tele", "fix-it", "design")
		if !ok || c.ReleasedAt.IsZero() {
			t.Fatal("failed close did not release ownership")
		}
		p := filepath.Join(worktree, run.ContentPath("tele", "fix-it", "design"))
		if err := os.WriteFile(p, []byte("# fake turn 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(worktree, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	md, err := run.Load(root, "tele", "fix-it")
	if err != nil {
		t.Fatal(err)
	}
	sid := md.Documents["design"].Sessions["interrupt-test"]
	if sid == "" {
		t.Fatal("native identity lost")
	}
	// Same entry point, same stage: one new user turn, never a cleanup agent.
	code = runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: "interrupt-test", SkipNextStage: true}, io.Discard, os.Stderr)
	if code != 0 || len(fake.execCalls) != 2 {
		t.Fatalf("resume: code=%d launches=%d", code, len(fake.execCalls))
	}
	if got := fake.execCalls[1]; got.sessionID != sid || got.newSession {
		t.Fatalf("identity changed: %+v wanted %s", got, sid)
	}
	if err := session.RequireUnreserved(root, "tele", "fix-it"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("resumed")
}

func TestStageInterruptUnwindsAndResumes(t *testing.T) {
	for _, boundary := range []string{"agent-return", "commit", "merge", "removal", "double", "hard-kill"} {
		t.Run(boundary, func(t *testing.T) {
			root, _ := setupResumeFixture(t, "parent-fixture")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStageInterruptHelper$")
			cmd.Env = append(os.Environ(), "MOE_STAGE_INTERRUPT_ROOT="+root, "MOE_STAGE_INTERRUPT_BOUNDARY="+boundary)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			lines := bufio.NewReader(output)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			for {
				line, err := lines.ReadString('\n')
				if err != nil {
					t.Fatalf("no controlled boundary: %v %s", err, &stderr)
				}
				if line == "boundary\n" {
					break
				}
			}
			if boundary == "hard-kill" {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				// A separate notification confirms the first signal reached the
				// child before sending the second; SIGINTs can otherwise coalesce.
				if boundary == "double" {
					line, err := lines.ReadString('\n')
					if err != nil || line != "first interrupt\n" {
						t.Fatalf("first signal acknowledgment: %q %v", line, err)
					}
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := input.Write([]byte("x")); err != nil {
						t.Fatal(err)
					}
				}
			}
			rest, err := io.ReadAll(lines)
			if err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			if boundary == "double" || boundary == "hard-kill" {
				if waitErr == nil {
					t.Fatal("force termination returned success")
				}
				c, ok := session.ReadClaim(root, "tele", "fix-it", "design")
				if !ok || !c.ReleasedAt.IsZero() {
					t.Fatal("forced termination published release")
				}
				if boundary == "double" {
					if !strings.Contains(waitErr.Error(), "130") {
						t.Fatalf("second interrupt: %v", waitErr)
					}
				}
				_, _, err := session.Reserve(root, "tele", "fix-it", "design", false)
				if err == nil {
					t.Fatal("fresh crash bypassed stale-heartbeat requirement")
				}
				return
			}
			if waitErr != nil || !strings.Contains(string(rest), "unwound\n") || !strings.Contains(string(rest), "resumed\n") {
				t.Fatalf("signal drive: %v\n%s\n%s", waitErr, rest, &stderr)
			}
			if boundary == "merge" {
				paths, err := filepath.Glob(filepath.Join(root, ".moe", "session-recovery", "*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("preservation: %v %v", paths, err)
				}
				if code := runSessionGC(nil, io.Discard, &stderr); code != 0 {
					t.Fatalf("gc %d: %s", code, &stderr)
				}
				if _, err := os.Stat(paths[0]); err != nil {
					t.Fatal("GC removed recovery files", err)
				}
			}
		})
	}
}

func TestStageEarlyFailuresReleaseOwnership(t *testing.T) {
	for _, boundary := range []string{"bootstrap", "quota", "commit"} {
		t.Run(boundary, func(t *testing.T) {
			root := newTestBureaucracy(t)
			s, stop, err := session.Reserve(root, "tele", "r", "design", false)
			if err != nil {
				t.Fatal(err)
			}
			stop()
			if err := session.ReleaseOwnership(s); err != nil {
				t.Fatal(err)
			}
			in := stageTurnInputs{Project: "tele", RunSlug: "r", DocID: "design", LockPurpose: "test"}
			in.BuildSpec = func(string) (stageTurnSpec, error) { return stageTurnSpec{}, fmt.Errorf("bootstrap failed") }
			if boundary == "quota" {
				in.Admit = func(*session.Session) int { return exitNotStarted }
			}
			if boundary == "commit" {
				f := &fakeAgent{canvasRel: run.ContentPath("tele", "r", "design"), transcriptFound: true}
				agent.Register("failed-commit-test", f)
				defer agent.Unregister("failed-commit-test")
				in.BuildSpec = func(string) (stageTurnSpec, error) {
					return stageTurnSpec{Agent: "failed-commit-test", BuildPrompt: func(string) (string, error) { return "test", nil }, CommitStager: func(string, error) error { return fmt.Errorf("commit failed") }}, nil
				}
			}
			if code := runStageTurn(root, in, io.Discard, io.Discard); code == 0 {
				t.Fatal("failure reported success")
			}
			c, ok := session.ReadClaim(root, "tele", "r", "design")
			if !ok || c.ReleasedAt.IsZero() {
				t.Fatal("retained failed session not released")
			}
			resumed, stop, err := session.Reserve(root, "tele", "r", "design", false)
			if err != nil {
				t.Fatal(err)
			}
			stop()
			if resumed.WorktreePath != s.WorktreePath {
				t.Fatal("lost resumed tree")
			}
		})
	}
}

func TestInterruptedCloseNeverLaunchesResolveAgent(t *testing.T) {
	old := launchRebaseResolve
	defer func() { launchRebaseResolve = old }()
	launchRebaseResolve = func(string, string, io.Writer, io.Writer) error {
		t.Fatal("resolve agent launched after interrupt")
		return nil
	}
	err := closeWithAutoResolveUnlessInterrupted(func() error { return &session.RebaseFailureError{Branch: "session/p/r/d"} }, io.Discard, io.Discard, func() bool { return true })
	if err == nil {
		t.Fatal("close error swallowed")
	}
}
