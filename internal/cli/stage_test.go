package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/wiki"
)

// newTestBureaucracy initializes a throwaway git repo with scoped git config,
// so commits can happen without polluting ~/.gitconfig. Returns the root path.
func newTestBureaucracy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}

// TestEmbeddedFragmentsCoverRegisteredStages is the load-bearing
// coverage check. For every registered (workflow, stage) pair that opens
// an agent session, the embedded FS must carry a non-empty fragment.
// Adding a new session stage without a fragment, or typoing the embed
// directory name, becomes a failing test here rather than a silent
// "prompt lost its stage lens" regression at runtime.
//
// Stages listed in noFragmentStages are operational (e.g. push), don't
// build a system prompt, and are exempt by design.
func TestEmbeddedFragmentsCoverRegisteredStages(t *testing.T) {
	// `push` is operational (no stage session). `idea` and `intent`
	// never enter a stage session either — both are editor-backed — so
	// no per-stage fragment is shipped for them.
	noFragmentStages := map[string]bool{"push": true, "idea": true, "intent": true}
	for _, wfName := range WorkflowNames() {
		// Other tests register throwaway workflows with a "test-"
		// prefix to exercise the missing-fragment fallback; by design
		// those don't ship fragments, so skip them here.
		if strings.HasPrefix(wfName, "test-") {
			continue
		}
		wf, err := LookupWorkflow(wfName)
		if err != nil {
			t.Fatalf("lookup %q: %v", wfName, err)
		}
		for _, stage := range wf.Stages() {
			if noFragmentStages[stage] {
				continue
			}
			got := moe.Stage(wfName, stage)
			if got == "" {
				t.Errorf("missing embedded fragment for workflow=%q stage=%q", wfName, stage)
			}
		}
	}
}

// TestEmbeddedSoulIsNonEmpty catches a busted //go:embed directive on
// soul.md — trivial to check, would otherwise degrade silently.
func TestEmbeddedSoulIsNonEmpty(t *testing.T) {
	if moe.Soul() == "" {
		t.Fatal("moe.Soul() is empty; //go:embed soul.md likely broken")
	}
}

// TestBuildSystemPromptInjectsSdlcDesignFragment is the end-to-end
// wiring check: the real sdlc/design.md fragment should land in the
// prompt when the run names the sdlc workflow. Uses a known
// heading as the sentinel so the assertion survives minor body edits
// (and breaks loudly if the heading itself is renamed, which is the
// point — renaming the heading is a signal the framing changed).
func TestBuildSystemPromptInjectsSdlcDesignFragment(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: design") {
		t.Fatalf("prompt missing design fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n") {
		t.Fatalf("prompt missing fragment separator:\n%s", got)
	}
	// Inlined from the former stages/_shared/cross-run.md. sdlc/design
	// has no prior stage, so the "Before you start" block is not
	// inlined here — its absence is the deliberate fix for the stale
	// block that used to fire on design.
	if !strings.Contains(got, "## Only edit this run") {
		t.Errorf("design fragment missing inlined cross-run block:\n%s", got)
	}
	if strings.Contains(got, "## Before you start") {
		t.Errorf("design fragment should not carry a 'Before you start' block (no prior stage):\n%s", got)
	}
}

func TestBuildSystemPromptInjectsSdlcCodeFragment(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: code") {
		t.Fatalf("prompt missing code fragment heading:\n%s", got)
	}
	// Both formerly-shared blocks are now inlined into stages/sdlc/code.md.
	if !strings.Contains(got, "## Before you start") {
		t.Errorf("code fragment missing inlined completeness block:\n%s", got)
	}
	if !strings.Contains(got, "## Only edit this run") {
		t.Errorf("code fragment missing inlined cross-run block:\n%s", got)
	}
}

func TestSdlcTestFragmentAllowsScopedMoeInvocations(t *testing.T) {
	got := moe.Stage("sdlc", "test")
	want := "When the target project is moe itself, this stage may\n  run `moe` for those scoped end-to-end checks"
	if !strings.Contains(got, want) {
		t.Fatalf("test fragment should explicitly allow scoped moe invocations during test stage:\n%s", got)
	}
}

// TestSdlcDesignFragmentNamesBakedCanvasReviewNote pins the baked-seed
// branch of the design fragment: when the canvas already carries a real
// design (promoted idea, reopened run, upstream seed) and the agent
// judges it complete, the stage instructs it to append a `## Design
// review` note instead of exiting on an unchanged canvas. The
// commit-time gate refuses no-op turns; without this guidance a headless
// design agent reading a baked seed has nowhere to go but the gate.
func TestSdlcDesignFragmentNamesBakedCanvasReviewNote(t *testing.T) {
	got := moe.Stage("sdlc", "design")
	for _, want := range []string{
		"## Resumed or seeded designs",
		"## Design review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("design fragment missing %q:\n%s", want, got)
		}
	}
}

// TestSdlcOneShotNamesBakedDesignReviewNote pins the headless addendum:
// the one-shot fragment must echo the design-stage rule that a baked
// canvas still needs a `## Design review` edit on success. Without the
// reminder a headless cascade can read a complete seed and exit on the
// unchanged canvas, stopping the chain at design with no recorded
// review.
func TestSdlcOneShotNamesBakedDesignReviewNote(t *testing.T) {
	got := moe.OneShot("sdlc")
	if !strings.Contains(got, "## Design review") {
		t.Fatalf("sdlc oneshot fragment should name the ## Design review note for baked designs:\n%s", got)
	}
}

