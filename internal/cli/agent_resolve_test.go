package cli

import (
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestResolveAgentNamePrecedence pins the ladder design.md describes:
// $MOE_FORCE_AGENT → explicit → run.json.Agent → stylesheet →
// "claude".
func TestResolveAgentNamePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		force      string
		explicit   string
		runDefault string
		stylesheet string
		want       string
	}{
		{
			name:     "force wins over explicit",
			force:    "claude",
			explicit: "codex",
			want:     "claude",
		},
		{
			name:       "force wins over runDefault",
			force:      "claude",
			runDefault: "codex",
			want:       "claude",
		},
		{
			name:       "force wins over stylesheet",
			force:      "claude",
			stylesheet: "codex",
			want:       "claude",
		},
		{
			name:     "explicit wins over everything below force",
			explicit: "codex",
			want:     "codex",
		},
		{
			name:       "runDefault wins over stylesheet",
			runDefault: "codex",
			stylesheet: "claude",
			want:       "codex",
		},
		{
			name:       "stylesheet used when bindings are empty",
			stylesheet: "codex",
			want:       "codex",
		},
		{
			name: "hard default kicks in when nothing else set",
			want: "claude",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Clearing MOE_FORCE_AGENT keeps a host export from silently
			// overriding the ladder under test.
			t.Setenv("MOE_FORCE_AGENT", c.force)
			if got := resolveAgentName(c.explicit, c.runDefault, c.stylesheet); got != c.want {
				t.Fatalf("resolveAgentName(%q, %q, %q) = %q, want %q",
					c.explicit, c.runDefault, c.stylesheet, got, c.want)
			}
		})
	}
}

func TestResolveAgentNameIgnoresRetiredMOEAgent(t *testing.T) {
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_AGENT", "codex")
	if got := resolveAgentName("", "", ""); got != "claude" {
		t.Fatalf("resolveAgentName with retired MOE_AGENT = %q, want claude", got)
	}
}

// TestStageAgentNameNilMetadata covers the early-bootstrap path
// where md hasn't been loaded yet. The helper must not deref a nil
// metadata pointer, and the run-default rung of the ladder is
// simply skipped.
func TestStageAgentNameNilMetadata(t *testing.T) {
	t.Setenv("MOE_FORCE_AGENT", "")
	if got := stageAgentName(stageSessionOpts{}, nil, ""); got != "claude" {
		t.Fatalf("nil md: got %q, want claude", got)
	}
}

// TestStageAgentNameRunDefault pins that md.Agent feeds the
// run-default rung.
func TestStageAgentNameRunDefault(t *testing.T) {
	t.Setenv("MOE_FORCE_AGENT", "")
	md := &run.Metadata{Agent: "codex"}
	if got := stageAgentName(stageSessionOpts{}, md, ""); got != "codex" {
		t.Fatalf("run default: got %q, want codex", got)
	}
}

// TestStageAgentNameStylesheet pins that the stylesheet agent feeds the
// ladder below the run default (which is unset here).
func TestStageAgentNameStylesheet(t *testing.T) {
	t.Setenv("MOE_FORCE_AGENT", "")
	if got := stageAgentName(stageSessionOpts{}, nil, "codex"); got != "codex" {
		t.Fatalf("stylesheet: got %q, want codex", got)
	}
	// An explicit --agent override still beats the stylesheet.
	if got := stageAgentName(stageSessionOpts{Agent: "claude"}, nil, "codex"); got != "claude" {
		t.Fatalf("explicit over stylesheet: got %q, want claude", got)
	}
}
