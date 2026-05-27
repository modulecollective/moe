package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClaudeProjectsDirEncoding pins the / → -, . → -, leading-dash
// substitution against the two example paths the design names. The
// `/.` → `--` double-dash case is the one most likely to break under
// a naive implementation.
func TestClaudeProjectsDirEncoding(t *testing.T) {
	t.Setenv("HOME", "/tmp/snag-test-home")
	cases := []struct {
		cwd  string
		want string
	}{
		{
			cwd:  "/home/dev/work/bureaucracy",
			want: "/tmp/snag-test-home/.claude/projects/-home-dev-work-bureaucracy",
		},
		{
			cwd:  "/home/dev/work/bureaucracy/.moe/worktrees/abc-123",
			want: "/tmp/snag-test-home/.claude/projects/-home-dev-work-bureaucracy--moe-worktrees-abc-123",
		},
	}
	for _, c := range cases {
		got := claudeProjectsDir(c.cwd)
		if got != c.want {
			t.Errorf("claudeProjectsDir(%q):\n  got:  %q\n  want: %q", c.cwd, got, c.want)
		}
	}
}

// TestSnagCopiesFreshTranscript covers the snag happy path: a
// JSONL written under a worktree-encoded claude projects dir whose
// mtime is after the child's `started` lands in
// `documents/design/transcripts/<uuid>.jsonl` under the run's
// canonical bureaucracy path.
func TestSnagCopiesFreshTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := t.TempDir()
	// Seed the source: ~/.claude/projects/<encoded worktree>/<sid>.jsonl
	worktreeCwd := filepath.Join(root, ".moe", "worktrees", "session-uuid")
	srcDir := claudeProjectsDir(worktreeCwd)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "claude-sid.jsonl")
	if err := os.WriteFile(srcFile, []byte(`{"event":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &child{
		id:      "alpha/my-run",
		started: time.Now().Add(-time.Minute),
	}
	c.snagTranscripts(root, io.Discard)

	dst := filepath.Join(root, "projects", "alpha", "runs", "my-run",
		"documents", "design", "transcripts", "claude-sid.jsonl")
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination not written: %v", err)
	}
	if !strings.Contains(string(body), `"event":"hi"`) {
		t.Errorf("destination contents wrong: %q", string(body))
	}
}

// TestSnagSkipsStaleTranscript: a JSONL whose mtime is *before* the
// child started belongs to a prior session and must not be copied.
func TestSnagSkipsStaleTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	worktreeCwd := filepath.Join(root, ".moe", "worktrees", "old-session")
	srcDir := claudeProjectsDir(worktreeCwd)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "stale.jsonl")
	if err := os.WriteFile(srcFile, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the file to before child.started.
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(srcFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	c := &child{
		id:      "alpha/my-run",
		started: time.Now().Add(-time.Hour), // after the file's mtime
	}
	c.snagTranscripts(root, io.Discard)

	dst := filepath.Join(root, "projects", "alpha", "runs", "my-run",
		"documents", "design", "transcripts", "stale.jsonl")
	if _, err := os.Stat(dst); err == nil {
		t.Error("stale jsonl should not have been copied")
	}
}

// TestSnagIgnoresUnrelatedProjectDirs: ~/.claude/projects/ on a real
// host carries dirs from every cwd claude was ever invoked under.
// Only entries matching this bureaucracy's worktree-encoded prefix
// should be touched.
func TestSnagIgnoresUnrelatedProjectDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	// Decoy: a project dir that *isn't* a worktree under this root.
	decoy := filepath.Join(home, ".claude", "projects", "-tmp-some-other-project")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "decoy.jsonl"), []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Real source: a matching worktree dir.
	worktreeCwd := filepath.Join(root, ".moe", "worktrees", "uuid")
	srcDir := claudeProjectsDir(worktreeCwd)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real.jsonl"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &child{
		id:      "alpha/my-run",
		started: time.Now().Add(-time.Minute),
	}
	c.snagTranscripts(root, io.Discard)

	destDir := filepath.Join(root, "projects", "alpha", "runs", "my-run",
		"documents", "design", "transcripts")
	if _, err := os.Stat(filepath.Join(destDir, "real.jsonl")); err != nil {
		t.Errorf("matching jsonl should have been copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "decoy.jsonl")); err == nil {
		t.Error("decoy jsonl must not be copied — prefix gate failed")
	}
}

// TestPerRunPageSurfacesTranscriptWhenSnagged: when a snagged jsonl
// lives under the canonical documents/<stage>/transcripts/ tree, the
// per-run page renders the on-disk path as the transcript affordance
// next to the canvas link.
func TestPerRunPageSurfacesTranscriptWhenSnagged(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-run", "sdlc")
	writeCanvas(t, root, "alpha", "my-run", "design", "# design\n")

	// Drop a snagged transcript jsonl in the canonical location.
	transcriptDir := filepath.Join(root, "projects", "alpha", "runs", "my-run",
		"documents", "design", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcriptDir, "sid.jsonl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(p, r, stage string) (string, error) {
			return filepath.Join(root, "projects", p, "runs", r,
				"documents", stage, "content.md"), nil
		},
		RunStages: func(_, _ string) ([]string, error) {
			return []string{"design", "code"}, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-run", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "transcript:") || !strings.Contains(body, transcriptDir) {
		t.Errorf("expected transcript link with on-disk path %q, got:\n%s", transcriptDir, body)
	}
}