// TestBuildSystemPromptMissingFragmentIsNotAnError registers a
// throwaway workflow with a stage that has no embedded fragment and
// confirms buildSystemPrompt still returns (no error, no ghost empty
// section). Soul, the stage-location header, the followups nudge, and
// the operational core are all unconditional for a registered stage,
// so we expect four sections joined by three separators
// (soul → location → followups → core). A regression that re-introduced
// an empty fragment insert would push the count to four in a row.
func TestBuildSystemPromptMissingFragmentIsNotAnError(t *testing.T) {
	root := newTestBureaucracy(t)
	wf := registerThrowawayWorkflow(t, "noFragment")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: wf.Name}
	got, err := buildSystemPrompt(root, md, "ghost", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "Your canvas for this document") {
		t.Fatalf("core prompt missing:\n%s", got)
	}
	if !strings.Contains(got, "## Stage location") {
		t.Fatalf("stage-location header missing:\n%s", got)
	}
	// Four sections (soul, location, followups, core) → three separators.
	// If Stage() had leaked an empty section we'd see four.
	if strings.Count(got, "\n---\n") != 3 {
		t.Fatalf("expected exactly three separators (soul→location→followups→core), got %d:\n%s",
			strings.Count(got, "\n---\n"), got)
	}
}

func TestBuildSystemPromptOrdersSoulBeforeStageBeforeOperational(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sentinels: soul.md heading, stage heading, first line of
	// operationalCore. All three must appear in order.
	soulIdx := strings.Index(got, "# Soul")
	stageIdx := strings.Index(got, "# Stage: design")
	opIdx := strings.Index(got, "You are collaborating")
	if soulIdx < 0 || stageIdx < 0 || opIdx < 0 {
		t.Fatalf("missing section(s) soul=%d stage=%d op=%d in:\n%s", soulIdx, stageIdx, opIdx, got)
	}
	if !(soulIdx < stageIdx && stageIdx < opIdx) {
		t.Fatalf("expected soul < stage < operational, got soul=%d stage=%d op=%d", soulIdx, stageIdx, opIdx)
	}
}

// assertPromptSectionsEndWithNewline pins the load-bearing invariant
// of every "\n---\n\n" section join across the cli package: every
// non-empty section must end with "\n", or the separator collides
// with the section's last byte and renders mid-line instead of as a
// section break. Four builders share the same join idiom
// (buildSystemPrompt + reflect/idea/lint); this is the
// shared assertion they all call into.
//
// minSections is the floor of expected chunks after splitting on the
// separator. A floor (rather than an exact count) lets a future
// section addition surface here without making count drift the
// failure mode — the per-chunk newline check is the actual contract.
func assertPromptSectionsEndWithNewline(t *testing.T, got string, minSections int) {
	t.Helper()
	chunks := strings.Split(got, "\n---\n\n")
	if len(chunks) < minSections {
		t.Fatalf("expected at least %d sections joined by separator, got %d in:\n%s",
			minSections, len(chunks), got)
	}
	for i, chunk := range chunks {
		if chunk == "" {
			continue
		}
		if !strings.HasSuffix(chunk, "\n") {
			tail := chunk
			if len(tail) > 48 {
				tail = "..." + tail[len(tail)-48:]
			}
			t.Errorf("section %d missing trailing newline; tail = %q", i, tail)
		}
	}
}

// TestBuildSystemPromptSectionsEndWithNewline is the originating
// caller of assertPromptSectionsEndWithNewline. Every existing
// optional section is wired in (soul, stage, twin reference,
// operational core, wiki ingest); the upstream-change banner has its
// own dedicated tests above and would require a prereq+prior-turn
// fixture to fire here for marginal coverage.
func TestBuildSystemPromptSectionsEndWithNewline(t *testing.T) {
	root := newTestBureaucracy(t)

	// digital-twin/<project>/ with one managed doc on disk →
	// TwinReferenceSectionAt returns a non-empty section.
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Closed-schema wiki config → IngestPromptSection fires. Closed
	// requires a non-empty ManagedDocs set; the contents don't matter
	// for this test, only that the section is emitted.
	wikiCfg := &wiki.Config{
		Name:            "twin",
		Mode:            wiki.Closed,
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "what this is."},
		},
	}

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", wikiCfg)
	if err != nil {
		t.Fatal(err)
	}

	// Five sections expected: soul, stage, twin reference, operational
	// core, wiki ingest.
	assertPromptSectionsEndWithNewline(t, got, 5)
}

func TestBannerFiresWhenPrereqDocMovedAfterWorkTurn(t *testing.T) {
	root := newTestBureaucracy(t)

	runID := "fix-it"
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	// First turn on design, then on code, then design is touched again.
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", t0)
	workSHA := trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "code", t0.Add(10*time.Second))
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", t0.Add(20*time.Second))

	md := &run.Metadata{ID: runID, Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `Since your last turn on "code"`) {
		t.Errorf("expected banner header, got:\n%s", got)
	}
	if !strings.Contains(got, workSHA) {
		t.Errorf("banner missing last-turn SHA %q:\n%s", workSHA, got)
	}
	relPath := run.ContentPath("tele", runID, "design")
	if !strings.Contains(got, relPath) {
		t.Errorf("banner missing prereq content path %q:\n%s", relPath, got)
	}
	if !strings.Contains(got, "git -C "+root+" diff "+workSHA+"..HEAD -- "+relPath) {
		t.Errorf("banner missing usable diff command:\n%s", got)
	}
}

