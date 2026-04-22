package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/run"
)

// newTestBureaucracy initializes a throwaway git repo with scoped git config,
// so commits can happen without polluting ~/.gitconfig. Returns the root path.
func newTestBureaucracy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}

// commitWorkTurnAt records a `work: update <docID>` commit with the trailers
// commitTurn writes in production, dated to when. Returns HEAD's SHA so the
// caller can assert it appears in the banner.
func commitWorkTurnAt(t *testing.T, root, runID, docID string, when time.Time) string {
	t.Helper()
	commitTrailer(t, root, "work: update "+docID,
		"MoE-Run: "+runID+"\nMoE-Document: "+docID, when)
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func commitTrailer(t *testing.T, root, subject, trailers string, when time.Time) {
	t.Helper()
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", subject+"\n\n"+trailers+"\n")
	cmd.Dir = root
	if !when.IsZero() {
		stamp := when.Format(time.RFC3339)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+stamp,
			"GIT_COMMITTER_DATE="+stamp,
		)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
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
	noFragmentStages := map[string]bool{"push": true}
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

// TestEmbeddedSharedFragmentsAreNonEmpty catches a regression on the
// //go:embed directive specifically: fs.Embed skips "_"-prefixed paths
// unless the directive uses the `all:` prefix. Without it, stages/_shared/
// silently disappears from the binary and every Claude-driven stage
// loses its shared guidance blocks — exactly the failure mode this
// run was added to prevent.
func TestEmbeddedSharedFragmentsAreNonEmpty(t *testing.T) {
	for _, name := range []string{"completeness", "cross-run"} {
		if got := moe.Stage("_shared", name); got == "" {
			t.Errorf("moe.Stage(%q, %q) is empty; //go:embed likely missing `all:` prefix", "_shared", name)
		}
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

	md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: design") {
		t.Fatalf("prompt missing design fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n") {
		t.Fatalf("prompt missing fragment separator:\n%s", got)
	}
}

func TestBuildSystemPromptInjectsSdlcCodeFragment(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: code") {
		t.Fatalf("prompt missing code fragment heading:\n%s", got)
	}
}

// TestBuildSystemPromptInjectsSharedFragmentsAtSdlcStages is the
// wiring check for the cross-workflow guidance blocks under
// stages/_shared/. Both fragments must land in the prompt for
// sdlc/design and sdlc/code, in a stable order (completeness before
// cross-run), and after the per-stage fragment but before the
// operational core.
func TestBuildSystemPromptInjectsSharedFragmentsAtSdlcStages(t *testing.T) {
	root := newTestBureaucracy(t)

	for _, docID := range []string{"design", "code"} {
		md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: "sdlc"}
		got, err := buildSystemPrompt(root, md, docID, "")
		if err != nil {
			t.Fatalf("%s: %v", docID, err)
		}
		stageIdx := strings.Index(got, "# Stage: "+docID)
		completeIdx := strings.Index(got, "## Before you start")
		crossRunIdx := strings.Index(got, "## Only edit this run")
		opIdx := strings.Index(got, "You are collaborating")
		if stageIdx < 0 || completeIdx < 0 || crossRunIdx < 0 || opIdx < 0 {
			t.Fatalf("%s: missing section(s) stage=%d complete=%d cross=%d op=%d in:\n%s",
				docID, stageIdx, completeIdx, crossRunIdx, opIdx, got)
		}
		if !(stageIdx < completeIdx && completeIdx < crossRunIdx && crossRunIdx < opIdx) {
			t.Fatalf("%s: expected stage < completeness < cross-run < op, got stage=%d complete=%d cross=%d op=%d",
				docID, stageIdx, completeIdx, crossRunIdx, opIdx)
		}
	}
}

// TestBuildSystemPromptOmitsSharedFragmentsOutsideSdlcDesignAndCode
// is the negative counterpart: the shared fragments are scoped to
// Claude-driven sdlc stages. Other workflows (and sdlc stages that
// aren't design or code) get no shared block.
func TestBuildSystemPromptOmitsSharedFragmentsOutsideSdlcDesignAndCode(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "brief", Project: "tele", Title: "Brief", Workflow: "kb"}
	got, err := buildSystemPrompt(root, md, "research", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "## Before you start") {
		t.Errorf("shared completeness fragment leaked into kb/research:\n%s", got)
	}
	if strings.Contains(got, "## Only edit this run") {
		t.Errorf("shared cross-run fragment leaked into kb/research:\n%s", got)
	}
}

