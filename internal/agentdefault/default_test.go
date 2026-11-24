package agentdefault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/modulecollective/moe/internal/agent/claude"
	_ "github.com/modulecollective/moe/internal/agent/codex"
	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestStateLifetimeAndRevisions(t *testing.T) {
	root := gittest.InitSeeded(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	state, err := Read(root)
	if err != nil || state.Revision != 0 || state.Active(now) != "" {
		t.Fatalf("missing: %+v %v", state, err)
	}
	head := gittest.HeadSHA(t, root)
	state, err = Set(root, "", nil, clock)
	if err != nil || gittest.HeadSHA(t, root) != head {
		t.Fatalf("empty reset: %v", err)
	}
	state, err = Set(root, "codex", nil, clock)
	if err != nil || state.Revision != 1 || state.Active(now) != "codex" || !state.ExpiresAt.Equal(now.Add(Lifetime)) {
		t.Fatalf("set: %+v %v", state, err)
	}
	expiry := state.ExpiresAt
	head = gittest.HeadSHA(t, root)
	now = now.Add(time.Hour)
	stale := 0
	state, err = Set(root, "codex", &stale, clock)
	if err != nil || state.ExpiresAt != expiry || gittest.HeadSHA(t, root) != head {
		t.Fatalf("duplicate: %+v %v", state, err)
	}
	if _, err = Set(root, "claude", &stale, clock); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	state, err = Set(root, "claude", nil, clock)
	if err != nil || state.Revision != 2 || !state.ExpiresAt.Equal(now.Add(Lifetime)) {
		t.Fatalf("replace: %+v %v", state, err)
	}
	if state.Active(state.ExpiresAt.Add(-time.Nanosecond)) != "claude" || state.Active(state.ExpiresAt) != "" {
		t.Fatal("expiry is not strict")
	}
	now = state.ExpiresAt
	state, err = Read(root)
	if err != nil || state.Active(now) != "" {
		t.Fatalf("expired restart: %+v %v", state, err)
	}
	state, err = Set(root, "claude", nil, clock)
	if err != nil || state.Revision != 3 || state.Active(now) != "claude" {
		t.Fatalf("fresh window: %+v %v", state, err)
	}
	state, err = Set(root, "", nil, clock)
	if err != nil || state.Revision != 4 || state.Agent != "" || !state.ExpiresAt.IsZero() {
		t.Fatalf("reset: %+v %v", state, err)
	}
	head = gittest.HeadSHA(t, root)
	if _, err := Set(root, "", &stale, clock); err != nil || gittest.HeadSHA(t, root) != head {
		t.Fatalf("duplicate reset: %v", err)
	}
}

func TestReadRejectsMalformedAndUnknown(t *testing.T) {
	for _, body := range []string{"", "null", "[]", "{}", `{"revision":0}`, `{"revision":-1}`, `{"revision":1,"agent":"codex"}`, `{"revision":1,"expires_at":"2026-11-25T00:00:00Z"}`, `{"revision":1,"agent":"missing","expires_at":"2026-11-25T00:00:00Z"}`, `{"revision":1,"extra":true}`, `{"revision":1} {}`, `{"revision":1,"expires_at":"bad"}`} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(root); err == nil {
				t.Fatalf("accepted %q", body)
			}
		})
	}
}

func TestTransactionRestoresBytesAndIndex(t *testing.T) {
	for _, initial := range []string{"missing", "tracked", "untracked"} {
		t.Run(initial, func(t *testing.T) {
			root := gittest.InitSeeded(t)
			before := "{\"revision\":7,\"agent\":\"claude\",\"expires_at\":\"2000-01-01T00:00:00Z\"}\n"
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
			_, err := Set(root, "codex", nil, time.Now)
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
			if _, err := Set(root, "codex", nil, time.Now); err != nil {
				t.Fatalf("retry cannot enter: %v", err)
			}
		})
	}
}

func TestDirtyTreeAndInvalidBackendRefuse(t *testing.T) {
	root := gittest.InitSeeded(t)
	gittest.WriteAndCommit(t, root, "unrelated", "original", "seed")
	if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gittest.HeadSHA(t, root)
	if _, err := Set(root, "codex", nil, time.Now); err == nil {
		t.Fatal("accepted dirty tree")
	}
	if _, err := Set(root, "unknown", nil, time.Now); err == nil {
		t.Fatal("accepted unknown backend")
	}
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("refusal committed")
	}
}

func TestConcurrentControlsCannotOverwriteNewerChoice(t *testing.T) {
	root := gittest.InitSeeded(t)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, name := range []string{"claude", "codex"} {
		wg.Go(func() { revision := 0; _, err := Set(root, name, &revision, time.Now); outcomes <- err })
	}
	wg.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		}
	}
	state, err := Read(root)
	if successes != 1 || err != nil || state.Revision != 1 {
		t.Fatalf("successes=%d state=%+v err=%v", successes, state, err)
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
	if _, err := Set(root, "codex", nil, time.Now); err == nil || !strings.Contains(err.Error(), "nothing recorded") {
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
	body := "#!/bin/sh\nrm agent-default.json\nmkdir agent-default.json\ntouch agent-default.json/keep\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	if _, err := Set(root, "codex", nil, time.Now); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback error hidden: %v", err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("rollback residue claimed configured state")
	}
}
