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
				"Workflow: sdlc — **design** → code → review → test → push",
				"You are at: design",
				"Next stage: code",
				"`moe sdlc code p/r`.",
			},
			deny: []string{"Previous stage"},
		},
		{
			stage: "code",
			want: []string{
				"Workflow: sdlc — design → **code** → review → test → push",
				"You are at: code",
				"Previous stage: design",
				"Next stage: review",
				"`moe sdlc review p/r`.",
			},
		},
		{
			stage: "review",
			want: []string{
				"Workflow: sdlc — design → code → **review** → test → push",
				"You are at: review",
				"Previous stage: code",
				"Next stage: test",
				"`moe sdlc test p/r`.",
			},
		},
		{
			stage: "test",
			want: []string{
				"Workflow: sdlc — design → code → review → **test** → push",
				"You are at: test",
				"Previous stage: review",
				"Next stage: push",
				"`moe sdlc push p/r`.",
			},
		},
		{
			stage: "push",
			want: []string{
				"Workflow: sdlc — design → code → review → test → **push**",
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

// seedIntentRun materialises an intent run on disk: run.json (workflow
// intent, given status) plus a canvas with the given body. Test helper
// for the intents reference section and dash tests, which read run.json
// (via run.Scan) and the canvas's first heading.
func seedIntentRun(t *testing.T, root, projectID, slug, status, canvasBody string) {
	t.Helper()
	md := &run.Metadata{
		ID:        slug,
		Project:   projectID,
		Status:    status,
		Workflow:  "intent",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatalf("save intent run %s: %v", slug, err)
	}
	path := filepath.Join(root, run.ContentPath(projectID, slug, "intent"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(canvasBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIntentsReferenceSection pins the catalog: open intents render as
// `slug — title` lines (title from the canvas's first heading) pointing
// at each canvas path; closed intents are excluded; and a project with
// no open intents produces no section at all.
func TestIntentsReferenceSection(t *testing.T) {
	root := newTestBureaucracy(t)
	seedIntentRun(t, root, "tele", "north-star", run.StatusInProgress, "# Be the fastest dash\n\nbody\n")
	seedIntentRun(t, root, "tele", "retired", run.StatusClosed, "# Old direction\n")

	got := intentsReferenceSection(root, "tele")
	for _, want := range []string{
		"## Project intents",
		"`north-star` — Be the fastest dash",
		filepath.Join(root, run.ContentPath("tele", "north-star", "intent")),
		"never create or edit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("intents section missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "retired") || strings.Contains(got, "Old direction") {
		t.Errorf("closed intent must be excluded from the catalog:\n%s", got)
	}

	// A project with no open intents produces no section — the empty
	// case pays zero prompt cost.
	if empty := intentsReferenceSection(root, "no-such-project"); empty != "" {
		t.Errorf("expected empty section for a project with no open intents, got:\n%s", empty)
	}
}

// TestIntentTitleFallsBackToSlug: a headless canvas (no `# ` heading)
// falls back to the slug rather than rendering a blank title.
func TestIntentTitleFallsBackToSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	seedIntentRun(t, root, "tele", "headless", run.StatusInProgress, "no heading here, just prose\n")
	if got := intentTitle(root, "tele", "headless"); got != "headless" {
		t.Errorf("expected slug fallback %q, got %q", "headless", got)
	}
}

// TestBuildSystemPromptInjectsIntentsBetweenTwinAndLore pins the design
// contract: the intents catalog lands after the twin reference and
// before the lore catalog, so the agent reads project-specific direction
// between what the project *is* (twin) and the project-agnostic facts
// (lore).
func TestBuildSystemPromptInjectsIntentsBetweenTwinAndLore(t *testing.T) {
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
		[]byte("---\ntitle: Sentinel\napplies-when: testing\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIntentRun(t, root, "tele", "aim-here", run.StatusInProgress, "# Aim here\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	twinIdx := strings.Index(got, "## Project digital twin")
	intentIdx := strings.Index(got, "## Project intents")
	loreIdx := strings.Index(got, "## Lore (cross-project)")
	if twinIdx < 0 || intentIdx < 0 || loreIdx < 0 {
		t.Fatalf("missing a section; twin=%d intents=%d lore=%d in:\n%s", twinIdx, intentIdx, loreIdx, got)
	}
	if !(twinIdx < intentIdx && intentIdx < loreIdx) {
		t.Errorf("expected twin < intents < lore ordering; got twin=%d intents=%d lore=%d", twinIdx, intentIdx, loreIdx)
	}
}

// TestBuildSystemPromptOmitsIntentsWhenNone: a project with no open
// intents produces no "## Project intents" section in the assembled
// prompt.
func TestBuildSystemPromptOmitsIntentsWhenNone(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "## Project intents") {
		t.Errorf("expected no intents section for a project with no open intents:\n%s", got)
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
		"`moe-context`",
		wantPath,
		// The one-line grammar is inlined so an agent that never opens
		// the skill still writes a parseable shape, plus the loud-at-close
		// note so a wrong shape reads as recoverable, not silent.
		"`- [ ] `slug` — Title`",
		"rejected at close",
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

// TestOperationalCoreNamesMoeContextSkill pins the always-on read cue:
// every stage's operationalCore points the agent at the moe-context
// skill for prior-run canvases / transcripts / journal slicing. Sibling
// to the per-trace-channel cues that point at moe-bureaucracy from
// twin / lore / followups sections — those name the *write* skill, this
// names the *read* skill in the always-on framing.
func TestOperationalCoreNamesMoeContextSkill(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	for _, stage := range []string{"design", "code"} {
		clone := ""
		if stage == "code" {
			clone = "/sandbox/clones/tele/fix-it"
		}
		got := operationalCore(root, md, stage, clone)
		for _, want := range []string{
			"`moe-context`",
			"prior run",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("stage %q: operationalCore missing %q in:\n%s", stage, want, got)
			}
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

// writePriorRun materialises a fake prior run dir on disk: writes
// run.json with the given metadata, plus a content.md per docID
// listed in docs, plus followups.md when withFollowups is true. Test
// helper for priorRunsSection — the section reads run.json (status,
// chain pointer) and globs documents/*/content.md, so the fakes need
// to match those shapes.
func writePriorRun(t *testing.T, root, projectID, slug, status, reopenOf string, docs []string, withFollowups bool) {
	t.Helper()
	md := &run.Metadata{
		ID:        slug,
		Project:   projectID,
		Status:    status,
		Workflow:  "sdlc",
		ReopenOf:  reopenOf,
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatalf("save prior run %s: %v", slug, err)
	}
	for _, doc := range docs {
		path := filepath.Join(root, run.ContentPath(projectID, slug, doc))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# "+doc+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if withFollowups {
		path := filepath.Join(root, run.FollowupsPath(projectID, slug))
		if err := os.WriteFile(path, []byte("- something\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPriorRunsSectionSingleDeep: a reopen with one prior names the
// prior's slug, its status, and the absolute paths to each existing
// documents/<doc>/content.md plus followups.md. This is the common
// case — most reopens are one level deep.
func TestPriorRunsSectionSingleDeep(t *testing.T) {
	root := newTestBureaucracy(t)
	writePriorRun(t, root, "tele", "fix-it", run.StatusClosed, "",
		[]string{"design", "code"}, true)
	md := &run.Metadata{
		ID:       "fix-it-2026-05-27",
		Project:  "tele",
		Workflow: "sdlc",
		ReopenOf: "fix-it",
	}
	got := priorRunsSection(root, md)
	wantSubs := []string{
		"## Prior runs",
		"This run is a reopen.",
		"Listed most-recent first",
		"`fix-it` (status: closed):",
		filepath.Join(root, run.ContentPath("tele", "fix-it", "design")),
		filepath.Join(root, run.ContentPath("tele", "fix-it", "code")),
		filepath.Join(root, run.FollowupsPath("tele", "fix-it")),
	}
	for _, want := range wantSubs {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestPriorRunsSectionTwoDeep: a chain of A → A-2 → A-3 (newest is
// the active run, A-2 is its prior, A is the grandprior) lists both
// priors with A-2 first. Order matters: the agent is told to read
// most-recent first so it sees the immediately-preceding attempt
// before deeper history.
func TestPriorRunsSectionTwoDeep(t *testing.T) {
	root := newTestBureaucracy(t)
	writePriorRun(t, root, "tele", "fix-it", run.StatusMerged, "",
		[]string{"design"}, false)
	writePriorRun(t, root, "tele", "fix-it-2", run.StatusClosed, "fix-it",
		[]string{"design", "code"}, false)
	md := &run.Metadata{
		ID:       "fix-it-3",
		Project:  "tele",
		Workflow: "sdlc",
		ReopenOf: "fix-it-2",
	}
	got := priorRunsSection(root, md)
	for _, want := range []string{
		"`fix-it-2` (status: closed):",
		"`fix-it` (status: merged):",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	idx2 := strings.Index(got, "`fix-it-2`")
	idx1 := strings.Index(got, "`fix-it` ")
	if !(idx2 >= 0 && idx1 > idx2) {
		t.Errorf("expected fix-it-2 before fix-it (most-recent first); fix-it-2=%d fix-it=%d in:\n%s",
			idx2, idx1, got)
	}
}

// TestPriorRunsSectionAbsentForNonReopen: a fresh run with no
// ReopenOf produces no section, so non-reopen runs pay zero prompt
// cost. Pinned because the slot is conditional in buildSystemPrompt
// and the negative case is the common case.
func TestPriorRunsSectionAbsentForNonReopen(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fresh", Project: "tele", Workflow: "sdlc"}
	if got := priorRunsSection(root, md); got != "" {
		t.Errorf("expected empty section for non-reopen, got:\n%s", got)
	}
}

// TestPriorRunsSectionStopsOnBrokenChain: if a prior's run.json
// can't be loaded (missing dir, corrupt json, etc.) the walk stops
// rather than failing the whole prompt. The walk so far still
// renders; downstream priors are silently dropped. Failing-soft
// matches every other prompt section's behaviour.
func TestPriorRunsSectionStopsOnBrokenChain(t *testing.T) {
	root := newTestBureaucracy(t)
	writePriorRun(t, root, "tele", "fix-it", run.StatusClosed, "missing-grandprior",
		[]string{"design"}, false)
	md := &run.Metadata{
		ID:       "fix-it-2",
		Project:  "tele",
		Workflow: "sdlc",
		ReopenOf: "fix-it",
	}
	got := priorRunsSection(root, md)
	if !strings.Contains(got, "`fix-it` (status: closed):") {
		t.Errorf("loaded prior must still render:\n%s", got)
	}
	if strings.Contains(got, "missing-grandprior") {
		t.Errorf("broken chain link must not appear:\n%s", got)
	}
}

// TestBuildSystemPromptIncludesPriorRunsAfterProjectGuidance pins
// the slot order: lineage lands after project AGENTS.md guidance so
// the agent reads project ground rules first, then run-specific
// history. Sequence is checked by substring index, not exact string
// match, so the test stays robust to prose edits.
func TestBuildSystemPromptIncludesPriorRunsAfterProjectGuidance(t *testing.T) {
	root := newTestBureaucracy(t)
	writePriorRun(t, root, "tele", "fix-it", run.StatusClosed, "",
		[]string{"design"}, false)
	md := &run.Metadata{
		ID:       "fix-it-2",
		Project:  "tele",
		Workflow: "sdlc",
		ReopenOf: "fix-it",
	}
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, "AGENTS.md"), []byte("stdlib only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := buildSystemPrompt(root, md, "code", clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	projIdx := strings.Index(got, "## Project guidance")
	priorIdx := strings.Index(got, "## Prior runs")
	if projIdx < 0 || priorIdx < 0 {
		t.Fatalf("missing sections; project=%d prior=%d in:\n%s", projIdx, priorIdx, got)
	}
	if !(projIdx < priorIdx) {
		t.Errorf("project guidance must precede prior runs; project=%d prior=%d",
			projIdx, priorIdx)
	}
}
