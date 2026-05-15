package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestResolveAgentNamePrecedence pins the ladder design.md describes:
// explicit → run.json.Agent → $MOE_AGENT → config.default_agent →
// "claude". The new step is the config layer between env and hard
// default — easy to regress, hence the table here.
func TestResolveAgentNamePrecedence(t *testing.T) {
	type setup struct {
		// withConfig, when non-empty, lays a .moe/config.json with
		// {"default_agent": <value>}. Empty means no file.
		withConfig string
		// withEnv is the MOE_AGENT value to set on the test process.
		// Empty means unset.
		withEnv string
	}
	cases := []struct {
		name       string
		explicit   string
		runDefault string
		setup      setup
		want       string
	}{
		{
			name:     "explicit wins over everything",
			explicit: "codex",
			setup:    setup{withConfig: "claude", withEnv: "claude"},
			want:     "codex",
		},
		{
			name:       "runDefault wins over env + config",
			runDefault: "codex",
			setup:      setup{withConfig: "claude", withEnv: "claude"},
			want:       "codex",
		},
		{
			name:  "env wins over config",
			setup: setup{withConfig: "claude", withEnv: "codex"},
			want:  "codex",
		},
		{
			name:  "config used when nothing else set",
			setup: setup{withConfig: "codex"},
			want:  "codex",
		},
		{
			name: "hard default kicks in when nothing else set",
			want: "claude",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if c.setup.withConfig != "" {
				if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
					t.Fatal(err)
				}
				body := `{"default_agent": "` + c.setup.withConfig + `"}`
				if err := os.WriteFile(filepath.Join(root, ".moe", "config.json"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// t.Setenv always sets the var; explicit empty is the
			// closest stand-in for "unset" without unsetting whatever
			// the host shell injected.
			t.Setenv("MOE_AGENT", c.setup.withEnv)
			if got := resolveAgentName(c.explicit, c.runDefault, root); got != c.want {
				t.Fatalf("resolveAgentName(%q, %q, root) = %q, want %q",
					c.explicit, c.runDefault, got, c.want)
			}
		})
	}
}

// TestResolveAgentNameEmptyRoot covers the test/fallback path where
// no bureaucracy is available: the function must still resolve
// without panicking, falling through to env / "claude".
func TestResolveAgentNameEmptyRoot(t *testing.T) {
	t.Setenv("MOE_AGENT", "")
	if got := resolveAgentName("", "", ""); got != "claude" {
		t.Fatalf("empty root, no env: got %q, want claude", got)
	}
	t.Setenv("MOE_AGENT", "codex")
	if got := resolveAgentName("", "", ""); got != "codex" {
		t.Fatalf("empty root, MOE_AGENT=codex: got %q, want codex", got)
	}
}

// TestStageAgentNameUsesCanonicalRoot pins the call-site contract
// that resolveAgentName's unit tests don't reach: the production
// stage call sites must hand stageAgentName the canonical bureaucracy
// root (the dir holding .moe/config.json), not the session worktree
// (which has no .moe/ of its own). The first pass of this run
// shipped the wrong variable at all three call sites, and the unit
// tests above passed because they build a tempdir and pass it
// directly — they can't catch "the caller picked the wrong dir."
//
// Failure mode this regression catches: when a caller hands in a
// path with no .moe/ at it, config.Read silently returns an empty
// Config{} and the helper drops through to "claude". The operator's
// configured default_agent is ignored. The test asserts both halves:
// canonical root resolves the configured value; a worktree-shaped
// path falls through to the hard default.
func TestStageAgentNameUsesCanonicalRoot(t *testing.T) {
	root := t.TempDir()
	worktree := t.TempDir() // simulate a session worktree: no .moe/
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".moe", "config.json"), []byte(`{"default_agent": "codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_AGENT", "")

	md := &run.Metadata{}
	opts := stageSessionOpts{}

	if got := stageAgentName(opts, md, root); got != "codex" {
		t.Fatalf("canonical root: got %q, want codex (config should resolve)", got)
	}
	if got := stageAgentName(opts, md, worktree); got != "claude" {
		t.Fatalf("worktree path: got %q, want claude (no .moe/ at worktree — config silently misses)", got)
	}
}

// TestStageAgentNameNilMetadata covers the early-bootstrap path
// where md hasn't been loaded yet. The helper must not deref a nil
// metadata pointer, and the run-default rung of the ladder is
// simply skipped.
func TestStageAgentNameNilMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".moe", "config.json"), []byte(`{"default_agent": "codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_AGENT", "")
	if got := stageAgentName(stageSessionOpts{}, nil, root); got != "codex" {
		t.Fatalf("nil md: got %q, want codex", got)
	}
}

// TestResolveAgentNameIgnoresBrokenConfig: a malformed config.json
// shouldn't ground stage dispatch — fall through to the hard default
// rather than wedge. The operator sees the parse error from `moe
// config list` at the surface where they can act on it.
func TestResolveAgentNameIgnoresBrokenConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".moe", "config.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_AGENT", "")
	if got := resolveAgentName("", "", root); got != "claude" {
		t.Fatalf("broken config: got %q, want claude", got)
	}
}