func TestBannerSilentBeforeFirstWorkTurn(t *testing.T) {
	root := newTestBureaucracy(t)

	runID := "fix-it"
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &run.Metadata{ID: runID, Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("did not expect banner before first work turn on code:\n%s", got)
	}
}

func TestBannerSilentWhenPrereqDocMovedBeforeLastTurn(t *testing.T) {
	root := newTestBureaucracy(t)

	runID := "fix-it"
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", t0.Add(10*time.Second)) // another design turn before any code
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "code", t0.Add(20*time.Second))

	md := &run.Metadata{ID: runID, Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("banner should not fire when prereq moved before last turn:\n%s", got)
	}
}

func TestBannerSilentAtDesignStage(t *testing.T) {
	root := newTestBureaucracy(t)

	runID := "fix-it"
	// Design has no prereqs in prereqDocs. Even with a prior work turn,
	// there's nothing to surface.
	trailerstest.CommitWorkTurnAt(t, root, "tele", runID, "sdlc", "design", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &run.Metadata{ID: runID, Project: "tele", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("banner should not fire for a doc with no prereqs:\n%s", got)
	}
}

func TestRunStageSessionBannerShowsResolvedAgent(t *testing.T) {
	cases := []struct {
		name      string
		explicit  string
		persisted string
		env       string
		wantAgent string
	}{
		{
			name:      "hard default",
			wantAgent: "claude",
		},
		{
			name:      "persisted default",
			persisted: "codex",
			wantAgent: "codex",
		},
		{
			name:      "explicit override wins",
			explicit:  "codex",
			persisted: "claude",
			env:       "claude",
			wantAgent: "codex",
		},
		{
			name:      "environment fallback",
			env:       "codex",
			wantAgent: "codex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			gittest.Run(t, root, "add", "bureaucracy.conf")
			gittest.Run(t, root, "commit", "-m", "mark bureaucracy root")
			t.Setenv("MOE_HOME", root)
			t.Setenv("MOE_AGENT", tc.env)
			md := trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
			if tc.persisted != "" {
				md.Agent = tc.persisted
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "add", filepath.Join(run.Dir(md.Project, md.ID), "run.json"))
				gittest.Run(t, root, "commit", "-m", "set run agent")
			}

			var stdout, stderr bytes.Buffer
			code := runStageSession("tele", "fix-it", "design", stageSessionOpts{
				Agent: tc.explicit,
				WikiBuilder: func(root string, md *run.Metadata) (*wiki.Config, error) {
					return nil, errors.New("stop before executor")
				},
			}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("runStageSession unexpectedly succeeded; stderr=%q", stderr.String())
			}
			want := "▓▒░ MINISTRY OF EVERYTHING ░▒▓  [" + tc.wantAgent + "] sdlc · design  ·  tele/fix-it\n"
			if got := stdout.String(); !strings.HasPrefix(got, want) {
				t.Fatalf("stdout prefix = %q, want %q (stderr=%q)", got, want, stderr.String())
			}
		})
	}
}

// TestCommitSessionStartWritesTrailersAndKeepsTreeClean is the core
// property commitSessionStart was introduced to guarantee: after
// EnsureDocument mints a fresh session and the metadata is saved, the
// eager commit lands on HEAD with the standard MoE trailer block and
// the working tree reaches a clean state (no dirty run.json sitting
// around for the duration of the Claude run).
func TestCommitSessionStartWritesTrailersAndKeepsTreeClean(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	doc, mutated, err := run.EnsureDocument(root, md, "design")
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatalf("expected EnsureDocument to mutate on fresh document")
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	subject := gitLogFormat(t, root, 1, "HEAD", "%s")
	if subject != "work: start session for design" {
		t.Errorf("subject = %q, want %q", subject, "work: start session for design")
	}
	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	for _, want := range []string{
		"MoE-Run: fix-it",
		"MoE-Project: tele",
		"MoE-Document: design",
		"MoE-Session: " + doc.Session,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commit body missing %q:\n%s", want, body)
		}
	}

	if entries, err := git.Status(root); err != nil {
		t.Fatalf("git status: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("expected clean tree after eager commit, got:\n%v", entries)
	}
}

