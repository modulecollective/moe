package runowner

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// holdGuard opens the run's guard on its own descriptor and flocks it
// with how, standing in for a concurrent probe (LOCK_SH) or owner
// (LOCK_EX). Closing the returned file drops the lock.
func holdGuard(t *testing.T, root string, how int) *os.File {
	t.Helper()
	p := Path(root, "p", "r")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold guard: %v", err)
	}
	return f
}

// openGuard opens a fresh, unlocked descriptor on an existing guard.
func openGuard(t *testing.T, root string) *os.File {
	t.Helper()
	f, err := os.Open(Path(root, "p", "r"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func mustContendExclusive(t *testing.T, f *os.File) {
	t.Helper()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("exclusive flock = %v, want EWOULDBLOCK", err)
	}
}

func noSleep(t *testing.T) func(time.Duration) {
	return func(time.Duration) { t.Helper(); t.Fatal("uncontended lock asked to wait") }
}

// Two probes overlapping must not read each other as owners. A held
// shared lock is a probe frozen mid-flight.
func TestConcurrentProbesAreCompatible(t *testing.T) {
	root := t.TempDir()
	holdGuard(t, root, syscall.LOCK_SH)
	mustBusy(t, root, false)
	mustContendExclusive(t, openGuard(t, root)) // the fixture really is locked
}

// An acquisition that collides with a probe retries past it: the probe
// is still held at the first attempt, released at the first wait, and
// the next attempt owns the run.
func TestAcquireRidesOutATransientProbe(t *testing.T) {
	root := t.TempDir()
	probe := holdGuard(t, root, syscall.LOCK_SH)
	f := openGuard(t, root)
	var waits int
	err := lockExclusive(int(f.Fd()), func(d time.Duration) {
		waits++
		if waits == 1 {
			probe.Close()
		}
	})
	if err != nil {
		t.Fatalf("acquire past a released probe = %v, want success", err)
	}
	if waits != 1 {
		t.Fatalf("waited %d times, want 1 (the first attempt must have contended)", waits)
	}
	mustBusy(t, root, true)
	mustContendExclusive(t, openGuard(t, root))
	f.Close()
	mustBusy(t, root, false)
}

// Contention that outlasts the retries refuses — after exactly
// acquireAttempts-1 waits — and leaves the holder holding.
func TestAcquireRefusesAPersistentHolder(t *testing.T) {
	for name, how := range map[string]int{"probe": syscall.LOCK_SH, "owner": syscall.LOCK_EX} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			holdGuard(t, root, how)
			f := openGuard(t, root)
			var waits []time.Duration
			err := lockExclusive(int(f.Fd()), func(d time.Duration) { waits = append(waits, d) })
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatalf("lockExclusive = %v, want EWOULDBLOCK", err)
			}
			if len(waits) != 4 {
				t.Fatalf("waits = %v, want 4", waits)
			}
			for _, d := range waits {
				if d != 10*time.Millisecond {
					t.Fatalf("waits = %v, want 10ms each", waits)
				}
			}
			if _, err := Acquire(root, "p", "r"); !IsBusy(err) {
				t.Fatalf("Acquire = %v, want busy", err)
			}
			mustBusy(t, root, how == syscall.LOCK_EX)
			mustContendExclusive(t, openGuard(t, root))
		})
	}
}

func TestLockExclusiveFastPaths(t *testing.T) {
	root := t.TempDir()
	holdGuard(t, root, syscall.LOCK_SH).Close()
	f := openGuard(t, root)
	if err := lockExclusive(int(f.Fd()), noSleep(t)); err != nil {
		t.Fatalf("uncontended lock = %v", err)
	}
	// A non-contention failure is returned at once, not retried.
	err := lockExclusive(-1, func(time.Duration) { t.Fatal("non-contention error was retried") })
	if !errors.Is(err, syscall.EBADF) {
		t.Fatalf("lockExclusive(-1) = %v, want EBADF", err)
	}
}

// A probe leaves nothing behind: a missing guard stays missing, and an
// existing one is immediately acquirable after Busy returns.
func TestProbeLeavesNoLock(t *testing.T) {
	root := t.TempDir()
	mustBusy(t, root, false)
	if _, err := os.Stat(Path(root, "p", "r")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe created the guard: %v", err)
	}
	holdGuard(t, root, syscall.LOCK_SH).Close()
	mustBusy(t, root, false)
	if err := lockExclusive(int(openGuard(t, root).Fd()), noSleep(t)); err != nil {
		t.Fatalf("acquire right after a probe = %v", err)
	}
}
