//go:build unix

package codex

import (
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
	"github.com/modulecollective/moe/internal/run"
)

const (
	chatID   = "01a0d4ca-9f99-7072-bf27-8df65cd9edc7"
	pulseID  = "01a0d4ed-8ae9-70c0-ae72-ab51e8868613"
	secondID = "01a0d4ed-8ae9-70c0-ae72-ab51e8868614"
)

func rolloutMeta(id, cwd, source string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":%q}}`+"\n", id, cwd, source)
}

func TestDiscoverSessionIDOwnedNewRollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd := t.TempDir()
	foreign := t.TempDir()
	old := writeFakeRollout(t, home, "2026/09/23", secondID, rolloutMeta(secondID, cwd, "cli"))
	before, err := rolloutSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// A pre-existing local session can be touched, and a foreign session
	// can be newer; neither owns the new invocation.
	if err := os.Chtimes(old, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	writeFakeRollout(t, home, "2026/09/24", chatID, rolloutMeta(chatID, cwd, "cli"))
	pulse := writeFakeRollout(t, home, "2026/09/25", pulseID, rolloutMeta(pulseID, foreign, "cli"))
	if err := os.Chtimes(pulse, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := discoverSessionID(before, cwd)
	if err != nil || got != chatID {
		t.Fatalf("got %q, %v; want %s", got, err, chatID)
	}
}

func TestDiscoverSessionIDRejectsUnsafeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bodies func(cwd, foreign string) []struct{ id, body string }
	}{
		{"foreign only", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{pulseID, rolloutMeta(pulseID, foreign, "cli")}}
		}},
		{"ambiguous", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, rolloutMeta(chatID, cwd, "cli")}, {secondID, rolloutMeta(secondID, cwd, "cli")}}
		}},
		{"exec source", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, rolloutMeta(chatID, cwd, "exec")}}
		}},
		{"subagent source", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, rolloutMeta(chatID, cwd, "subagent")}}
		}},
		{"missing metadata", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, `{"type":"session_meta","payload":{"id":"x"}}`}}
		}},
		{"malformed metadata", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, "{"}}
		}},
		{"ID mismatch", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{chatID, rolloutMeta(pulseID, cwd, "cli")}}
		}},
		{"invalid ID", func(cwd, foreign string) []struct{ id, body string } {
			return []struct{ id, body string }{{"not-a-uuid", rolloutMeta("not-a-uuid", cwd, "cli")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			cwd, foreign := t.TempDir(), t.TempDir()
			before, err := rolloutSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			for i, file := range tc.bodies(cwd, foreign) {
				writeFakeRollout(t, home, fmt.Sprintf("2026/09/%02d", i+1), file.id, file.body)
			}
			got, err := discoverSessionID(before, cwd)
			if got != "" || err == nil {
				t.Fatalf("got %q, %v; want empty ID and error", got, err)
			}
		})
	}
}

func TestDiscoverSessionIDLargeMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd := t.TempDir()
	before, err := rolloutSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(rolloutMeta(chatID, cwd, "cli"), "\n")
	body = strings.TrimSuffix(body, "}}") + fmt.Sprintf(`,"prompt":%q}}`, strings.Repeat("x", 128*1024)) + "\n"
	writeFakeRollout(t, home, "2026/09/24", chatID, body)
	got, err := discoverSessionID(before, cwd)
	if err != nil || got != chatID {
		t.Fatalf("got %q, %v; want %s", got, err, chatID)
	}
}

func TestRolloutSnapshotRejectsInvalidSessionsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "sessions"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rolloutSnapshot(); err == nil {
		t.Fatal("expected a sessions directory error")
	}
}

func TestExecuteMirrorsOwnedInteractiveSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	root := t.TempDir()
	foreign := t.TempDir()
	chatBody := rolloutMeta(chatID, root, "cli") + "chat\n"
	pulseBody := rolloutMeta(pulseID, foreign, "exec") + "pulse\n"
	script := fmt.Sprintf("#!/bin/sh\ncat > %q <<'EOF'\n%sEOF\ncat > %q <<'EOF'\n%sEOF\n",
		rolloutDest(t, home, "2026/09/24", chatID), chatBody,
		rolloutDest(t, home, "2026/09/25", pulseID), pulseBody)
	script += fmt.Sprintf("touch -d '2030-01-01 00:00:00 UTC' %q\n", rolloutDest(t, home, "2026/09/25", pulseID))
	fakeCodexOnPath(t, script)
	md := &run.Metadata{Project: "p", ID: "r"}
	sid, err := (Agent{}).Execute(agent.Request{Root: root, Metadata: md, DocID: "chat", SessionID: secondID, NewSession: true, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil || sid != chatID {
		t.Fatalf("got %q, %v; want %s", sid, err, chatID)
	}
	mirror := filepath.Join(root, run.ThreadPathFor("codex", "p", "r", "chat"))
	got, err := os.ReadFile(mirror)
	if err != nil || string(got) != chatBody {
		t.Fatalf("mirror = %q, %v; want chat rollout", got, err)
	}
}

func TestExecuteDiscoveryFailurePreservesMirrorAndChildError(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		exit        int
	}{
		{"foreign only", "", 0},
		{"ambiguous", "local", 0},
		{"child failure", "", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			root, foreign := t.TempDir(), t.TempDir()
			md := &run.Metadata{Project: "p", ID: "r"}
			mirror := filepath.Join(root, run.ThreadPathFor("codex", "p", "r", "chat"))
			if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mirror, []byte("previous"), 0o644); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("#!/bin/sh\ncat > %q <<'EOF'\n%sEOF\n",
				rolloutDest(t, home, "2026/09/24", pulseID), rolloutMeta(pulseID, foreign, "cli"))
			if tc.extra != "" {
				script += fmt.Sprintf("cat > %q <<'EOF'\n%sEOF\n", rolloutDest(t, home, "2026/09/25", chatID), rolloutMeta(chatID, root, "cli"))
				script += fmt.Sprintf("cat > %q <<'EOF'\n%sEOF\n", rolloutDest(t, home, "2026/09/26", secondID), rolloutMeta(secondID, root, "cli"))
			}
			script += fmt.Sprintf("exit %d\n", tc.exit)
			fakeCodexOnPath(t, script)
			sid, err := (Agent{}).Execute(agent.Request{Root: root, Metadata: md, DocID: "chat", SessionID: secondID, NewSession: true, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
			if sid != "" || err == nil {
				t.Fatalf("got %q, %v; want empty ID and error", sid, err)
			}
			if tc.exit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
					t.Fatalf("child error lost: %v", err)
				}
			}
			got, readErr := os.ReadFile(mirror)
			if readErr != nil || string(got) != "previous" {
				t.Fatalf("mirror = %q, %v; want previous bytes", got, readErr)
			}
		})
	}
}

func TestExecuteResumeBypassesDiscovery(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeCodexOnPath(t, "#!/bin/sh\nexit 0\n")
	sid, err := (Agent{}).Execute(agent.Request{Root: t.TempDir(), SessionID: chatID, NewSession: false, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil || sid != chatID {
		t.Fatalf("got %q, %v; want resume ID", sid, err)
	}
}

func rolloutDest(t *testing.T, home, shard, sid string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", shard)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "rollout-2026-09-24T19-00-44-"+sid+".jsonl")
}
