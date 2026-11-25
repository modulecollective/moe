//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
)

func TestCloneCLIHelper(t *testing.T) {
	if os.Getenv("MOE_TEST_CLONE_CLI") == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("MOE_TEST_CLONE_CLI")), &args); err != nil {
		os.Exit(99)
	}
	os.Exit(Run(args, os.Stdout, os.Stderr))
}

func cloneCLICommand(t *testing.T, root string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCloneCLIHelper$")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "MOE_HOME="+root, "MOE_SERVE_AGENT=", "MOE_SESSION_ROOT=", "MOE_TEST_CLONE_CLI="+string(data))
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func waitCloneCondition(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloneCLIExclusiveShellOwnsDetachedJobs(t *testing.T) {
	root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	barrier := filepath.Join(t.TempDir(), "release")
	script := filepath.Join(t.TempDir(), "shell")
	writeFile(t, script, "#!/bin/sh\nsetsid sh -c 'printf started > \"$MOE_TEST_BARRIER-started\"; i=0; while [ ! -f \"$MOE_TEST_BARRIER\" ] && [ \"$i\" -lt 800 ]; do i=$((i+1)); sleep .01; done' </dev/null >/dev/null 2>&1 &\nprintf exited > \"$MOE_TEST_BARRIER-exited\"\nexit 7\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := cloneCLICommand(t, root, "sdlc", "shell", "tele/finish")
	cmd.Env = append(cmd.Env, "SHELL="+script, "MOE_TEST_BARRIER="+barrier)
	log, err := os.CreateTemp(t.TempDir(), "shell-output")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		_ = os.WriteFile(barrier, nil, 0o600)
		if reaped {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}()
	waitCloneCondition(t, "shell exit with detached job", func() bool {
		_, a := os.Stat(barrier + "-started")
		_, b := os.Stat(barrier + "-exited")
		return a == nil && b == nil
	})
	result := collectClone(root, "tele", "finish", true, true, time.Now().UTC(), io.Discard, io.Discard)
	if result.Reason != "owner_busy" {
		t.Fatalf("collector entered live shell: %+v", result)
	}
	for _, args := range [][]string{{"sdlc", "design", "--once", "tele/finish"}, {"sdlc", "close", "tele/finish"}, {"sdlc", "push", "tele/finish"}, {"sdlc", "shell", "tele/finish"}} {
		var output safeBuffer
		if code := Run(args, &output, &output); code != 1 || !strings.Contains(output.String(), "owned by another") {
			t.Fatalf("%v entered shell ownership: %d %s", args, code, output.String())
		}
	}
	// Outer process death leaves the dedicated supervisor's descriptor alive.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		reaped = true
	case <-time.After(5 * time.Second):
		t.Fatal("outer process did not exit")
	}
	waitCloneCondition(t, "parent exit while supervisor owns", func() bool { busy, err := runowner.Busy(root, "tele", "finish"); return err == nil && busy })
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitCloneCondition(t, "guard release after descendants", func() bool { busy, err := runowner.Busy(root, "tele", "finish"); return err == nil && !busy })
	h, err := runowner.Acquire(root, "tele", "finish")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if err := clonereclaim.CheckRunRecovery(h); err != nil {
		t.Fatal("completed shell left uncertainty:", err)
	}
	blocked := cloneCLICommand(t, root, "sdlc", "shell", "tele/finish")
	blocked.Env = append(blocked.Env, "SHELL="+script)
	if err := blocked.Run(); err == nil {
		t.Fatal("shell entered another owner's guard")
	}
}

func TestCloneCLIShellPublishesWaitingPhase(t *testing.T) {
	root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	barrier := filepath.Join(t.TempDir(), "release")
	script := filepath.Join(t.TempDir(), "shell")
	writeFile(t, script, "#!/bin/sh\nsetsid sh -c 'i=0; while [ ! -f \"$MOE_TEST_BARRIER\" ] && [ \"$i\" -lt 800 ]; do i=$((i+1)); sleep .01; done' </dev/null >/dev/null 2>&1 &\nexit 7\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", script)
	t.Setenv("MOE_TEST_BARRIER", barrier)
	done := make(chan int, 1)
	go func() { done <- shellRunWorkspace(root, "tele", "finish", io.Discard, io.Discard) }()
	defer func() {
		os.WriteFile(barrier, nil, 0o600)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("shell phase probe did not finish")
		}
	}()
	waitCloneCondition(t, "shell jobs phase", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, err := runsock.ReadStatus(ctx, runsock.RuntimeDir(), runsock.Identity{Root: root, Project: "tele", Run: "finish"})
		return err == nil && status.Phase == "waiting for shell jobs"
	})
	if busy, err := runowner.Busy(root, "tele", "finish"); err != nil || !busy {
		t.Fatalf("waiting phase released owner: %v %v", busy, err)
	}
}

func startCloneTTY(t *testing.T, root string, args ...string) (*exec.Cmd, *os.File, *safeBuffer, chan error) {
	t.Helper()
	master, terminal := cooldownPTY(t)
	if err := syscall.SetNonblock(int(terminal.Fd()), false); err != nil {
		t.Fatal(err)
	}
	cmd := cloneCLICommand(t, root, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = terminal, terminal, terminal
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var screen safeBuffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&screen, master); close(drained) }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); terminal.Close(); master.Close(); <-drained })
	return cmd, master, &screen, done
}

