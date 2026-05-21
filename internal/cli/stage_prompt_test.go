package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
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
				"`moe sdlc code p/r`.",
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
				"`moe sdlc test p/r`.",
			},
		},
		{
			stage: "test",
			want: []string{
				"Workflow: sdlc — design → code → **test** → push",
				"You are at: test",
				"Previous stage: code",
				"Next stage: push",
				"`moe sdlc push p/r`.",
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

// TestBuildSystemPromptInjectsLoreAfterTwin pins the design contract:
// the lore catalog appears right after the twin reference, before the
// operational core, so the agent reads project-specific intent first,
// then project-agnostic operational facts that build on it.
func TestBuildSystemPromptInjectsLoreAfterTwin(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loreDir := wiki.LoreDir(root)
	if err := os.MkdirAll(loreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loreDir, "entry.md"),
		[]byte("---\ntitle: Sentinel Lore Entry\napplies-when: testing prompt placement\n---\n\nbody\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	twinIdx := strings.Index(got, "## Project digital twin")
	loreIdx := strings.Index(got, "## Lore (cross-project)")
	opIdx := strings.Index(got, "You are collaborating with the operator")
	if twinIdx < 0 || loreIdx < 0 || opIdx < 0 {
		t.Fatalf("missing one of the expected sections; twin=%d lore=%d op=%d in:\n%s",
			twinIdx, loreIdx, opIdx, got)
	}
	if !(twinIdx < loreIdx && loreIdx < opIdx) {
		t.Errorf("expected twin < lore < operational-core ordering; got twin=%d lore=%d op=%d",
			twinIdx, loreIdx, opIdx)
	}
	if !strings.Contains(got, "Sentinel Lore Entry") {
		t.Errorf("lore catalog missing entry title; got:\n%s", got)
	}
	if !strings.Contains(got, "testing prompt placement") {
		t.Errorf("lore catalog missing applies-when hint; got:\n%s", got)
	}
}

// TestFollowupsReferenceSection pins the followups nudge: a short
// "Out-of-scope work" block naming the per-run followups path and the
// moe-bureaucracy skill by name. Sibling of TwinReferenceSection /
// LoreReferenceSection — each trace channel gets one recognise-and-
// contribute cue in the prompt so the agent has *capture* as a live
// category. The skill body retains the *how*.
func TestFollowupsReferenceSection(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it", Workflow: "sdlc"}
	got := followupsReferenceSection(root, md)
	wantPath := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	for _, want := range []string{
		"## Out-of-scope work",
		"`moe-bureaucracy`",
		wantPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("followups reference missing %q in:\n%s", want, got)
		}
	}
}

// TestBuildSystemPromptIncludesFollowupsNudge pins that the assembled
// prompt for a code-stage session contains the followups nudge block,
// landing between the lore catalog and operationalCore so the three
// trace-channel nudges (twin, lore, followups) sit together as siblings
// before the per-turn framing.
func TestBuildSystemPromptIncludesFollowupsNudge(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	followupsIdx := strings.Index(got, "## Out-of-scope work")
	opIdx := strings.Index(got, "You are collaborating with the operator")
	if followupsIdx < 0 || opIdx < 0 {
		t.Fatalf("missing sections; followups=%d op=%d in:\n%s", followupsIdx, opIdx, got)
	}
	if !(followupsIdx < opIdx) {
		t.Errorf("followups nudge must precede operationalCore; followups=%d op=%d",
			followupsIdx, opIdx)
	}
	wantPath := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	if !strings.Contains(got, wantPath) {
		t.Errorf("followups nudge missing per-run path %q in:\n%s", wantPath, got)
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

// TestOperationalCoreCanvasPathIsAbsoluteAcrossStages pins the canvas
// path operationalCore renders. Under the cwd-inversion shape both
// code-bearing and document-only stages name the canvas at its
// absolute bureaucracy path — code stages reach it because cwd is
// the bureaucracy session worktree (the clone is reached via
// --add-dir for source edits), so the agent's natural write target
// matches the path MoE reads back at commit time.
//
// Trace-recording paths (followups, twin feedback, lore feedback)
// used to be checked here too; that guidance now lives in the
// moe-bureaucracy skill so the prompt itself only carries the
// always-on framing.
func TestOperationalCoreCanvasPathIsAbsoluteAcrossStages(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	wantDocOnly := filepath.Join(root, run.ContentPath(md.Project, md.ID, "design"))
	docOnly := operationalCore(root, md, "design", "")
	if !strings.Contains(docOnly, wantDocOnly) {
		t.Errorf("doc-only prompt missing absolute canvas path %q:\n%s", wantDocOnly, docOnly)
	}

	wantCode := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
	codeStage := operationalCore(root, md, "code", "/sandbox/clones/tele/fix-it")
	if !strings.Contains(codeStage, wantCode) {
		t.Errorf("code-stage prompt missing absolute canvas path %q:\n%s", wantCode, codeStage)
	}
	// The legacy `./.moe-run/` shuttle paths must not leak back into
	// the prompt — they belong to the removed clone-canvas indirection.
	for _, deny := range []string{"./.moe-run/", ".moe-run/documents", ".moe-run/followups", ".moe-run/feedback"} {
		if strings.Contains(codeStage, deny) {
			t.Errorf("code-stage prompt still names shuttle path %q:\n%s", deny, codeStage)
		}
	}
}

// TestOperationalCoreNoLongerCarriesTraceBlocks pins the moe-bureaucracy
// skill extraction: the three trace-recording paragraphs (twin
// observations, portable lore, followups) are out of the per-turn
// prompt and live in the skill's progressive-disclosure body. A
// regression that reinlines them undoes the token savings the
// adopt-agent-skills run shipped to claw back.
func TestOperationalCoreNoLongerCarriesTraceBlocks(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	for _, stage := range []string{"design", "code"} {
		clone := ""
		if stage == "code" {
			clone = "/sandbox/clones/tele/fix-it"
		}
		got := operationalCore(root, md, stage, clone)
		// Phrases that anchor the three extracted paragraphs.
		for _, deny := range []string{
			"belongs in the digital",
			"`moe twin reflect`",
			"belongs in `lore/`",
			"applies-when:",
			"out of scope for this cycle",
			"compose-tailscale-binds",
		} {
			if strings.Contains(got, deny) {
				t.Errorf("stage %q: trace-recording phrase %q reinlined into operationalCore:\n%s", stage, deny, got)
			}
		}
		// Negative path check: the trace-recording file paths must not
		// appear in operationalCore either, since they only made sense
		// alongside their now-extracted prose.
		for _, deny := range []string{
			filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "twin")),
			filepath.Join(root, run.FeedbackPath(md.Project, md.ID, "lore")),
			filepath.Join(root, run.FollowupsPath(md.Project, md.ID)),
		} {
			if strings.Contains(got, deny) {
				t.Errorf("stage %q: trace-recording path %q reinlined into operationalCore:\n%s", stage, deny, got)
			}
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

// TestProjectAgentsGuidance pins the path-mention shape that replaced
// the body-inline approach. The agent's cwd doesn't reach the clone
// under the cwd-inversion shape, so the prompt names the absolute
// path to the project's AGENTS.md / CLAUDE.md and trusts the agent
// to read it on its first relevant action. Inlining the body would
// pay the cost on every turn even when the guidance never got read;
// the path mention is one short paragraph.
//
// Misroute = the project's ground rules silently vanish from the
// agent's context.
func TestProjectAgentsGuidance(t *testing.T) {
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "AGENTS.md"), []byte("stdlib only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "CLAUDE.md"), []byte("internal/git is the sole seam\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := projectAgentsGuidance(clone)
	for _, want := range []string{
		"## Project guidance",
		clone,
		filepath.Join(clone, "AGENTS.md"),
		filepath.Join(clone, "CLAUDE.md"),
		"Read it before",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// AGENTS.md listed first so its path mention precedes CLAUDE.md's.
	if strings.Index(got, "AGENTS.md") > strings.Index(got, "CLAUDE.md") {
		t.Errorf("AGENTS.md must precede CLAUDE.md:\n%s", got)
	}
	// The file bodies must NOT be inlined — that's the whole reason
	// for the path-mention rewrite.
	for _, deny := range []string{"stdlib only", "internal/git is the sole seam"} {
		if strings.Contains(got, deny) {
			t.Errorf("file body %q must not be inlined into prompt:\n%s", deny, got)
		}
	}

	emptyClone := t.TempDir()
	if got := projectAgentsGuidance(emptyClone); got != "" {
		t.Errorf("expected empty string when no files, got %q", got)
	}

	if got := projectAgentsGuidance(""); got != "" {
		t.Errorf("expected empty string for empty clonePath, got %q", got)
	}

	onlyClaude := t.TempDir()
	if err := os.WriteFile(filepath.Join(onlyClaude, "CLAUDE.md"), []byte("just claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = projectAgentsGuidance(onlyClaude)
	if !strings.Contains(got, filepath.Join(onlyClaude, "CLAUDE.md")) {
		t.Errorf("single-file case missing CLAUDE.md path: %q", got)
	}
	if strings.Contains(got, "AGENTS.md") {
		t.Errorf("AGENTS.md path mentioned when file absent: %q", got)
	}
}
