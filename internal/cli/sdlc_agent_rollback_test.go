package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

func agentSwitchFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	stubEditor(t)
	suppressNextStagePrompt(t)
	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/switch-agent"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	return root
}

// HEAD, index and working tree have different metadata. The working bytes
// also include formatting and an unknown field that reserialization loses.
func seedAgentSwitchTarget(t *testing.T, root, slug string) (string, []byte, string) {
	t.Helper()
	rel := filepath.Join(run.Dir("tele", slug), "run.json")
	abs := filepath.Join(root, rel)
	md := loadRun(t, root, "tele", slug)
	md.Agent = "claude"
	md.ReopenOf = "staged-source"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", rel)
	gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
	index := gittest.Output(t, root, "ls-files", "-s", "--", rel)
	md.ReopenOf = "working-source"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	working, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	working = bytes.Replace(working, []byte("{"), []byte("{\n  \"future_field\": {\"keep\": true},"), 1)
	working = append([]byte(" \n"), working...)
	if err := os.WriteFile(abs, working, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(abs, 0o644); err != nil {
		t.Fatal(err)
	}
	return rel, working, index
}

func assertAgentSwitchTarget(t *testing.T, root, rel string, working []byte, index string) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(root, rel)); err != nil || !bytes.Equal(got, working) {
		t.Errorf("working metadata changed: got=%q err=%v want=%q", got, err, working)
	}
	if got := gittest.Output(t, root, "ls-files", "-s", "--", rel); got != index {
		t.Errorf("target index changed: got=%q want=%q", got, index)
	}
	if info, err := os.Stat(filepath.Join(root, rel)); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("working mode changed: info=%v err=%v", info, err)
	}
}

func rejectAgentSwitch(t *testing.T, root string) {
	t.Helper()
	installCreateFailureHook(t, root, `case "$(head -n 1 "$1")" in
  "switch agent:"*) echo reject-switch >&2; exit 1 ;;
esac
`)
}

func TestSDLCStageAgentAbsentOrUnmergedIndex(t *testing.T) {
	for _, state := range []string{"staged-deletion", "untracked", "unmerged"} {
		t.Run(state, func(t *testing.T) {
			root := agentSwitchFixture(t)
			assertUnrelated := seedAgentSwitchStaging(t, root, "switch-agent")
			rel, working, _ := seedAgentSwitchTarget(t, root, "switch-agent")
			gittest.Run(t, root, "update-index", "--force-remove", "--", rel)
			if state == "untracked" {
				// Remove only the HEAD entry while preserving the separately
				// staged unrelated work and the target's working bytes.
				if err := os.Remove(filepath.Join(root, rel)); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "commit", "--only", "-m", "untrack metadata", "--", rel)
				if err := os.WriteFile(filepath.Join(root, rel), working, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if state == "unmerged" {
				blob := gittest.Output(t, root, "rev-parse", "HEAD:"+rel)
				entries, err := os.CreateTemp(t.TempDir(), "index-info")
				if err != nil {
					t.Fatal(err)
				}
				defer entries.Close()
				for stage := 1; stage <= 3; stage++ {
					if _, err := fmt.Fprintf(entries, "100644 %s %d\t%s\n", blob, stage, rel); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := entries.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				prevStdin := os.Stdin
				os.Stdin = entries
				err = git.Stream(root, io.Discard, io.Discard, "update-index", "--index-info")
				os.Stdin = prevStdin
				if err != nil {
					t.Fatal(err)
				}
			}
			index := gittest.Output(t, root, "ls-files", "-s", "--", rel)
			before := gittest.HeadSHA(t, root)
			rejectAgentSwitch(t, root)
			var out, errb bytes.Buffer
			_, code := persistSDLCStageAgent("sdlc design", "design", "tele", "switch-agent", "codex", &out, &errb)
			want := "reject-switch"
			if state == "unmerged" {
				want = "unmerged index entry"
			}
			if code != 1 || !strings.Contains(errb.String(), want) || out.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errb.String())
			}
			if got := gittest.HeadSHA(t, root); got != before {
				t.Errorf("HEAD changed: %s -> %s", before, got)
			}
			assertAgentSwitchTarget(t, root, rel, working, index)
			assertUnrelated()
		})
	}
}

