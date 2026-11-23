//go:build linux

package clonereclaim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/runowner"
)

func TestShellOwnerHelper(t *testing.T) {
	if os.Getenv("MOE_TEST_SHELL_PARENT") != "1" {
		return
	}
	root := os.Getenv("MOE_TEST_SHELL_ROOT")
	h, err := runowner.Acquire(root, "p", "r")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r, err := LoadRegistry(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code, err := r.SuperviseShell(h, os.Getenv("MOE_TEST_SHELL_BIN"), r.Identity.Path, os.Environ(), os.Stdin, os.Stdout, os.Stderr, func() {
		if err := os.WriteFile(os.Getenv("MOE_TEST_SHELL_WAITING"), []byte("waiting"), 0o600); err != nil {
			panic(err)
		}
	})
	r.Close()
	h.Release()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func waitUntil(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(until) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestShellDescendantLifetime(t *testing.T) {
	for _, mode := range []string{"normal", "parent-death", "supervisor-death"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := exec.LookPath("setsid"); err != nil {
				t.Fatal("Linux lifetime fixture requires setsid:", err)
			}
			h, path := registryFixture(t)
			r, err := Enroll(h)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			root := h.Root
			if err := h.Release(); err != nil {
				t.Fatal(err)
			}
			fixture := t.TempDir()
			release, ready, finished := filepath.Join(fixture, "release"), filepath.Join(fixture, "ready"), filepath.Join(fixture, "finished")
			waiting, supervisor, noGuard := filepath.Join(fixture, "waiting"), filepath.Join(fixture, "supervisor"), filepath.Join(fixture, "no-guard")
			shell := filepath.Join(fixture, "shell")
			script := `#!/bin/sh
printf '%s' "$PPID" > "$SUPERVISOR"
for f in /proc/$$/fd/*; do
  target=$(readlink "$f")
  if [ "$target" = "$LOCK" ]; then exit 99; fi
done
printf 'no inherited guard' > "$NO_GUARD"
setsid sh -c '
  (
    printf ready > "$READY"
    while ! [ -f "$RELEASE" ]; do sleep .01; done
    printf finished > "$FINISHED"
  ) </dev/null >/dev/null 2>&1 &
  exit 0
' &
exit 7
`
			if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(self, "-test.run=^TestShellOwnerHelper$")
			cmd.Env = append(os.Environ(), "MOE_TEST_SHELL_PARENT=1", "MOE_TEST_SHELL_ROOT="+root, "MOE_TEST_SHELL_BIN="+shell, "MOE_TEST_SHELL_WAITING="+waiting, "RELEASE="+release, "READY="+ready, "FINISHED="+finished, "SUPERVISOR="+supervisor, "NO_GUARD="+noGuard, "LOCK="+runowner.Path(root, "p", "r"))
			out, err := os.Create(filepath.Join(fixture, "output"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				os.WriteFile(release, []byte("stop"), 0o600)
				if !waited {
					cmd.Process.Kill()
					<-done
				}
				// Completion of the adopted job and release of the guard prove
				// that no fixture process remains alive after this test.
				if fileExists(ready) {
					waitUntil(t, func() bool { return fileExists(finished) })
				}
				waitUntil(t, func() bool { busy, err := runowner.Busy(root, "p", "r"); return err == nil && !busy })
			})
			waitUntil(t, func() bool { return fileExists(ready) && fileExists(waiting) })
			if mode == "normal" {
				select {
				case err := <-done:
					waited = true
					t.Fatalf("shell returned while its detached descendant still waits for release: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
			}
			if !fileExists(noGuard) {
				t.Fatal("user shell inherited guard")
			}
			if busy, err := runowner.Busy(root, "p", "r"); err != nil || !busy {
				t.Fatalf("background job is unowned: %v %v", busy, err)
			}
			if _, err := runowner.Acquire(root, "p", "r"); !runowner.IsBusy(err) {
				t.Fatalf("second owner entered: %v", err)
			}
			if mode == "parent-death" {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				<-done
				waited = true
				if busy, err := runowner.Busy(root, "p", "r"); err != nil || !busy {
					t.Fatalf("parent death released supervisor guard: %v %v", busy, err)
				}
			}
			if mode == "supervisor-death" {
				b, err := os.ReadFile(supervisor)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(b))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err == nil {
					t.Fatal("supervisor death reported success")
				}
				waited = true
			}
			if err := os.WriteFile(release, []byte("finish"), 0o600); err != nil {
				t.Fatal(err)
			}
			if !waited {
				err := <-done
				waited = true
				if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 7 {
					b, _ := os.ReadFile(out.Name())
					t.Fatalf("shell status not preserved: %v\n%s", err, b)
				}
			}
			waitUntil(t, func() bool { busy, err := runowner.Busy(root, "p", "r"); return err == nil && !busy })
			h, err = runowner.Acquire(root, "p", "r")
			if err != nil {
				t.Fatal(err)
			}
			defer h.Release()
			r, err = LoadRegistry(h)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			err = r.CheckShells(h)
			if mode == "supervisor-death" {
				if err == nil || !strings.Contains(err.Error(), "unresolved shell") {
					t.Fatalf("crash hold cleared: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !fileExists(filepath.Join(path, ".git", "HEAD")) {
				t.Fatal("shell lifetime changed clone contents")
			}
		})
	}
}

func TestShellStartPublicationFailurePreventsLaunch(t *testing.T) {
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	original := syncRecordFile
	defer func() { syncRecordFile = original }()
	injected := errors.New("injected sync failure")
	syncRecordFile = func(*os.File) error { return injected }
	if _, err := r.SuperviseShell(h, "/bin/sh", path, os.Environ(), nil, nil, nil, nil); !errors.Is(err, injected) {
		t.Fatalf("launch attempted without durable start: %v", err)
	}
}
