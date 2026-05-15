package cli

import (
	"os"
	"path/filepath"
	"testing"
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