// TestBuildSystemPromptMissingFragmentIsNotAnError registers a
// throwaway workflow with a stage that has no embedded fragment and
// confirms buildSystemPrompt still returns (no error, no ghost empty
// section). The soul section is always embedded so we still expect
// exactly one separator — between soul and the operational core —
// not two or more in a row from an empty stage insert.
func TestBuildSystemPromptMissingFragmentIsNotAnError(t *testing.T) {
	root := newTestBureaucracy(t)
	wf := registerThrowawayWorkflow(t, "noFragment")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: wf.Name}
	got, err := buildSystemPrompt(root, md, "ghost", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "Your canvas for this document") {
		t.Fatalf("core prompt missing:\n%s", got)
	}
	// Two sections (soul, core) → one separator. If Stage() had leaked
	// an empty section we'd see the separator twice in a row.
	if strings.Count(got, "\n---\n") != 1 {
		t.Fatalf("expected exactly one separator (soul→core), got %d:\n%s",
			strings.Count(got, "\n---\n"), got)
	}
}

func TestBuildSystemPromptOrdersSoulBeforeStageBeforeOperational(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "")
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

func TestBannerFiresWhenPrereqDocMovedAfterWorkTurn(t *testing.T) {
	root := newTestBureaucracy(t)

	runID := "fix-it"
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	// First turn on design, then on code, then design is touched again.
	commitWorkTurnAt(t, root, runID, "design", t0)
	workSHA := commitWorkTurnAt(t, root, runID, "code", t0.Add(10*time.Second))
	commitWorkTurnAt(t, root, runID, "design", t0.Add(20*time.Second))

	md := &run.Metadata{ID: runID, Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "")
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
	commitWorkTurnAt(t, root, runID, "design", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &run.Metadata{ID: runID, Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "")
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
	commitWorkTurnAt(t, root, runID, "design", t0)
	commitWorkTurnAt(t, root, runID, "design", t0.Add(10*time.Second)) // another design turn before any code
	commitWorkTurnAt(t, root, runID, "code", t0.Add(20*time.Second))

	md := &run.Metadata{ID: runID, Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "code", "")
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
	commitWorkTurnAt(t, root, runID, "design", time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC))

	md := &run.Metadata{ID: runID, Project: "tele", Title: "Fix it", Workflow: "sdlc"}
	got, err := buildSystemPrompt(root, md, "design", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Since your last turn") {
		t.Errorf("banner should not fire for a doc with no prereqs:\n%s", got)
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

	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	} else if len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("expected clean tree after eager commit, got:\n%s", out)
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

	out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	porcelain := strings.TrimSpace(string(out))
	// Stray file should still be untracked; nothing else should be dirty.
	if porcelain != "?? stray.txt" {
		t.Errorf("unexpected porcelain after eager commit:\n%s", out)
	}

	// And HEAD should only mention run.json, not stray.txt.
	diff, err := exec.Command("git", "-C", root, "show", "--name-only", "--pretty=", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v\n%s", err, diff)
	}
	names := strings.TrimSpace(string(diff))
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

// gitLogFormat runs `git log -n <n> --format=<fmt> <rev>` and returns
// the trimmed stdout — small helper so each assertion doesn't
// reimplement the exec.Command plumbing.
func gitLogFormat(t *testing.T, root string, n int, rev, format string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "log", fmt.Sprintf("-n%d", n), "--format="+format, rev).CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

// registerThrowawayWorkflow adds a one-off workflow to the package
// registry for the duration of the test run. Tests use it to probe the
// missing-fragment fallback without touching real workflows. The name
// is derived from t.Name() so parallel runs don't collide on the
// registry's duplicate-guard panic. The registry has no deregister
// hook; entries just accumulate across tests in the same process,
// which is fine — they're only read by LookupWorkflow/WorkflowNames.
func registerThrowawayWorkflow(t *testing.T, suffix string) *Workflow {
	t.Helper()
	name := "test-" + suffix + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	wf := NewWorkflow(name, "test workflow")
	noop := func(args []string, stdout, stderr io.Writer) int { return 0 }
	wf.Register(&Command{Name: "ghost", Summary: "no fragment on disk", Run: noop})
	RegisterWorkflow(wf)
	return wf
}