// TestCommitSessionStartLeavesUnrelatedDirtyFilesAlone is the other
// half of the contract: the eager commit is scoped to run.json, so an
// operator who had stray edits in their tree before launching the
// stage keeps those edits — they are neither staged nor committed.
func TestCommitSessionStartLeavesUnrelatedDirtyFilesAlone(t *testing.T) {
	root := newTestBureaucracy(t)

	stray := filepath.Join(root, "stray.txt")
	if err := os.WriteFile(stray, []byte("operator WIP\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	entries, err := git.Status(root)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	// Stray file should still be untracked; nothing else should be dirty.
	if len(entries) != 1 || entries[0].XY != "??" || entries[0].Path != "stray.txt" {
		t.Errorf("unexpected status after eager commit: %v", entries)
	}

	// And HEAD should only mention run.json, not stray.txt.
	names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
	wantPath := filepath.Join("projects", "tele", "runs", "fix-it", "run.json")
	if names != wantPath {
		t.Errorf("HEAD files = %q, want %q", names, wantPath)
	}
}

// TestCommitSessionStartRegeneratesUUIDForLegacyDocument covers the
// "invalid session id" branch of EnsureDocument: a legacy Document
// entry with an empty / malformed Session gets a new UUID, mutated=true,
// and the eager commit carries the freshly minted UUID in its trailer.
func TestCommitSessionStartRegeneratesUUIDForLegacyDocument(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{
			"design": {Session: "not-a-uuid"},
		},
	}
	doc, mutated, err := run.EnsureDocument(root, md, "design")
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatalf("expected EnsureDocument to re-mint legacy session id")
	}
	if doc.Session == "not-a-uuid" || doc.Session == "" {
		t.Fatalf("Session not refreshed: %q", doc.Session)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	if !strings.Contains(body, "MoE-Session: "+doc.Session) {
		t.Errorf("trailer missing freshly minted session %q:\n%s", doc.Session, body)
	}
}

// TestCommitSessionStartFollowedByCommitTurnYieldsTwoDistinctCommits is
// the composition check: on a first turn, the eager start-session
// commit plus the closing commitTurn commit produce two commits on
// HEAD with distinct subjects. Mirrors the intended runtime sequence
// without dragging in the executor.
func TestCommitSessionStartFollowedByCommitTurnYieldsTwoDistinctCommits(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	// Simulate the agent writing content.md mid-session.
	contentRel := run.ContentPath("tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitTurn(root, md, "design"); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	log := gitLogFormat(t, root, 2, "HEAD", "%s")
	subjects := strings.Split(strings.TrimSpace(log), "\n")
	// git log is newest-first.
	want := []string{"work: update design", "work: start session for design"}
	if len(subjects) != len(want) || subjects[0] != want[0] || subjects[1] != want[1] {
		t.Errorf("subjects = %v, want %v", subjects, want)
	}
}

// TestSecondTurnOnExistingDocumentSkipsEagerCommit guards the other
// side of the `if mutated` gate in runStageSession: once a document
// has a valid session UUID committed, EnsureDocument no longer
// mutates, so a subsequent turn produces only the closing
// `work: update` commit — no duplicate `work: start session` commit
// per turn.
func TestSecondTurnOnExistingDocumentSkipsEagerCommit(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}
	// First turn lands a bit of content via commitTurn.
	contentRel := run.ContentPath("tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitTurn(root, md, "design"); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	// Second turn: EnsureDocument should NOT mutate; mirror the
	// `if mutated { commitSessionStart }` gate by simply not calling
	// commitSessionStart on this path. Then the agent writes, and
	// commitTurn is the only new commit.
	_, mutated, err := run.EnsureDocument(root, md, "design")
	if err != nil {
		t.Fatal(err)
	}
	if mutated {
		t.Fatalf("expected mutated=false on second turn, got true")
	}
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := gitLogFormat(t, root, 1, "HEAD", "%H")
	if err := commitTurn(root, md, "design"); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}
	headAfter := gitLogFormat(t, root, 1, "HEAD", "%H")
	if headBefore == headAfter {
		t.Fatal("expected commitTurn to add a commit on second turn")
	}
	// Exactly one new commit, and its subject is `work: update …`.
	subj := gitLogFormat(t, root, 1, "HEAD", "%s")
	if subj != "work: update design" {
		t.Errorf("second-turn HEAD subject = %q, want %q", subj, "work: update design")
	}
	// HEAD~1 must still be the first-turn update, not a duplicate start-session.
	prev := gitLogFormat(t, root, 1, "HEAD~1", "%s")
	if prev != "work: update design" {
		t.Errorf("HEAD~1 subject = %q, want %q (no eager commit on second turn)", prev, "work: update design")
	}
}

// TestCommitTurnRequiresCanvas guards the post-stage assertion: a turn
// that produced a thread file but no content.md must fail loudly
// rather than silently committing a transcript-only snapshot. This is
// the failure mode the missing-canvas-doc run was opened against.
func TestCommitTurnRequiresCanvas(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	// Simulate the failure mode: the agent's thread is mirrored but no
	// content.md is ever written.
	threadRel := run.ThreadPathFor("claude", "tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, threadRel), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	headBefore := gitLogFormat(t, root, 1, "HEAD", "%H")
	err := commitTurn(root, md, "design")
	if err == nil {
		t.Fatal("commitTurn returned nil, want error about missing canvas")
	}
	canvasRel := run.ContentPath("tele", "fix-it", "design")
	if !strings.Contains(err.Error(), canvasRel) {
		t.Errorf("error %q does not mention canvas path %q", err.Error(), canvasRel)
	}
	if headAfter := gitLogFormat(t, root, 1, "HEAD", "%H"); headBefore != headAfter {
		t.Fatalf("commitTurn created a commit despite missing canvas: %s -> %s", headBefore, headAfter)
	}
}

// TestCommitTurnRejectsEmptyCanvas covers the size==0 branch: a
// content.md that exists but is empty is treated the same as missing,
// since the agent has nothing to show for the turn.
func TestCommitTurnRejectsEmptyCanvas(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	canvasRel := run.ContentPath("tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, canvasRel), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	headBefore := gitLogFormat(t, root, 1, "HEAD", "%H")
	err := commitTurn(root, md, "design")
	if err == nil {
		t.Fatal("commitTurn returned nil, want error about empty canvas")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not mention empty canvas", err.Error())
	}
	if headAfter := gitLogFormat(t, root, 1, "HEAD", "%H"); headBefore != headAfter {
		t.Fatalf("commitTurn created a commit despite empty canvas: %s -> %s", headBefore, headAfter)
	}
}

// TestCommitTurnNoOpTurnReturnsErrNothingToCommit pins the no-op
// path: a second turn that doesn't touch the canvas, run.json, or
// followups must return ErrNothingToCommit and leave HEAD untouched.
// run.Save now runs unconditionally inside commitTurn (it used to be
// gated behind a HasStagedChanges check); this guards the byte-stable
// rewrite — an unchanged Metadata produces the same bytes, git add
// finds no diff, and StageAndCommit's internal check refuses cleanly.
func TestCommitTurnNoOpTurnReturnsErrNothingToCommit(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}
	contentRel := run.ContentPath("tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitTurn(root, md, "design"); err != nil {
		t.Fatalf("first commitTurn: %v", err)
	}

	headBefore := gitLogFormat(t, root, 1, "HEAD", "%H")
	err := commitTurn(root, md, "design")
	if !errors.Is(err, run.ErrNothingToCommit) {
		t.Fatalf("commitTurn err = %v, want ErrNothingToCommit", err)
	}
	if headAfter := gitLogFormat(t, root, 1, "HEAD", "%H"); headBefore != headAfter {
		t.Fatalf("no-op commitTurn advanced HEAD: %s -> %s", headBefore, headAfter)
	}
}

// TestReportWikiSessionExitNonZeroOnFinalizeError pins the contract
// the twin-dash-never-reflected-bug run was opened against: when
// FinalizeIngest fails, the session exits non-zero so the operator
// notices, but the per-turn commit is still reported as having
// landed. Before the fix, finalize errors only logged a stderr line
// and the session exited 0 — silently letting checkpoint.json /
// log.md drift from disk and producing the dash's "never reflected"
// misreport hours later.
func TestReportWikiSessionExitNonZeroOnFinalizeError(t *testing.T) {
	in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "reflect"}
	finalizeErr := errors.New("wiki: closed-schema has unexpected top-level doc history-summary.md")
	var stdout, stderr bytes.Buffer
	code := reportWikiSessionExit(in, nil, nil, nil, finalizeErr, nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 on finalize error", code)
	}
	// Per-turn commit succeeded (commitErr nil), so the operator still
	// sees the "committed turn" line — finalize failure is loud but
	// doesn't masquerade as a commit failure.
	if !strings.Contains(stdout.String(), "committed reflect turn for moe/r") {
		t.Errorf("stdout missing committed-turn line: %q", stdout.String())
	}
}

// TestReportWikiSessionExitZeroOnHappyPath is the negative control:
// no errors → exit 0. Without it the previous test could pass
// trivially against a function that always returns 1.
func TestReportWikiSessionExitZeroOnHappyPath(t *testing.T) {
	in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "reflect"}
	var stdout, stderr bytes.Buffer
	code := reportWikiSessionExit(in, nil, nil, nil, nil, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 on clean run", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr non-empty on clean run: %q", stderr.String())
	}
}

