package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// `moe {idea,intent} edit --chat` opens an ordinary stage session on the
// document the capture run already is. These tests pin the wiring at the
// seam — which document, which knobs, which backend — without spinning a
// real session worktree; the fragment's effect on the assembled prompt
// is the test stage's business.

// editChatCall records the one runStageSession invocation an
// `edit --chat` turn makes.
type editChatCall struct {
	called    bool
	projectID string
	runID     string
	docID     string
	opts      stageSessionOpts
}

// captureEditChatSession swaps runStageSession for a recorder, restoring
// the original on cleanup. Sibling of chat_test.go's stubStageSession,
// but it keeps the opts so the knobs can be asserted.
func captureEditChatSession(t *testing.T) *editChatCall {
	t.Helper()
	got := &editChatCall{}
	prev := runStageSession
	runStageSession = func(projectID, runID, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		got.called = true
		got.projectID, got.runID, got.docID, got.opts = projectID, runID, docID, opts
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })
	return got
}

// TestEditChatOpensDocumentSession is the core claim of the feature: the
// refinement session rides runStageSession on the capture run's own
// document — which is what homes the transcript and gives `log` something
// to render — with no sandbox clone and no chain prompt.
//
// noEditor is load-bearing, not hygiene: --chat must not require $EDITOR,
// so a turn that reaches the session with neither var set proves the
// editor gate was skipped rather than satisfied.
func TestEditChatOpensDocumentSession(t *testing.T) {
	cases := []struct {
		verb     string
		workflow string
		docID    string
	}{
		{"idea", dash.IdeaWorkflow, dash.IdeaDocID},
		{"intent", dash.IntentWorkflow, dash.IntentDocID},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			trailerstest.SeedProject(t, root, "tele")
			trailerstest.SeedRun(t, root, "tele", "sharpen-me", tc.workflow, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			noEditor(t)

			got := captureEditChatSession(t)
			var out, errb bytes.Buffer
			if code := Run([]string{tc.verb, "edit", "--chat", "tele/sharpen-me"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if !got.called {
				t.Fatal("--chat did not open a stage session")
			}
			if got.projectID != "tele" || got.runID != "sharpen-me" || got.docID != tc.docID {
				t.Fatalf("session on %s/%s doc=%q, want tele/sharpen-me doc=%q",
					got.projectID, got.runID, got.docID, tc.docID)
			}
			// A shelf note is the operator's framing of a problem, not a
			// claim about source — the crisp line against `moe chat`.
			if got.opts.NeedsSandbox {
				t.Error("refinement session should be document-only (NeedsSandbox=false)")
			}
			// Single-stage workflow: the only prompt on offer would be
			// the close nudge, whose Enter-default closes the entry.
			if !got.opts.SkipNextStage {
				t.Error("single-stage capture workflow should suppress the chain prompt")
			}
			if got.opts.InitialPrompt == "" {
				t.Error("session should auto-send a kickoff so the operator isn't typing 'go'")
			}
			if got.opts.Headless {
				t.Error("edit --chat is interactive-only; nothing cascades into a capture run")
			}
			// The capture workflows' close deliberately skips harvest, so
			// without this every followup and lore entry the session files
			// is committed to the journal and stranded there.
			if !got.opts.HarvestOnExit {
				t.Error("a capture run's session end is its only harvest point")
			}
			if got.opts.Agent != "" {
				t.Errorf("Agent=%q without --agent, want the ladder's empty default", got.opts.Agent)
			}
		})
	}
}

// TestEditChatThreadsAgentOverride pins --agent riding through to the
// per-turn ladder rather than being parsed and dropped.
func TestEditChatThreadsAgentOverride(t *testing.T) {
	for _, tc := range []struct {
		verb     string
		workflow string
	}{
		{"idea", dash.IdeaWorkflow},
		{"intent", dash.IntentWorkflow},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			trailerstest.SeedProject(t, root, "tele")
			trailerstest.SeedRun(t, root, "tele", "sharpen-me", tc.workflow, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			noEditor(t)

			got := captureEditChatSession(t)
			var out, errb bytes.Buffer
			if code := Run([]string{tc.verb, "edit", "--chat", "--agent=codex", "tele/sharpen-me"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if got.opts.Agent != "codex" {
				t.Fatalf("Agent=%q, want codex", got.opts.Agent)
			}
		})
	}
}

// TestEditChatRefusesForeignRun keeps the workflow guard ahead of the
// session: ideas, intents, and sdlc runs share one slug namespace per
// project, so `intent edit --chat` on an idea's slug must refuse rather
// than open a sharpening session on someone else's document.
func TestEditChatRefusesForeignRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "shelf-note", dash.IdeaWorkflow, run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)

	got := captureEditChatSession(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "edit", "--chat", "tele/shelf-note"}, &out, &errb); code == 0 {
		t.Fatalf("expected refusal on a foreign run, got 0; stdout=%q", out.String())
	}
	if got.called {
		t.Fatal("session opened on an idea run through the intent verb")
	}
}

// TestEditChatRefusesDirtyTree keeps the clean-tree gate ahead of the
// session too — the session machinery rebases the bureaucracy, so a dirty
// tree has to refuse before the worktree opens, not after.
func TestEditChatRefusesDirtyTree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "shelf-note", dash.IdeaWorkflow, run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	dirtyTracked(t, root)

	got := captureEditChatSession(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "edit", "--chat", "tele/shelf-note"}, &out, &errb); code == 0 {
		t.Fatalf("expected refusal on a dirty tree, got 0; stdout=%q", out.String())
	}
	if got.called {
		t.Fatal("session opened against a dirty bureaucracy")
	}
}
