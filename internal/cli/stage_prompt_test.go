package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestStageLocationSectionSDLC pins the rendered header for each sdlc
// stage. The header is the agent-facing replacement for the
// neighbor-command prose the fragments used to carry; pinning the
// expected substrings catches a future rendering bug at build time
// rather than at "operator complained the agent said the wrong thing."
func TestStageLocationSectionSDLC(t *testing.T) {
	md := &run.Metadata{Project: "p", ID: "r", Workflow: "sdlc"}
	cases := []struct {
		stage string
		want  []string
		deny  []string
	}{
		{
			stage: "design",
			want: []string{
				"## Stage location",
				"Workflow: sdlc — **design** → code → test → push",
				"You are at: design",
				"Next stage: code",
				"`moe sdlc code p r`.",
			},
			deny: []string{"Previous stage"},
		},
		{
			stage: "code",
			want: []string{
				"Workflow: sdlc — design → **code** → test → push",
				"You are at: code",
				"Previous stage: design",
				"Next stage: test",
				"`moe sdlc test p r`.",
			},
		},
		{
			stage: "test",
			want: []string{
				"Workflow: sdlc — design → code → **test** → push",
				"You are at: test",
				"Previous stage: code",
				"Next stage: push",
				"`moe sdlc push p r`.",
			},
		},
		{
			stage: "push",
			want: []string{
				"Workflow: sdlc — design → code → test → **push**",
				"You are at: push",
				"Previous stage: test",
			},
			deny: []string{"Next stage"},
		},
	}
	for _, tc := range cases {
		got := stageLocationSection(md, tc.stage)
		for _, sub := range tc.want {
			if !strings.Contains(got, sub) {
				t.Errorf("stage %q: missing %q in:\n%s", tc.stage, sub, got)
			}
		}
		for _, sub := range tc.deny {
			if strings.Contains(got, sub) {
				t.Errorf("stage %q: unexpected %q in:\n%s", tc.stage, sub, got)
			}
		}
	}
}

// TestStageLocationSectionUnknownStage returns "" for stages not in
// the workflow's ladder — buildSystemPrompt then drops the section the
// same way it drops a missing fragment, instead of rendering a header
// that names a stage outside the workflow.
func TestStageLocationSectionUnknownStage(t *testing.T) {
	md := &run.Metadata{Project: "p", ID: "r", Workflow: "sdlc"}
	if got := stageLocationSection(md, "bogus"); got != "" {
		t.Errorf("expected empty for unknown stage, got:\n%s", got)
	}
}

// TestStageLocationSectionUnknownWorkflow returns "" for an unregistered
// workflow rather than a partial header. Symmetric with the unknown-
// stage case — both are upstream data bugs and both should surface as
// "no header" rather than wrong header.
func TestStageLocationSectionUnknownWorkflow(t *testing.T) {
	md := &run.Metadata{Project: "p", ID: "r", Workflow: "bogus"}
	if got := stageLocationSection(md, "code"); got != "" {
		t.Errorf("expected empty for unknown workflow, got:\n%s", got)
	}
}

// TestOperationalCoreCanvasPathSwitchesOnClonePath pins the canvas,
// followups, and twin-feedback paths operationalCore hands to the
// agent:
//   - clonePath == "" (document-only stage, cwd = bureaucracy root):
//     absolute paths under root, so the agent can write directly to
//     the canonical files.
//   - clonePath != "" (code-bearing stage, cwd = sandbox clone):
//     cwd-relative paths under ./.moe-run/, because codex's
//     apply_patch refuses to write outside the cwd's git project
//     even when the bureaucracy root is in --add-dir. The pre/post
//     shuttle in clone_canvas.go owns the bytes' actual journey.
//
// Either direction breaks a real workflow — pin all three writable
// paths so the next refactor of operationalCore can't silently
// regress codex's headless code stage back to "patch rejected:
// writing outside of the project".
func TestOperationalCoreCanvasPathSwitchesOnClonePath(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: "sdlc"}

	docOnly := operationalCore(root, md, "design", "")
	wantDocOnly := []string{
		filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")),
		filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "twin")),
		filepath.Join(root, run.FollowupsPath(md.Project, md.ID)),
	}
	for _, want := range wantDocOnly {
		if !strings.Contains(docOnly, want) {
			t.Errorf("doc-only prompt missing absolute path %q:\n%s", want, docOnly)
		}
	}
	if strings.Contains(docOnly, CloneRunDir) {
		t.Errorf("doc-only prompt must not name the clone run dir:\n%s", docOnly)
	}

	codeStage := operationalCore(root, md, "code", "/sandbox/clones/tele/fix-it")
	wantCode := []string{
		filepath.Join(".", CloneRunDir, "documents", "code", "content.md"),
		filepath.Join(".", CloneRunDir, "followups.md"),
		filepath.Join(".", CloneRunDir, "feedback", "twin.md"),
	}
	for _, want := range wantCode {
		if !strings.Contains(codeStage, want) {
			t.Errorf("code-stage prompt missing %q:\n%s", want, codeStage)
		}
	}
	// Absolute paths under the bureaucracy root would tempt the
	// agent to apply_patch them and trip codex's project-scope
	// check — keep the cwd-relative paths the only writable pointers
	// for code stages.
	denyCode := []string{
		filepath.Join(root, run.ContentPath(md.Project, md.ID, "code")),
		filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "twin")),
		filepath.Join(root, run.FollowupsPath(md.Project, md.ID)),
	}
	for _, deny := range denyCode {
		if strings.Contains(codeStage, deny) {
			t.Errorf("code-stage prompt must not name absolute bureaucracy path %q:\n%s", deny, codeStage)
		}
	}
}

// TestStageLocationSectionIdeaStage exercises the single-stage / no-
// runnable-verb branch: idea registers `idea` as its only stage and
// has no `moe idea idea` verb, so the header renders the ladder and
// the you-are-at line without a chain-prompt invocation hint and
// without prev/next lines.
func TestStageLocationSectionIdeaStage(t *testing.T) {
	md := &run.Metadata{Project: "p", ID: "r", Workflow: "idea"}
	got := stageLocationSection(md, "idea")
	wantSubs := []string{
		"## Stage location",
		"Workflow: idea — **idea**",
		"You are at: idea",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in:\n%s", sub, got)
		}
	}
	for _, sub := range []string{"Previous stage", "Next stage", "chain prompt will offer"} {
		if strings.Contains(got, sub) {
			t.Errorf("unexpected %q in:\n%s", sub, got)
		}
	}
}