func TestSDLCStageAgentEarlyFailures(t *testing.T) {
	for _, failure := range []string{"lock", "partial-save", "stage"} {
		t.Run(failure, func(t *testing.T) {
			root := agentSwitchFixture(t)
			assertUnrelated := seedAgentSwitchStaging(t, root, "switch-agent")
			rel, working, index := seedAgentSwitchTarget(t, root, "switch-agent")
			before := gittest.HeadSHA(t, root)
			marker := "partial-save-failed"
			switch failure {
			case "lock":
				lock, err := repolock.Acquire(root, repolock.Options{Purpose: "test-refusal"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := lock.Release(); err != nil {
						t.Error(err)
					}
				})
				marker = "nested Acquire"
			case "partial-save":
				prev := saveSDLCStageAgent
				saveSDLCStageAgent = func(root string, md *run.Metadata) error {
					if err := os.WriteFile(filepath.Join(root, rel), []byte("{partial"), 0o644); err != nil {
						return err
					}
					return errors.New(marker)
				}
				t.Cleanup(func() { saveSDLCStageAgent = prev })
			case "stage":
				marker = "ignored"
				gittest.Run(t, root, "update-index", "--force-remove", "--", rel)
				index = ""
				if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte(rel+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out, errb bytes.Buffer
			_, code := persistSDLCStageAgent("sdlc design", "design", "tele", "switch-agent", "codex", &out, &errb)
			if code != 1 || !strings.Contains(errb.String(), marker) || out.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errb.String())
			}
			if got := gittest.HeadSHA(t, root); got != before {
				t.Errorf("HEAD changed: %s", got)
			}
			assertAgentSwitchTarget(t, root, rel, working, index)
			assertUnrelated()
		})
	}
}

func TestSDLCStageAgentIncompleteRollback(t *testing.T) {
	for _, failure := range []string{"file", "index", "both"} {
		t.Run(failure, func(t *testing.T) {
			root := agentSwitchFixture(t)
			rel, working, index := seedAgentSwitchTarget(t, root, "switch-agent")
			body := ""
			if failure != "index" {
				body += fmt.Sprintf("rm -- %s\nmkdir -- %s\n", rel, rel)
			}
			if failure != "file" {
				body += "if [ -f .git/index.lock ]; then mv .git/index.lock .git/fixture-index.lock; fi\nmkdir .git/index.lock\n"
			}
			installCreateFailureHook(t, root, body+"echo reject-switch >&2\nexit 1\n")
			before := gittest.HeadSHA(t, root)
			calls := 0
			prev := runStageSession
			runStageSession = func(_, _, _ string, _ stageSessionOpts, _, _ io.Writer) int { calls++; return 0 }
			t.Cleanup(func() { runStageSession = prev })
			var out, errb bytes.Buffer
			code := runDesign([]string{"--agent", "codex", "tele/switch-agent"}, &out, &errb)
			if code != 1 || calls != 0 || out.Len() != 0 {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, out.String(), errb.String())
			}
			for _, want := range []string{"reject-switch", "rollback incomplete", rel, "inspect and repair", "before retrying"} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("missing %q: %s", want, errb.String())
				}
			}
			if failure != "file" {
				if !strings.Contains(errb.String(), "restore index") {
					t.Error(errb.String())
				}
				if err := os.Remove(filepath.Join(root, ".git", "index.lock")); err != nil {
					t.Fatal(err)
				}
			}
			if failure != "index" {
				if !strings.Contains(errb.String(), "restore file") {
					t.Error(errb.String())
				}
			} else if got, err := os.ReadFile(filepath.Join(root, rel)); err != nil || !bytes.Equal(got, working) {
				t.Errorf("file recovery skipped after index error: %q %v", got, err)
			}
			if failure == "file" && gittest.Output(t, root, "ls-files", "-s", "--", rel) != index {
				t.Error("index recovery skipped after file error")
			}
			if got := gittest.HeadSHA(t, root); got != before {
				t.Error("HEAD changed on failed commit")
			}
		})
	}
}

