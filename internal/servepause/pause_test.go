package servepause

import (
	"bytes"
	"errors"
	"github.com/modulecollective/moe/internal/git/gittest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransactionRestoresBytesAndIndex(t *testing.T) {
	for _, initial := range []string{"missing", "tracked", "untracked"} {
		t.Run(initial, func(t *testing.T) {
			root := gittest.InitSeeded(t)
			before := "{\"revision\":7,\"expires_at\":\"2000-01-01T00:00:00Z\"}\n"
			if initial != "missing" {
				if err := os.WriteFile(filepath.Join(root, FileName), []byte(before), 0o644); err != nil {
					t.Fatal(err)
				}
				if initial == "tracked" {
					gittest.Commit(t, root, "seed default")
				}
			}
			gittest.WriteAndCommit(t, root, "unrelated", "keep", "seed unrelated")
			if err := os.WriteFile(filepath.Join(root, "scratch"), []byte("keep too"), 0o644); err != nil {
				t.Fatal(err)
			}
			hooks := t.TempDir()
			if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "config", "core.hooksPath", hooks)
			head := gittest.HeadSHA(t, root)
			index := gittest.Output(t, root, "ls-files", "-s")
			_, err := Set(root, true, nil, time.Now)
			if err == nil || !strings.Contains(err.Error(), "nothing recorded") {
				t.Fatalf("failure: %v", err)
			}
			if gittest.HeadSHA(t, root) != head || gittest.Output(t, root, "ls-files", "-s") != index {
				t.Fatal("HEAD/index changed")
			}
			body, e := os.ReadFile(filepath.Join(root, FileName))
			if initial == "missing" {
				if !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("failed file remains: %q %v", body, e)
				}
			} else if e != nil || string(body) != before {
				t.Fatalf("record changed: %q %v", body, e)
			}
			for path, want := range map[string]string{"unrelated": "keep", "scratch": "keep too"} {
				b, e := os.ReadFile(filepath.Join(root, path))
				if e != nil || string(b) != want {
					t.Fatalf("%s changed", path)
				}
			}
			gittest.Run(t, root, "config", "--unset", "core.hooksPath")
			if _, err := Set(root, true, nil, time.Now); err != nil {
				t.Fatalf("retry cannot enter: %v", err)
			}
		})
	}
}

func TestDirtyTreeRefuses(t *testing.T) {
	root := gittest.InitSeeded(t)
	gittest.WriteAndCommit(t, root, "unrelated", "original", "seed")
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gittest.HeadSHA(t, root)
	if _, err := Set(root, true, nil, time.Now); err == nil {
		t.Fatal("accepted dirty tree")
	}
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("refusal committed")
	}
}

func TestConcurrentControlsAreIdempotent(t *testing.T) {
	root := gittest.InitSeeded(t)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Go(func() { revision := 0; _, err := Set(root, true, &revision, time.Now); outcomes <- err })
	}
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("no writer succeeded")
	}
	state, err := Read(root)
	if err != nil || state.Revision != 1 {
		t.Fatalf("concurrent pauses: %+v %v", state, err)
	}
}

func TestStagingFailureRestoresUntrackedRecord(t *testing.T) {
	root := gittest.InitSeeded(t)
	gittest.WriteAndCommit(t, root, ".gitignore", FileName+"\n", "ignore setting to induce add failure")
	before := []byte("{\"revision\":8}\n")
	if err := os.WriteFile(filepath.Join(root, FileName), before, 0o644); err != nil {
		t.Fatal(err)
	}
	head := gittest.HeadSHA(t, root)
	if _, err := Set(root, true, nil, time.Now); err == nil || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("add failure: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil || string(after) != string(before) || gittest.HeadSHA(t, root) != head || gittest.Output(t, root, "diff", "--cached") != "" {
		t.Fatal("add failure left effective/staged changes")
	}
}

func TestRollbackFailureIsReported(t *testing.T) {
	root := gittest.InitSeeded(t)
	hooks := t.TempDir()
	// This is confined to the fixture: replacing our new file with a directory
	// makes restoring absence fail after the hook refuses the commit.
	body := "#!/bin/sh\nrm serve-pause.json\nmkdir serve-pause.json\ntouch serve-pause.json/keep\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	if _, err := Set(root, true, nil, time.Now); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback error hidden: %v", err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("rollback residue claimed configured state")
	}
}