// TestReportWikiSessionExitGateBlocksWithoutCommit pins reflect's
// post-flight gate contract: a non-nil gateErr forces exit 1, the
// "no commit happened" branch fires (we deliberately skipped both
// FinalizeIngest and CommitStager), and the misleading "committed
// turn" line is suppressed. Catches a regression where a future
// refactor wires the gate up but forgets to teach the exit reporter
// that gateErr means "no commit was attempted" rather than "commit
// succeeded."
func TestReportWikiSessionExitGateBlocksWithoutCommit(t *testing.T) {
	in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "reflect"}
	gateErr := errors.New("reflect: post-flight scan found 2 unresolved findings")
	var stdout, stderr bytes.Buffer
	code := reportWikiSessionExit(in, nil, nil, nil, nil, gateErr, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 when gate fires", code)
	}
	if strings.Contains(stdout.String(), "committed reflect turn for moe r") {
		t.Errorf("gate fired but stdout claims commit landed: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "no document changes") {
		t.Errorf("gate fired but stdout claims no-op: %q", stdout.String())
	}
}

// TestReportWikiSessionExitNamesAgentInExitLine pins the silent-
// failure-at-push fix: when codex is the dispatched agent and its
// turn fails, the run-error stderr line must name codex, not claude.
// The bug it guards against: a hardcoded "claude exited:" lying to
// the operator about which agent died (and burying the failure
// under a misleading attribution).
func TestReportWikiSessionExitNamesAgentInExitLine(t *testing.T) {
	cases := []struct {
		name      string
		agent     string
		wantLabel string
	}{
		{name: "codex run", agent: "codex", wantLabel: "codex exited:"},
		{name: "claude run", agent: "claude", wantLabel: "claude exited:"},
		{name: "unresolved falls back to agent", agent: "", wantLabel: "agent exited:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "push", Agent: tc.agent}
			runErr := errors.New("turn.failed")
			var stdout, stderr bytes.Buffer
			code := reportWikiSessionExit(in, runErr, nil, nil, nil, nil, &stdout, &stderr)
			if code != 1 {
				t.Errorf("exit code = %d, want 1 on run error", code)
			}
			if !strings.Contains(stderr.String(), tc.wantLabel) {
				t.Errorf("stderr missing %q; got %q", tc.wantLabel, stderr.String())
			}
			// And the misleading hardcoded label must not slip through
			// for non-claude agents.
			if tc.agent == "codex" && strings.Contains(stderr.String(), "claude exited:") {
				t.Errorf("codex run still surfaced as 'claude exited:': %q", stderr.String())
			}
		})
	}
}

