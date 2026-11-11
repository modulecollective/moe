package runowner

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The helper process: hold one run's guard, say so, then either wait for
// stdin to close or spawn a long-lived grandchild and report its pid.
func TestMain(m *testing.M) {
	switch os.Getenv("RUNOWNER_HELPER") {
	case "hold":
		h, err := Acquire(os.Getenv("RUNOWNER_ROOT"), "p", "r")
		if err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		io.Copy(io.Discard, os.Stdin)
		h.Release()
		os.Exit(0)
	case "sleep":
		os.Stdout.WriteString("sleeping\n")
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(t *testing.T, mode, root string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "RUNOWNER_HELPER="+mode, "RUNOWNER_ROOT="+root)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("helper %s: %v", mode, err)
	}
	if want := map[string]string{"hold": "held\n", "sleep": "sleeping\n"}[mode]; line != want {
		t.Fatalf("helper %s said %q", mode, line)
	}
	return cmd, in
}

func mustBusy(t *testing.T, root string, want bool) {
	t.Helper()
	got, err := Busy(root, "p", "r")
	if err != nil || got != want {
		t.Fatalf("Busy = %v, %v; want %v", got, err, want)
	}
}

func TestSecondAcquisitionConflictsInProcess(t *testing.T) {
	root := t.TempDir()
	mustBusy(t, root, false)
	h, err := Acquire(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(root, "p", "r"); !IsBusy(err) {
		t.Fatalf("second acquire in one process = %v, want busy", err)
	}
	mustBusy(t, root, true)
	other, err := Acquire(root, "p", "other")
	if err != nil {
		t.Fatalf("a different run must not contend: %v", err)
	}
	other.Release()
	if !h.Owns(root, "p", "r") || h.Owns(root, "p", "other") {
		t.Fatal("handle authority is not exact to its run")
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if h.Owns(root, "p", "r") {
		t.Fatal("released handle still owns")
	}
	mustBusy(t, root, false)
	if _, err := os.Stat(Path(root, "p", "r")); err != nil {
		t.Fatalf("guard file must survive release: %v", err)
	}
	h2, err := Acquire(root, "p", "r")
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	h2.Release()
}

func TestGuardIsIgnoredByGit(t *testing.T) {
	root := t.TempDir()
	h, err := Acquire(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	body, err := os.ReadFile(filepath.Join(root, ".moe", ".gitignore"))
	if err != nil || !strings.Contains(string(body), "*") {
		t.Fatalf(".moe/.gitignore = %q, %v", body, err)
	}
}

func TestOtherProcessHoldsUntilExitOrKill(t *testing.T) {
	root := t.TempDir()
	cmd, in := helper(t, "hold", root)
	mustBusy(t, root, true)
	if _, err := Acquire(root, "p", "r"); !IsBusy(err) {
		t.Fatalf("acquire against another process = %v, want busy", err)
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	mustBusy(t, root, false)

	// Hard kill: the kernel drops the lock, no cleanup runs.
	cmd, _ = helper(t, "hold", root)
	mustBusy(t, root, true)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	h, err := Acquire(root, "p", "r")
	if err != nil {
		t.Fatalf("acquire after SIGKILL: %v", err)
	}
	h.Release()
}

// A child started while the guard is held — an agent, a viewer — must
// not inherit it: once the owner releases, the run is free even though
// the child is still running.
func TestChildrenDoNotInheritTheGuard(t *testing.T) {
	root := t.TempDir()
	h, err := Acquire(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	helper(t, "sleep", root)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	mustBusy(t, root, false)
}

func TestFilesystemFailuresAreNotUnowned(t *testing.T) {
	root := t.TempDir()
	// A file where the project directory belongs: ENOTDIR, not ENOENT.
	if err := os.MkdirAll(filepath.Join(root, ".moe", "run-owners"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".moe", "run-owners", "p"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if busy, err := Busy(root, "p", "r"); err == nil {
		t.Fatalf("Busy on an unreadable guard = %v, nil; want an error", busy)
	}
	if _, err := Acquire(root, "p", "r"); err == nil || IsBusy(err) {
		t.Fatalf("Acquire on an unreadable guard = %v; want a non-busy error", err)
	}
}
