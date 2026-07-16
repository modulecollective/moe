package cli

import (
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestResolveAgentNamePrecedence pins the ladder design.md describes:
// $MOE_FORCE_AGENT → explicit → run.json.Agent → stylesheet →
// $MOE_AGENT → "claude".
func TestResolveAgentNamePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		force      string
		explicit   string
		runDefault string
		stylesheet string
		env        string
		want       string
	}{
		{
			name:     "force wins over explicit",
			force:    "claude",
			explicit: "codex",
			env:      "codex",
			want:     "claude",
		},
		{
			name:       "force wins over runDefault",
			force:      "claude",
			runDefault: "codex",
			env:        "codex",
			want:       "claude",
		},
		{
			name:  "force wins over env",
			force: "claude",
			env:   "codex",
			want:  "claude",
		},
		{
			name:     "explicit wins over everything below force",
			explicit: "codex",
			env:      "claude",
			want:     "codex",
		},
		{
			name:       "runDefault wins over stylesheet",
			runDefault: "codex",
			stylesheet: "claude",
			env:        "claude",
			want:       "codex",
		},
		{
			name:       "stylesheet wins over env",
			stylesheet: "codex",
			env:        "claude",
			want:       "codex",
		},
		{
			name: "env used when nothing above set",
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
			// t.Setenv always sets the var; an empty value is the
			// closest stand-in for "unset" without unsetting whatever
			// the host shell injected. Clearing MOE_FORCE_AGENT in the
			// non-force cases is the regression guard: it keeps a host
			// export from silently overriding the legacy ladder.
			t.Setenv("MOE_FORCE_AGENT", c.force)
			t.Setenv("MOE_AGENT", c.env)
			if got := resolveAgentName(c.explicit, c.runDefault, c.stylesheet); got != c.want {
				t.Fatalf("resolveAgentName(%q, %q, %q) = %q, want %q",
					c.explicit, c.runDefault, c.stylesheet, got, c.want)
			}
		})
	}
}

// TestStageModel pins the paired-model drop rule: a stylesheet model
// rides only when the turn's resolved backend matches the stylesheet's
// own resolved agent. Each mismatch case routes the agent through a
// different rung above the stylesheet (force env, --agent, run.json)
// via stageAgentName, mirroring the production composition in
// runStageSession.
func TestStageModel(t *testing.T) {
	cases := []struct {
		name       string
		explicit   string // opts.Model from a bounded curation caller
		force      string // $MOE_FORCE_AGENT
		agentFlag  string // opts.Agent (--agent)
		runDefault string // run.json.Agent
		sheetAgent string
		sheetModel string
		wantModel  string
		wantNotice bool
	}{
		{
			name:       "paired match keeps model",
			sheetAgent: "claude",
			sheetModel: "fable",
			wantModel:  "fable",
		},
		{
			name:       "paired mismatch via force env drops model",
			force:      "codex",
			sheetAgent: "claude",
			sheetModel: "fable",
			wantModel:  "",
			wantNotice: true,
		},
		{
			name:       "paired mismatch via --agent drops model",
			agentFlag:  "codex",
			sheetAgent: "claude",
			sheetModel: "fable",
			wantModel:  "",
			wantNotice: true,
		},
		{
			name:       "paired mismatch via run.json agent drops model",
			runDefault: "codex",
			sheetAgent: "claude",
			sheetModel: "fable",
			wantModel:  "",
			wantNotice: true,
		},
		{
			name:       "unpaired model rides any backend",
			runDefault: "codex",
			sheetModel: "fable",
			wantModel:  "fable",
		},
		{
			name:       "explicit model never dropped",
			explicit:   "claude-fable-5",
			force:      "codex",
			sheetAgent: "claude",
			sheetModel: "fable",
			wantModel:  "claude-fable-5",
		},
		{
			name:       "no stylesheet model resolves empty",
			sheetAgent: "claude",
			wantModel:  "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MOE_FORCE_AGENT", c.force)
			t.Setenv("MOE_AGENT", "")
			var md *run.Metadata
			if c.runDefault != "" {
				md = &run.Metadata{Agent: c.runDefault}
			}
			agentName := stageAgentName(stageSessionOpts{Agent: c.agentFlag}, md, c.sheetAgent)
			var stderr strings.Builder
			got := stageModel(c.explicit, c.sheetAgent, c.sheetModel, agentName, &stderr)
			if got != c.wantModel {
				t.Fatalf("stageModel = %q, want %q", got, c.wantModel)
			}
			if c.wantNotice {
				want := `model-stylesheet: dropping model "fable" (rule pairs agent claude; turn runs codex)`
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("unexpected stderr: %q", stderr.String())
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
	if got := stageAgentName(stageSessionOpts{}, nil, ""); got != "codex" {
		t.Fatalf("nil md: got %q, want codex", got)
	}
}

// TestStageAgentNameRunDefault pins that md.Agent feeds the
// run-default rung — the persisted agent on the run wins over
// $MOE_AGENT.
func TestStageAgentNameRunDefault(t *testing.T) {
	t.Setenv("MOE_AGENT", "claude")
	md := &run.Metadata{Agent: "codex"}
	if got := stageAgentName(stageSessionOpts{}, md, ""); got != "codex" {
		t.Fatalf("run default: got %q, want codex", got)
	}
}

// TestStageAgentNameStylesheet pins that the stylesheet agent feeds the
// ladder below the run default (which is unset here) and above
// $MOE_AGENT.
func TestStageAgentNameStylesheet(t *testing.T) {
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_AGENT", "claude")
	if got := stageAgentName(stageSessionOpts{}, nil, "codex"); got != "codex" {
		t.Fatalf("stylesheet: got %q, want codex", got)
	}
	// An explicit --agent override still beats the stylesheet.
	if got := stageAgentName(stageSessionOpts{Agent: "claude"}, nil, "codex"); got != "claude" {
		t.Fatalf("explicit over stylesheet: got %q, want claude", got)
	}
}