// TestReportWikiSessionExitInterruptedReturns130 pins the interrupt
// classification: an operator Ctrl-C surfaces as agent.ErrInterrupted in
// runErr, and reportWikiSessionExit must exit 130 (exitInterrupted), not
// the bare 1 a failed turn returns — that distinct code is what lets the
// cascade halt the chain instead of mistaking the interrupt for a stage
// failure. The turn's commit is kept (commitErr nil here), so the
// "committed turn" line still prints: the work is on disk, push is
// suppressed upstream, the run stays at its stage.
func TestReportWikiSessionExitInterruptedReturns130(t *testing.T) {
	in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "test", Agent: "claude"}
	var stdout, stderr bytes.Buffer
	// Wrap the sentinel to prove errors.Is, not ==, is the check —
	// runErr threads through several layers before reaching here.
	runErr := fmt.Errorf("execute turn: %w", agent.ErrInterrupted)
	code := reportWikiSessionExit(in, runErr, nil, nil, nil, nil, &stdout, &stderr)
	if code != exitInterrupted {
		t.Errorf("exit code = %d, want %d (exitInterrupted) on operator Ctrl-C", code, exitInterrupted)
	}
	if !strings.Contains(stdout.String(), "committed test turn for moe/r") {
		t.Errorf("interrupted turn must keep its commit; stdout missing committed-turn line: %q", stdout.String())
	}
	// A genuine non-interrupt failure must still be the bare 1 — the
	// negative control so the test can't pass against a function that
	// always returns 130.
	plain := reportWikiSessionExit(in, errors.New("turn.failed"), nil, nil, nil, nil, &stdout, &stderr)
	if plain != 1 {
		t.Errorf("exit code = %d, want 1 on an ordinary run failure", plain)
	}
}

// TestReportWikiSessionExitNothingToCommitIsCleanExit guards the
// "no document changes" branch: the operator opens the session,
// looks around, exits without edits. ErrNothingToCommit is reported
// to stdout, exit is 0, and a finalize-error-style fallthrough
// doesn't accidentally promote it to non-zero.
func TestReportWikiSessionExitNothingToCommitIsCleanExit(t *testing.T) {
	in := wikiSessionInputs{Project: "moe", RunSlug: "r", DocID: "reflect"}
	var stdout, stderr bytes.Buffer
	code := reportWikiSessionExit(in, nil, run.ErrNothingToCommit, nil, nil, nil, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 on nothing-to-commit", code)
	}
	if !strings.Contains(stdout.String(), "no document changes") {
		t.Errorf("stdout missing nothing-to-commit line: %q", stdout.String())
	}
}

// TestRunWikiSessionFailsFastOnBootstrapError pins the contract this
// run was opened to restore: when EnsureManagedDocs returns a real
// error (here, the synchronous "closed-schema requires ManagedDocs to
// be non-empty" guard at bootstrap.go:24), runWikiSession must surface
// it as exit 1, tear the session worktree down via closeSess, and
// never reach the executor / commit / finalize. Before the fix, the
// error was logged to stderr and the session continued anyway, so the
// operator saw a downstream invariant breach at finalize instead of
// the bootstrap root cause.
func TestRunWikiSessionFailsFastOnBootstrapError(t *testing.T) {
	root := newTestBureaucracy(t)

	var reachedAfterBootstrap bool
	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "bootstrap-fail",
		DocID:       "design",
		LockPurpose: "stage",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			// Closed-schema with empty ManagedDocs is the cleanest
			// trigger: bootstrap returns the error before any I/O,
			// so the test doesn't need permission games.
			return &wiki.Config{
				Name:            "twin",
				Mode:            wiki.Closed,
				ContentDir:      filepath.Join(canonicalRoot, "projects", "moe", "twin"),
				BureaucracyPath: canonicalRoot,
			}, nil
		},
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			return wikiTurnSpec{
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					reachedAfterBootstrap = true
					return "", errors.New("BuildPrompt should not be reached after bootstrap failure")
				},
				CommitStager: func(workRoot, wikiRel string) error {
					reachedAfterBootstrap = true
					return nil
				},
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := runWikiSession(root, in, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 on bootstrap failure (stderr=%q)", code, stderr.String())
	}
	if reachedAfterBootstrap {
		t.Error("session continued past failed bootstrap; fail-fast didn't fire")
	}
	if !strings.Contains(stderr.String(), "ManagedDocs to be non-empty") {
		t.Errorf("stderr missing bootstrap root cause: %q", stderr.String())
	}
	// closeSess fires, but its canvas-unchanged gate refuses the
	// fast-forward (the session never wrote a canvas — same shape
	// as the cascade footgun this run targets) and leaves the
	// worktree intact so the operator can recover. The previous
	// silent-Abandon behavior on zero-commit branches is by design
	// gone — the operator should see the loud refusal alongside the
	// bootstrap root cause.
	if !strings.Contains(stderr.String(), "unchanged from main") {
		t.Errorf("stderr missing canvas-unchanged refusal from closeSess: %q", stderr.String())
	}
	branch := "session/moe/bootstrap-fail/design"
	out := gittest.Output(t, root, "worktree", "list")
	if !strings.Contains(out, branch) {
		t.Errorf("worktree for %s should remain after canvas-unchanged refusal:\n%s", branch, out)
	}
}

