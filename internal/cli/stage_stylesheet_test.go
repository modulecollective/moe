package cli

import (
	"bytes"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/stylesheet"
)

func TestBackendModelSelectionLadder(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		other := "claude"
		if backend == other {
			other = "codex"
		}
		for _, rung := range []string{"force", "flag", "binding", "stylesheet"} {
			t.Run(backend+"/"+rung, func(t *testing.T) {
				t.Setenv("MOE_FORCE_AGENT", "")
				opts := stageSessionOpts{}
				md := &run.Metadata{}
				sheetAgent := other
				switch rung {
				case "force":
					t.Setenv("MOE_FORCE_AGENT", backend)
					opts.Agent, md.Agent = other, other
				case "flag":
					opts.Agent, md.Agent = backend, other
				case "binding":
					md.Agent = backend
				case "stylesheet":
					sheetAgent = backend
				}
				// A broad backend model follows whichever backend the ladder picks.
				s, err := stylesheet.Parse([]byte("* { model-codex: codex-choice; model-claude: claude-choice; }\nsdlc.design { agent: " + sheetAgent + "; }"))
				if err != nil {
					t.Fatal(err)
				}
				resolved := stageAgentName(opts, md, s.Agent("sdlc", "design"))
				if got := s.ModelForAgent("sdlc", "design", resolved); resolved != backend || got != backend+"-choice" {
					t.Fatalf("agent=%q model=%q", resolved, got)
				}
			})
		}
	}
	t.Setenv("MOE_FORCE_AGENT", "")
	for _, tc := range []struct{ src, model string }{
		{"* { model-codex: other-backend; }", ""},
		{"* { model-codex: other-backend; model-claude: own-model; }", "own-model"},
		{"", ""},
	} {
		s, err := stylesheet.Parse([]byte(tc.src))
		if err != nil {
			t.Fatal(err)
		}
		resolved := stageAgentName(stageSessionOpts{}, nil, s.Agent("sdlc", "design"))
		if got := s.ModelForAgent("sdlc", "design", resolved); resolved != "claude" || got != tc.model {
			t.Errorf("model-only sheet selected agent=%q model=%q", resolved, got)
		}
	}
}

func TestBackendModelsReachStageDispatch(t *testing.T) {
	for _, headless := range []bool{false, true} {
		mode := "interactive"
		method := "Execute"
		if headless {
			mode, method = "headless", "ExecuteOneShot"
		}
		t.Run(mode, func(t *testing.T) {
			root, fake := setupResumeFixture(t, "fake-model-dispatch")
			// Use the real names so the exact operator file passes validation.
			// Both executors record requests; no vendor process runs.
			for _, name := range []string{"claude", "codex"} {
				original, err := agent.Get(name)
				if err != nil {
					t.Fatal(err)
				}
				agent.Unregister(name)
				agent.Register(name, fake)
				t.Cleanup(func() {
					agent.Unregister(name)
					agent.Register(name, original)
				})
			}
			gittest.WriteAndCommit(t, root, stylesheet.FileName, backendModelsSheet, "seed backend model choices")
			for i, tc := range []struct{ force, flag, model string }{
				{"", "", "gpt-6-astra"},
				{"claude", "codex", "fable"},
				{"codex", "claude", "gpt-6-astra"},
				{"", "claude", "fable"},
			} {
				t.Setenv("MOE_FORCE_AGENT", tc.force)
				var stdout, stderr bytes.Buffer
				code := runStageSession("tele", "fix-it", "design", stageSessionOpts{
					Agent: tc.flag, Headless: headless, SkipNextStage: true,
				}, &stdout, &stderr)
				if code != 0 {
					t.Fatalf("turn %d exit=%d\nstdout=%s\nstderr=%s", i, code, stdout.String(), stderr.String())
				}
				if len(fake.execCalls) != i+1 {
					t.Fatalf("turn %d: calls=%+v", i, fake.execCalls)
				}
				call := fake.execCalls[i]
				if call.method != method || call.model != tc.model {
					t.Errorf("turn %d: method=%s model=%q, want %s / %q", i, call.method, call.model, method, tc.model)
				}
			}
		})
	}
}
