package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestStageVerbAgentFlagRejectsUnknown is the negative test that backs
// the per-turn --agent override on every stage-entry verb. agent.Get
// runs after fs.Parse / NArg so a typoed backend surfaces at the verb
// the operator typed (exit 2) instead of at first dispatch.
//
// Each case names the smallest invocation that reaches the validation
// block; the validation runs before any project/run lookup, so no
// bureaucracy fixture is needed for the refusal path.
func TestStageVerbAgentFlagRejectsUnknown(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		// sdlc trio — backfilled validation (the design's "Plus: fix
		// sdlc design/code/test to validate" subsection).
		{"sdlc design", []string{"sdlc", "design", "--agent=gpt", "moe/x"}},
		{"sdlc code", []string{"sdlc", "code", "--agent=gpt", "moe/x"}},
		{"sdlc test", []string{"sdlc", "test", "--agent=gpt", "moe/x"}},
		// Group A — stage-entry verbs gaining --agent.
		{"twin vision", []string{"twin", "vision", "--agent=gpt", "moe/x"}},
		{"twin architecture", []string{"twin", "architecture", "--agent=gpt", "moe/x"}},
		{"twin finalize", []string{"twin", "finalize", "--agent=gpt", "moe/x"}},
		{"kb research", []string{"kb", "research", "--agent=gpt", "moe/x"}},
		{"kb summarize", []string{"kb", "summarize", "--agent=gpt", "moe/x"}},
		{"audit plan", []string{"audit", "plan", "--agent=gpt", "moe/x"}},
		{"audit report", []string{"audit", "report", "--agent=gpt", "moe/x"}},
		{"hooks code", []string{"hooks", "code", "--agent=gpt", "moe/x"}},
		{"meta-moe report", []string{"meta-moe", "report", "--agent=gpt", "moe/x"}},
		// Group B — wiki-session verbs.
		{"twin reflect", []string{"twin", "reflect", "--agent=gpt", "moe"}},
		{"twin claim", []string{"twin", "claim", "--agent=gpt", "moe"}},
		{"kb lint", []string{"kb", "lint", "--agent=gpt", "moe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run(tc.args, &out, &errb)
			if code != 2 {
				t.Fatalf("%s: exit=%d, want 2; stderr=%q", tc.name, code, errb.String())
			}
			if !strings.Contains(errb.String(), "unknown backend") {
				t.Fatalf("%s: expected unknown-backend error, got: %q", tc.name, errb.String())
			}
		})
	}
}

// TestTwinReflectAgentFlagPersistsToRunJSON pins the design's "twin
// reflect --agent codex persists to run.json" claim: the value lands
// on Metadata.Agent at mint time, so every later moe twin <stage>
// invocation reads codex through stageAgentName without needing the
// flag again. Pattern: bureaucracy + project fixture, Run the verb,
// load the just-minted run.json.
func TestTwinReflectAgentFlagPersistsToRunJSON(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"twin", "reflect", "--agent=codex", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "reflect", "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var md run.Metadata
	if err := json.Unmarshal(body, &md); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	if md.Agent != "codex" {
		t.Fatalf("Agent = %q, want %q", md.Agent, "codex")
	}
}