// TestRunWikiSessionBuildsInitialPromptAgainstWorktree pins the fix the
// twin-pooped-in-bureaucracy run was opened against: a deferred
// InitialPromptBuilder must receive the *worktree-rewritten* wiki cfg,
// not the canonical one. Reflect bakes absolute bureaucracy paths (the
// history summary, the managed docs) into the agent's first instruction;
// before this fix the kickoff was assembled against the canonical root
// before the worktree existed, the agent followed the canonical path,
// and a reflect pass edited the operator's live checkout. The builder
// now runs after the rewrite, so the cfg it sees must point inside
// <root>/.moe/worktrees/<uuid>/ and never at the canonical content dir.
func TestRunWikiSessionBuildsInitialPromptAgainstWorktree(t *testing.T) {
	root := newTestBureaucracy(t)
	canonicalContentDir := filepath.Join(root, "projects", "moe", "digital-twin")

	var gotWorkRoot string
	var gotCfg *wiki.Config
	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "kickoff-binding",
		DocID:       "vision",
		LockPurpose: "stage",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			// Open schema keeps EnsureManagedDocs a no-op, so the turn
			// reaches the builder without managed-doc fixtures.
			return &wiki.Config{
				Name:            "twin",
				Mode:            wiki.Open,
				ContentDir:      canonicalContentDir,
				BureaucracyPath: canonicalRoot,
			}, nil
		},
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			return wikiTurnSpec{
				InitialPromptBuilder: func(workRoot string, worktreeWiki *wiki.Config, stubbed bool) (string, error) {
					gotWorkRoot = workRoot
					gotCfg = worktreeWiki
					return "kickoff", nil
				},
				// Fail fast right after the builder captures so the test
				// never needs a real agent / executor.
				BuildPrompt: func(string, *wiki.Config) (string, error) {
					return "", errors.New("stop before executor")
				},
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	runWikiSession(root, in, &stdout, &stderr)

	if gotCfg == nil {
		t.Fatalf("InitialPromptBuilder never ran (stderr=%q)", stderr.String())
	}
	if !strings.Contains(gotWorkRoot, filepath.Join(".moe", "worktrees")) {
		t.Errorf("builder workRoot %q is not a session worktree", gotWorkRoot)
	}
	// The whole point: the cfg the builder renders against lives inside
	// the worktree, not the operator's canonical checkout.
	if gotCfg.ContentDir == canonicalContentDir {
		t.Errorf("builder got the canonical ContentDir %q; want the worktree copy", gotCfg.ContentDir)
	}
	if !strings.HasPrefix(gotCfg.ContentDir, gotWorkRoot) {
		t.Errorf("builder ContentDir %q not under worktree %q", gotCfg.ContentDir, gotWorkRoot)
	}
	if gotCfg.BureaucracyPath != gotWorkRoot {
		t.Errorf("builder BureaucracyPath = %q, want worktree root %q", gotCfg.BureaucracyPath, gotWorkRoot)
	}
}

// TestSessionDocCwdIsStableAcrossTurns is the regression for this run:
// document-only stages must hand claude a cwd that's identical across
// turns, so the encoded-cwd project dir under ~/.claude/projects/ stays
// the same and `--resume <sid>` finds the JSONL it wrote on turn 1. Two
// calls with the same (root, project, run, doc) must return the same
// path; the path must live under <root>/.moe/sessions/ rather than the
// per-turn session worktree (which churns a UUID and was the source of
// the bug). Drives the helper directly because the executor seam (real
// `claude` subprocess) isn't available in tests — a stable helper plus
// the field-threading edits in BuildSpec/Execute are the entire fix.
func TestSessionDocCwdIsStableAcrossTurns(t *testing.T) {
	root := t.TempDir()
	turn1 := sessionDocCwd(root, "tele", "fix-it", "design")
	turn2 := sessionDocCwd(root, "tele", "fix-it", "design")
	if turn1 != turn2 {
		t.Fatalf("session cwd not stable across turns: turn1=%q turn2=%q", turn1, turn2)
	}
	want := filepath.Join(root, ".moe", "sessions", "tele", "fix-it", "design")
	if turn1 != want {
		t.Fatalf("session cwd shape = %q, want %q", turn1, want)
	}
	if strings.Contains(turn1, filepath.Join(".moe", "worktrees")) {
		t.Errorf("session cwd should not be under the per-turn worktree dir: %q", turn1)
	}
}

// TestSessionDocCwdDistinguishesByDoc is the negative control: distinct
// (project, run, doc) tuples must map to distinct cwds, otherwise two
// concurrent design+code sessions on the same run would share an encoded
// project dir and step on each other's `--resume` lookups.
func TestSessionDocCwdDistinguishesByDoc(t *testing.T) {
	root := t.TempDir()
	design := sessionDocCwd(root, "tele", "fix-it", "design")
	code := sessionDocCwd(root, "tele", "fix-it", "code")
	if design == code {
		t.Fatalf("doc id ignored in session cwd: %q == %q", design, code)
	}
	otherRun := sessionDocCwd(root, "tele", "other", "design")
	if otherRun == design {
		t.Fatalf("run id ignored in session cwd: %q == %q", otherRun, design)
	}
}

