package claude

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// writeClaudeConfig plants .claude.json under a scratch
// CLAUDE_CONFIG_DIR and returns its path.
func writeClaudeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTrust(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Projects
}

func configWith(projects map[string]map[string]any, extra map[string]any) string {
	cfg := map[string]any{"projects": projects}
	maps.Copy(cfg, extra)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

// The clone and every AddDir get a trust record when the bureaucracy
// is trusted; unrelated keys and the existing entries survive with
// their values intact.
func TestSeedTrustStampsWhenBureaucracyTrusted(t *testing.T) {
	root := t.TempDir()
	path := writeClaudeConfig(t, configWith(map[string]map[string]any{
		root:         {"hasTrustDialogAccepted": true, "allowedTools": []any{"Bash(<x> & y)"}},
		"/elsewhere": {"hasTrustDialogAccepted": false, "lastCost": 1.5},
	}, map[string]any{"numStartups": 42, "oauthAccount": map[string]any{"email": "a<b>&c"}}))
	before, _ := os.ReadFile(path)

	if err := seedTrust(root, []string{"/clone/run", "", "/scratch/home", "/clone/run/"}); err != nil {
		t.Fatal(err)
	}

	got := readTrust(t, path)
	for _, d := range []string{"/clone/run", "/scratch/home"} {
		if got[d]["hasTrustDialogAccepted"] != true {
			t.Errorf("projects[%s] = %v, want trusted", d, got[d])
		}
	}
	if len(got) != 4 {
		t.Errorf("projects has %d entries, want 4: %v", len(got), got)
	}

	var was, now map[string]any
	_ = json.Unmarshal(before, &was)
	after, _ := os.ReadFile(path)
	_ = json.Unmarshal(after, &now)
	for _, k := range []string{"numStartups", "oauthAccount"} {
		if b1, b2 := mustJSON(t, was[k]), mustJSON(t, now[k]); b1 != b2 {
			t.Errorf("%s changed: %s -> %s", k, b1, b2)
		}
	}
	wp, np := was["projects"].(map[string]any), now["projects"].(map[string]any)
	for _, k := range []string{root, "/elsewhere"} {
		if b1, b2 := mustJSON(t, wp[k]), mustJSON(t, np[k]); b1 != b2 {
			t.Errorf("projects[%s] changed: %s -> %s", k, b1, b2)
		}
	}
	if strings.Contains(string(after), `<`) {
		t.Errorf("rewrite HTML-escaped a value:\n%s", after)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// An existing project entry keeps its other fields when stamped.
func TestSeedTrustKeepsExistingEntryFields(t *testing.T) {
	root := t.TempDir()
	path := writeClaudeConfig(t, configWith(map[string]map[string]any{
		root:         {"hasTrustDialogAccepted": true},
		"/clone/run": {"hasTrustDialogAccepted": false, "allowedTools": []any{"Read"}},
	}, nil))

	if err := seedTrust(root, []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	e := readTrust(t, path)["/clone/run"]
	if e["hasTrustDialogAccepted"] != true || mustJSON(t, e["allowedTools"]) != `["Read"]` {
		t.Errorf("projects[/clone/run] = %v", e)
	}
}

// Without trust on the bureaucracy nothing is written — not even when
// an ancestor of everything is trusted, which Claude does not count
// for a path inside a repository.
func TestSeedTrustWritesNothingWhenBureaucracyUntrusted(t *testing.T) {
	root := t.TempDir()
	body := configWith(map[string]map[string]any{
		filepath.Dir(root): {"hasTrustDialogAccepted": true},
		root:               {"hasTrustDialogAccepted": false},
	}, nil)
	path := writeClaudeConfig(t, body)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if err := seedTrust(root, []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, path, body, old)
}

// Every dir already stamped: no rewrite.
func TestSeedTrustNoopWhenAlreadyStamped(t *testing.T) {
	root := t.TempDir()
	body := configWith(map[string]map[string]any{
		root:         {"hasTrustDialogAccepted": true},
		"/clone/run": {"hasTrustDialogAccepted": true},
	}, nil)
	path := writeClaudeConfig(t, body)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if err := seedTrust(root, []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, path, body, old)
}

// No config file: Claude has never run here, so nothing to derive from
// and nothing created.
func TestSeedTrustMissingConfigDoesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	if err := seedTrust(t.TempDir(), []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("config dir not empty: %v", entries)
	}
}

// The bureaucracy root MoE hands the executor is a session worktree;
// Claude keys a worktree's trust by its main repository, so trust on
// the main root is what counts.
func TestSeedTrustFollowsWorktreeToMainRepository(t *testing.T) {
	main := gittest.InitSeeded(t)
	wt := filepath.Join(t.TempDir(), "session")
	gittest.Run(t, main, "worktree", "add", "-q", "--detach", wt)
	mainReal, err := filepath.EvalSymlinks(main)
	if err != nil {
		t.Fatal(err)
	}
	path := writeClaudeConfig(t, configWith(map[string]map[string]any{
		mainReal: {"hasTrustDialogAccepted": true},
	}, nil))

	if err := seedTrust(wt, []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	if readTrust(t, path)["/clone/run"]["hasTrustDialogAccepted"] != true {
		t.Errorf("clone not stamped via the worktree's main repository")
	}
}

// A live Claude writer holding the config lock past lockWait makes the
// seed give up with an error and leave the file alone; a lock older
// than the staleness window is a dead writer's and is taken over.
func TestSeedTrustHonoursConfigLock(t *testing.T) {
	root := t.TempDir()
	body := configWith(map[string]map[string]any{root: {"hasTrustDialogAccepted": true}}, nil)
	path := writeClaudeConfig(t, body)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	prev := lockWait
	lockWait = 100 * time.Millisecond
	t.Cleanup(func() { lockWait = prev })

	lock := path + ".lock"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	err := seedTrust(root, []string{"/clone/run"})
	if err == nil || !strings.Contains(err.Error(), "held by another writer") {
		t.Fatalf("err = %v, want held-lock error", err)
	}
	assertUntouched(t, path, body, old)

	stale := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := seedTrust(root, []string{"/clone/run"}); err != nil {
		t.Fatal(err)
	}
	if readTrust(t, path)["/clone/run"]["hasTrustDialogAccepted"] != true {
		t.Errorf("stale lock not taken over")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

func assertUntouched(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("config rewritten:\n%s", got)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(mtime) {
		t.Errorf("config mtime moved: %v", info.ModTime())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The interactive launch stamps the clone and AddDirs before claude
// starts: the fake CLI snapshots the config it would read at startup.
// The headless launch shows no dialog and leaves the config alone.
func TestExecuteSeedsTrustBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	body := configWith(map[string]map[string]any{root: {"hasTrustDialogAccepted": true}}, nil)
	path := writeClaudeConfig(t, body)
	snap := filepath.Join(t.TempDir(), "at-launch.json")
	fakeClaudeOnPath(t, "#!/bin/sh\ncp \"$CLAUDE_CONFIG_DIR/.claude.json\" "+snap+"\n")

	if _, err := (Agent{}).ExecuteOneShot(agent.OneShotRequest{
		Root: root, ClonePath: "/clone/run", Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Fatalf("headless launch rewrote the config:\n%s", got)
	}

	var stderr strings.Builder
	if _, err := (Agent{}).Execute(agent.Request{
		Root: root, SessionID: "s", NewSession: true, ClonePath: "/clone/run",
		AddDirs: []string{"/scratch/home"}, Stdin: strings.NewReader(""),
		Stdout: io.Discard, Stderr: &stderr,
	}); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	got := readTrust(t, snap)
	for _, d := range []string{"/clone/run", "/scratch/home"} {
		if got[d]["hasTrustDialogAccepted"] != true {
			t.Errorf("at launch projects[%s] = %v, want trusted", d, got[d])
		}
	}
}