func TestCloneCLITTYJobControlInterruptAndExitCode(t *testing.T) {
	root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	t.Setenv("SHELL", "/bin/bash")
	_, master, screen, done := startCloneTTY(t, root, "sdlc", "shell", "tele/finish")
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(master, s); err != nil {
			t.Fatal(err)
		}
	}
	write("printf '__ready__\\n'; sleep 20\n")
	// Match only the marker's trailing CRLF. The typed command's echo has a
	// literal backslash-n there, and bash with bracketed paste precedes the
	// output with an escape sequence and a bare CR rather than CRLF.
	waitCloneCondition(t, "foreground command", func() bool { return strings.Contains(screen.String(), "__ready__\r\n") })
	write("\x1a")
	waitCloneCondition(t, "stopped foreground job", func() bool { return strings.Contains(screen.String(), "Stopped") })
	write("fg\n")
	time.Sleep(100 * time.Millisecond)
	write("\x03")
	write("printf '__recovered__\\n'; exit 7\n")
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("shell exit=%v screen=%s", err, screen.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("TTY did not recover after Ctrl-Z/fg/Ctrl-C: " + screen.String())
	}
	if !strings.Contains(screen.String(), "__recovered__\r\n") {
		t.Fatal("shell did not execute after interrupt: " + screen.String())
	}
	if busy, err := runowner.Busy(root, "tele", "finish"); err != nil || busy {
		t.Fatalf("TTY shell left ownership: %v %v", busy, err)
	}
}

func TestCloneCLIInteractiveRecoveryRetainsActiveClone(t *testing.T) {
	root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	_, master, screen, done := startCloneTTY(t, root, "clone", "recover", "tele/finish")
	waitCloneCondition(t, "concrete recovery prompt", func() bool { return strings.Contains(screen.String(), "Type \"tele/finish stopped\"") })
	if _, err := io.WriteString(master, "tele/finish stopped\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovery: %v %s", err, screen.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovery did not finish")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("recovery removed active clone")
	}
	if loadRun(t, root, "tele", "finish").Status != run.StatusInProgress {
		t.Fatal("recovery closed active run")
	}
}

func TestCloneGCApplyOwnsHooksAgainstAnotherCollector(t *testing.T) {
	root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	parent := installCloseResourceHooks(t, root, "tele")
	script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, script, strings.Replace(string(body), "set -eu\n", "set -eu\nprintf started > \"$TEST_CLOSE_PARENT/started\"\ni=0; while [ ! -f \"$TEST_CLOSE_PARENT/release\" ] && [ \"$i\" -lt 800 ]; do i=$((i+1)); sleep .01; done\n", 1))
	gittest.Run(t, root, "add", script)
	gittest.Commit(t, root, "blocking cleanup contract")
	resources := allocateCloseResources(t, root, "tele", "finish")
	commitGCState(t, root, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	cmd := cloneCLICommand(t, root, "clone", "gc", "--apply", "--json", "tele/finish")
	var stdout, stderr safeBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		os.WriteFile(filepath.Join(parent, "release"), nil, 0o600)
		if !reaped {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	}()
	waitCloneCondition(t, "collector hook", func() bool { _, err := os.Stat(filepath.Join(parent, "started")); return err == nil })
	other := collectClone(root, "tele", "finish", true, true, time.Now().UTC(), io.Discard, io.Discard)
	if other.Reason != "owner_busy" {
		t.Fatalf("second collector entered: %+v", other)
	}
	if code := shellRunWorkspace(root, "tele", "finish", io.Discard, io.Discard); code != 1 {
		t.Fatal("shell entered collector ownership")
	}
	if err := os.WriteFile(filepath.Join(parent, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		reaped = true
		if err != nil {
			t.Fatalf("collector: %v %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("collector did not finish")
	}
	var report cloneReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatal(err)
	}
	if report.Candidates[0].Outcome != "removed" {
		t.Fatalf("collector outcome: %+v", report)
	}
	resources.assertReleased(t)
}

func TestCloneGCRecoversAfterParentDiesDuringTeardown(t *testing.T) {
	root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	parent := installCloseResourceHooks(t, root, "tele")
	script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, script, strings.Replace(string(body), "set -eu\n", "set -eu\nprintf started > \"$TEST_CLOSE_PARENT/started\"\ni=0; while [ ! -f \"$TEST_CLOSE_PARENT/release\" ] && [ \"$i\" -lt 800 ]; do i=$((i+1)); sleep .01; done\n", 1))
	gittest.Run(t, root, "add", script)
	gittest.Commit(t, root, "parent-death cleanup contract")
	resources := allocateCloseResources(t, root, "tele", "finish")
	commitGCState(t, root, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	cmd := cloneCLICommand(t, root, "clone", "gc", "--apply", "--json", "tele/finish")
	output, err := os.CreateTemp(t.TempDir(), "collector-output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		os.WriteFile(filepath.Join(parent, "release"), nil, 0o600)
		if !reaped {
			cmd.Process.Kill()
			<-done
		}
		waitCloneCondition(t, "cleanup supervisor teardown", func() bool { busy, err := runowner.Busy(root, "tele", "finish"); return err == nil && !busy })
	}()
	waitCloneCondition(t, "cleanup supervisor hook", func() bool { _, err := os.Stat(filepath.Join(parent, "started")); return err == nil })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		reaped = true
	case <-time.After(5 * time.Second):
		t.Fatal("collector parent did not exit")
	}
	if busy, err := runowner.Busy(root, "tele", "finish"); err != nil || !busy {
		t.Fatal("parent death lost worker ownership")
	}
	if err := os.WriteFile(filepath.Join(parent, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitCloneCondition(t, "completed worker releases guard", func() bool { busy, err := runowner.Busy(root, "tele", "finish"); return err == nil && !busy })
	result := collectClone(root, "tele", "finish", true, true, time.Now().UTC(), io.Discard, io.Discard)
	if result.Outcome != "removed" {
		t.Fatalf("roll forward cleanup: %+v", result)
	}
	resources.assertReleased(t)
}