// The moe-bureaucracy skill teaches the agent three trace-recording
// channels: twin observations, portable lore, and followups. The
// split is the whole point — the existing dashboard pollution is
// category confusion between them — so pin all three paragraphs,
// the twin-first ordering (an agent who has already mentally drafted
// a followup never re-checks), the mechanical trigger that names the
// twin docs, and the backward link from followups that catches an
// agent who drafted there first.
//
// Previously this block lived in operationalCore and got asserted
// against the prompt directly. After the moe-bureaucracy skill
// extraction the materialised SKILL.md is the surface to pin — the
// per-turn prompt no longer carries the prose.
func TestMoeBureaucracySkillCarriesAllThreeTraceChannels(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	if err := materializeMoeBureaucracySkill(root, "", md); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, ".codex", "skills", "moe-bureaucracy", "SKILL.md"))
	if err != nil {
		t.Fatalf("read materialised skill: %v", err)
	}
	got := string(body)

	followups := filepath.Join(root, run.FollowupsPath("tele", "fix-it"))
	twin := filepath.Join(root, run.FeedbackPath("tele", "fix-it", "twin"))
	lore := filepath.Join(root, run.FeedbackPath("tele", "fix-it", "lore"))
	for _, want := range []string{followups, twin, lore} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing path %q:\n%s", want, got)
		}
	}
	// Ordering: twin first, lore second, followups last. Reordering
	// is a deliberate change — pin by path position so a regression
	// shows up as a failing test.
	ti := strings.Index(got, twin)
	li := strings.Index(got, lore)
	fi := strings.Index(got, followups)
	if !(ti >= 0 && li >= 0 && fi >= 0 && ti < li && li < fi) {
		t.Errorf("expected twin < lore < followups ordering; got twin=%d lore=%d followups=%d:\n%s", ti, li, fi, got)
	}
	// Mechanical trigger: enumerate the twin docs the agent should
	// recognize. Philosophical phrasing ("a decision the doc doesn't
	// reflect") is what failed in claim-seems-broken; the names of
	// the actual files are the load-bearing cue.
	for _, doc := range []string{"architecture.md", "vision.md", "patterns.md", "operations.md", "glossary.md"} {
		if !strings.Contains(got, doc) {
			t.Errorf("twin trigger missing twin doc name %q:\n%s", doc, got)
		}
	}
	// Backward link from the followups paragraph closes the
	// asymmetric-redirect hole: an agent who reads only the
	// followups paragraph still gets sent to feedback/twin.md.
	if !strings.Contains(got, "feedback/twin.md") {
		t.Errorf("followups paragraph missing backward link to feedback/twin.md:\n%s", got)
	}
}

// commitTurn stages feedback/*.md alongside followups.md when a stage
// turn lands. Without this the agent's twin-note would sit on disk in
// the sandbox clone and never reach the bureaucracy journal, so the
// next reflect's loadTwinFeedback walk would never see it.
func TestCommitTurnStagesFeedbackFile(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "design"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "design"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	contentRel := run.ContentPath("tele", "fix-it", "design")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Agent drops a twin feedback note this turn.
	feedbackRel := run.FeedbackPath("tele", "fix-it", "twin")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, feedbackRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, feedbackRel), []byte("patterns.md drifted from cli/foo.go:42.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := commitTurn(root, md, "design"); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
	if !strings.Contains(names, feedbackRel) {
		t.Errorf("commit missing feedback file %q in:\n%s", feedbackRel, names)
	}
}

// stageableFeedback returns every feedback/*.md path on disk so a
// future moe.md (or any other recipient) rides the same commit without
// a code change. Pin the multi-recipient case directly.
func TestStageableFeedbackGlobsRecipients(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	dir := filepath.Join(root, run.FeedbackDir(md.Project, md.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"twin.md", "moe.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("note\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-md sibling should be ignored — the glob is intentional.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := stageableFeedback(root, md)
	wantSet := map[string]bool{
		run.FeedbackPath("tele", "fix-it", "twin"): false,
		run.FeedbackPath("tele", "fix-it", "moe"):  false,
	}
	for _, p := range got {
		if _, ok := wantSet[p]; !ok {
			t.Errorf("unexpected path %q in stageable set", p)
			continue
		}
		wantSet[p] = true
	}
	for p, seen := range wantSet {
		if !seen {
			t.Errorf("expected %q in stageable set, got %v", p, got)
		}
	}
}

// gitLogFormat runs `git log -n <n> --format=<fmt> <rev>` and returns
// the trimmed stdout — small helper so each assertion doesn't
// reimplement the exec.Command plumbing.
func gitLogFormat(t *testing.T, root string, n int, rev, format string) string {
	t.Helper()
	return gittest.Output(t, root, "log", fmt.Sprintf("-n%d", n), "--format="+format, rev)
}

// registerThrowawayWorkflow adds a one-off workflow to the package
// registry for the duration of the test run. Tests use it to probe the
// missing-fragment fallback without touching real workflows. The name
// is derived from t.Name() so parallel runs don't collide on the
// registry's duplicate-guard panic. The entry is deleted on cleanup:
// t.Name() is stable across `-count` iterations, so leaving it behind
// makes the second iteration panic on the duplicate guard.
func registerThrowawayWorkflow(t *testing.T, suffix string) *Workflow {
	t.Helper()
	name := "test-" + suffix + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	wf := NewWorkflow(name)
	wf.RegisterStage("ghost")
	RegisterWorkflow(wf)
	t.Cleanup(func() { delete(workflows, name) })
	return wf
}
