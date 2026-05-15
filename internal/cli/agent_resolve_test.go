package cli

import (
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestResolveAgentNamePrecedence pins the ladder design.md describes:
// explicit → run.json.Agent → $MOE_AGENT → "claude".
func TestResolveAgentNamePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		explicit   string
		runDefault string
		env        string
		want       string
	}{
		{
			name:     "explicit wins over everything",
			explicit: "codex",
			env:      "claude",
			want:     "codex",
		},
		{
			name:       "runDefault wins over env",
			runDefault: "codex",
			env:        "claude",
			want:       "codex",
		},
		{
			name: "env used when nothing else set",
			env:  "codex",
			want: "codex",
		},
		{
			name: "hard default kicks in when nothing else set",
			want: "claude",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// t.Setenv always sets the var; explicit empty is the
			// closest stand-in for "unset" without unsetting whatever
			// the host shell injected.
			t.Setenv("MOE_AGENT", c.env)
			if got := resolveAgentName(c.explicit, c.runDefault); got != c.want {
				t.Fatalf("resolveAgentName(%q, %q) = %q, want %q",
					c.explicit, c.runDefault, got, c.want)
			}
		})
	}
}

// TestStageAgentNameNilMetadata covers the early-bootstrap path
// where md hasn't been loaded yet. The helper must not deref a nil
// metadata pointer, and the run-default rung of the ladder is
// simply skipped.
func TestStageAgentNameNilMetadata(t *testing.T) {
	t.Setenv("MOE_AGENT", "codex")
	if got := stageAgentName(stageSessionOpts{}, nil); got != "codex" {
		t.Fatalf("nil md: got %q, want codex", got)
	}
}

// TestStageAgentNameRunDefault pins that md.Agent feeds the
// run-default rung — the persisted agent on the run wins over
// $MOE_AGENT.
func TestStageAgentNameRunDefault(t *testing.T) {
	t.Setenv("MOE_AGENT", "claude")
	md := &run.Metadata{Agent: "codex"}
	if got := stageAgentName(stageSessionOpts{}, md); got != "codex" {
		t.Fatalf("run default: got %q, want codex", got)
	}
}