// A separate process is required: repolock deliberately refuses same-process
// contention. The parent releases this writer only from the lock's wait seam.
func TestSDLCStageAgentLockWriterHelper(t *testing.T) {
	root := os.Getenv("MOE_AGENT_LOCK_HELPER")
	if root == "" {
		return
	}
	err := repolock.With(root, repolock.Options{Purpose: "agent-test-writer"}, func() error {
		fmt.Println("locked")
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			return err
		}
		md, err := run.Load(root, "tele", "switch-agent")
		if err != nil {
			return err
		}
		md.ReopenOf = "published-under-lock"
		md.Agent = os.Getenv("MOE_AGENT_LOCK_BINDING")
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, "publish newer metadata", filepath.Join(run.Dir("tele", "switch-agent"), "run.json"))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSDLCStageAgentReloadsUnderLock(t *testing.T) {
	for _, outcome := range []string{"reject", "success", "noop"} {
		t.Run(outcome, func(t *testing.T) {
			root := agentSwitchFixture(t)
			md := loadRun(t, root, "tele", "switch-agent")
			md.Agent = "codex"
			freshAgent := "claude"
			if outcome == "noop" {
				md.Agent, freshAgent = "claude", "codex"
			}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			rel := filepath.Join(run.Dir("tele", md.ID), "run.json")
			gittest.Run(t, root, "add", "--", rel)
			gittest.Run(t, root, "commit", "-m", "initial binding")
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "-test.run=^TestSDLCStageAgentLockWriterHelper$")
			cmd.Env = append(os.Environ(), "MOE_AGENT_LOCK_HELPER="+root, "MOE_AGENT_LOCK_BINDING="+freshAgent)
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
				t.Fatalf("child failed to lock: %q %v", line, err)
			}
			if outcome == "reject" {
				rejectAgentSwitch(t, root)
			}
			prev := withSDLCStageAgentLock
			waited := false
			var published []byte
			var publishedIndex, publishedHead string
			withSDLCStageAgentLock = func(root string, opts repolock.Options, fn func() error) error {
				opts.Sleep = func(time.Duration) {
					if waited {
						t.Fatal("still waiting after cooperating writer released")
					}
					waited = true
					if err := stdin.Close(); err != nil {
						t.Fatal(err)
					}
					if err := cmd.Wait(); err != nil {
						t.Fatalf("writer: %v %s", err, childErr.String())
					}
					var err error
					published, err = os.ReadFile(filepath.Join(root, rel))
					if err != nil {
						t.Fatal(err)
					}
					publishedIndex = gittest.Output(t, root, "ls-files", "-s", "--", rel)
					publishedHead = gittest.HeadSHA(t, root)
				}
				return repolock.With(root, opts, fn)
			}
			t.Cleanup(func() { withSDLCStageAgentLock = prev })
			var out, errb bytes.Buffer
			_, code := persistSDLCStageAgent("sdlc design", "design", "tele", "switch-agent", "codex", &out, &errb)
			if !waited {
				t.Fatal("switch decided before lock acquisition")
			}
			if outcome == "reject" {
				if code != 1 || !strings.Contains(errb.String(), "reject-switch") || out.Len() != 0 {
					t.Fatalf("exit=%d out=%q err=%q", code, out.String(), errb.String())
				}
				assertAgentSwitchTarget(t, root, rel, published, publishedIndex)
			} else {
				if code != 0 {
					t.Fatalf("exit=%d err=%q", code, errb.String())
				}
				fresh := loadRun(t, root, "tele", "switch-agent")
				if fresh.ReopenOf != "published-under-lock" || fresh.Agent != "codex" {
					t.Fatalf("stale metadata: %+v", fresh)
				}
			}
			if outcome == "success" {
				assertAgentSwitchCommit(t, root, "switch-agent", publishedHead)
			} else if got := gittest.HeadSHA(t, root); got != publishedHead || out.Len() != 0 {
				t.Errorf("unexpected switch commit/announcement: head=%s published=%s out=%q", got, publishedHead, out.String())
			}
		})
	}
}
