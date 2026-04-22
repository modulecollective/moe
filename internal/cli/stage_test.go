package cli

import (
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
