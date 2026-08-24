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
		// sdlc stages — backfilled validation (the design's "Plus: fix
		// sdlc design/code/review/test to validate" subsection).
		{"sdlc design", []string{"sdlc", "design", "--agent=gpt", "moe/x"}},
		{"sdlc code", []string{"sdlc", "code", "--agent=gpt", "moe/x"}},
		{"sdlc review", []string{"sdlc", "review", "--agent=gpt", "moe/x"}},
		{"sdlc test", []string{"sdlc", "test", "--agent=gpt", "moe/x"}},
		// Group A — stage-entry verbs gaining --agent.
		{"twin vision", []string{"twin", "vision", "--agent=gpt", "moe/x"}},
		{"twin architecture", []string{"twin", "architecture", "--agent=gpt", "moe/x"}},
		{"twin finalize", []string{"twin", "finalize", "--agent=gpt", "moe/x"}},
		{"chat chat", []string{"chat", "chat", "--agent=gpt", "moe/x"}},
		// Group B — wiki-session verbs.
		{"twin reflect", []string{"twin", "reflect", "--agent=gpt", "moe"}},
		// The two edit verbs validate only on the --chat path; without
		// it, --agent is a usage error before the backend lookup (see
		// TestEditChatAgentFlagNeedsChat).
		{"idea edit", []string{"idea", "edit", "--chat", "--agent=gpt", "moe/x"}},
		{"intent edit", []string{"intent", "edit", "--chat", "--agent=gpt", "moe/x"}},
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

// TestCaptureVerbsRejectAgentFlagsAsUnknown pins the capture half of the
// discipline: `new` stays editor-only for both capture workflows, so
// neither --chat nor --agent is defined there and both fail in flag
// parsing rather than reaching a backend lookup. Refinement is the only
// door an agent holds (see TestEditChatAgentFlagNeedsChat).
func TestCaptureVerbsRejectAgentFlagsAsUnknown(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"idea new agent", []string{"idea", "new", "--agent=gpt", "moe/x"}},
		{"idea new chat", []string{"idea", "new", "--chat", "moe/x"}},
		{"intent new agent", []string{"intent", "new", "--agent=gpt", "moe/x"}},
		{"intent new chat", []string{"intent", "new", "--chat", "moe/x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run(tc.args, &out, &errb)
			if code != 2 {
				t.Fatalf("%s: exit=%d, want 2; stderr=%q", tc.name, code, errb.String())
			}
			if !strings.Contains(errb.String(), "flag provided but not defined") {
				t.Fatalf("%s: expected unknown-flag error, got: %q", tc.name, errb.String())
			}
			if strings.Contains(errb.String(), "unknown backend") {
				t.Fatalf("%s: capture flags should fail in parsing, not backend lookup: %q", tc.name, errb.String())
			}
		})
	}
}

// TestEditChatAgentFlagNeedsChat pins --agent as chat-only on the edit
// verbs: the $EDITOR path launches no agent, so accepting the flag there
// would be a dead flag that silently does nothing. It refuses at parse
// time (exit 2), before any project or run lookup — note the fixture-free
// invocation.
func TestEditChatAgentFlagNeedsChat(t *testing.T) {
	for _, verb := range []string{"idea", "intent"} {
		t.Run(verb, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run([]string{verb, "edit", "--agent=claude", "moe/x"}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "--agent needs --chat") {
				t.Fatalf("expected --agent-needs---chat error, got: %q", errb.String())
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