func TestLifetimeStaleRequestsAndRestart(t *testing.T) {
	root := gittest.InitSeeded(t)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	state, err := Read(root)
	if err != nil || state.Revision != 0 || state.Active(now) {
		t.Fatalf("missing: %+v %v", state, err)
	}
	head := gittest.HeadSHA(t, root)
	if _, err := Set(root, false, nil, clock); err != nil || gittest.HeadSHA(t, root) != head {
		t.Fatalf("empty unpause: %v", err)
	}
	state, err = Set(root, true, nil, clock)
	if err != nil || state.Revision != 1 || !state.ExpiresAt.Equal(now.Add(Lifetime)) {
		t.Fatalf("pause: %+v %v", state, err)
	}
	expiry := state.ExpiresAt
	if err := Check(root, clock); err == nil {
		t.Fatal("admitted paused sweep")
	}
	head = gittest.HeadSHA(t, root)
	stale := 0
	now = now.Add(time.Hour)
	state, err = Set(root, true, &stale, clock)
	if err != nil || !state.ExpiresAt.Equal(expiry) || gittest.HeadSHA(t, root) != head {
		t.Fatalf("duplicate: %+v %v", state, err)
	}
	if _, err := Set(root, false, &stale, clock); !errors.Is(err, ErrStale) {
		t.Fatalf("stale opposing request: %v", err)
	}
	if !state.Active(expiry.Add(-time.Nanosecond)) || state.Active(expiry) {
		t.Fatal("expiry is not strict")
	}
	now = expiry
	state, err = Read(root)
	if err != nil || state.Active(now) || gittest.HeadSHA(t, root) != head {
		t.Fatalf("restart/expiry: %+v %v", state, err)
	}
	if err := Check(root, clock); err != nil {
		t.Fatal(err)
	}
	state, err = Set(root, true, nil, clock)
	if err != nil || state.Revision != 2 || !state.ExpiresAt.Equal(now.Add(Lifetime)) {
		t.Fatalf("fresh pause: %+v %v", state, err)
	}
	state, err = Set(root, false, nil, clock)
	if err != nil || state.Revision != 3 || !state.ExpiresAt.IsZero() {
		t.Fatalf("unpause: %+v %v", state, err)
	}
	head = gittest.HeadSHA(t, root)
	if _, err := Set(root, false, &stale, clock); err != nil || gittest.HeadSHA(t, root) != head {
		t.Fatalf("duplicate unpause: %v", err)
	}
	// Expired records may be explicitly cleared once.
	if _, err := Set(root, true, nil, clock); err != nil {
		t.Fatal(err)
	}
	now = now.Add(Lifetime)
	state, err = Set(root, false, nil, clock)
	if err != nil || state.Revision != 5 || !state.ExpiresAt.IsZero() {
		t.Fatalf("clear expired: %+v %v", state, err)
	}
}

func TestMalformedHoldsAdmission(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{}", `{"revision":0}`, `{"revision":-1}`, `{"revision":1.5}`, `{"revision":1,"extra":true}`, `{"revision":1} {}`, `{"revision":1,"expires_at":null}`, `{"revision":null}`, `{"revision":1,"expires_at":"bad"}`, `{"revision":1,"expires_at":"0001-01-01T00:00:00Z"}`, `{"revision":1,"expires_at":"2026-11-25T12:00:00-08:00"}`} {
		t.Run(body, func(t *testing.T) {
			root := gittest.InitSeeded(t)
			if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(root); err == nil {
				t.Fatalf("accepted %q", body)
			}
			if err := Check(root, time.Now); err == nil {
				t.Fatal("malformed state admitted sweep")
			}
			if _, err := Set(root, false, nil, time.Now); err == nil {
				t.Fatal("malformed state silently unpaused")
			}
		})
	}
	root := gittest.InitSeeded(t)
	if err := os.Mkdir(filepath.Join(root, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Check(root, time.Now); err == nil {
		t.Fatal("unreadable state admitted sweep")
	}
}

// A real writer process keeps the lock while its commit hook exposes an
// uncommitted unpause. Admission must see the restored pause after rollback.
func TestAdmissionSerializesWithFailedUnpause(t *testing.T) {
	root := gittest.InitSeeded(t)
	if _, err := Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	hook := "#!/bin/sh\ntouch '" + entered + "'\ni=0\nwhile [ ! -e '" + release + "' ] && [ $i -lt 500 ]; do sleep 0.01; i=$((i+1)); done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", dir)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFailedUnpauseWriterProcess$")
	cmd.Env = append(os.Environ(), "MOE_PAUSE_WRITER_ROOT="+root)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		if !joined {
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not reach commit hook")
		}
		time.Sleep(time.Millisecond)
	}
	state, err := Read(root)
	if err != nil || !state.ExpiresAt.IsZero() {
		t.Fatalf("fixture did not expose transient unpause: %+v %v", state, err)
	}
	// The writer stays blocked until this attempt has begun. In the broken
	// lock-free implementation this returns nil from the transient record.
	timer := time.AfterFunc(150*time.Millisecond, func() { _ = os.WriteFile(release, nil, 0o644) })
	defer timer.Stop()
	if err := Check(root, time.Now); err == nil || !strings.Contains(err.Error(), "paused until") {
		t.Fatalf("admission saw uncommitted unpause: %v", err)
	}
	err = cmd.Wait()
	joined = true
	if err != nil {
		t.Fatalf("writer: %v %s", err, &output)
	}
	state, err = Read(root)
	if err != nil || !state.Active(time.Now()) || state.Revision != 1 {
		t.Fatalf("rollback: %+v %v", state, err)
	}
}

func TestFailedUnpauseWriterProcess(t *testing.T) {
	root := os.Getenv("MOE_PAUSE_WRITER_ROOT")
	if root == "" {
		return
	}
	if _, err := Set(root, false, nil, time.Now); err == nil || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("expected commit refusal: %v", err)
	}
}
