package run

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// fixedNow returns an Options.Now that always yields t — so tests that
// assert on a dated slug suffix aren't sensitive to wall-clock drift.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newTestRoot initializes a throwaway git repo with scoped config so
// run.New can commit without touching ~/.gitconfig. Mirrors
// cli/stage_test.go#newTestBureaucracy.
func newTestRoot(t *testing.T) string {
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

func TestNewRequiresWorkflow(t *testing.T) {
	root := newTestRoot(t)
	// Register the project so New's "project registered" check passes.
	if err := os.MkdirAll(filepath.Join(root, "projects", "tele"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "projects", "tele", "project.json"),
		[]byte(`{"id":"tele"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	_, err := New(root, "tele", "fix it", Options{Workflow: ""})
	if err == nil {
		t.Fatal("expected error for empty workflow, got nil")
	}
	if !strings.Contains(err.Error(), "workflow is required") {
		t.Fatalf("error should name the required field, got: %v", err)
	}
}

// seedProject registers projectID and commits the project.json so
// run.New's "working tree clean" precondition passes.
func seedProject(t *testing.T, root, projectID string) {
	t.Helper()
	rel := filepath.Join("projects", projectID, "project.json")
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, rel),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "--", rel},
		{"commit", "-m", "register project " + projectID},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// TestNewDerivedSlugAutoSuffixesPastHistory covers the delete-then-reopen
// flow: a run gets created, its dir gets nuked, but the `Open run` commit
// still sits on main with the original slug's MoE-Project / MoE-Run
// trailers. A second run.New with the same title must auto-suffix past
// that history, not hand the new run the ghost of the old one.
func TestNewDerivedSlugAutoSuffixesPastHistory(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	first, err := New(root, "tele", "Fix it", Options{Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if first.ID != "fix-it" {
		t.Fatalf("first id = %q, want %q", first.ID, "fix-it")
	}

	// Operator deletes the run dir and commits the removal; the
	// Open run commit from earlier stays on main.
	deleteRunDir(t, root, "tele", "fix-it")

	second, err := New(root, "tele", "Fix it", Options{Workflow: "quick"})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if second.ID != "fix-it-2" {
		t.Fatalf("second id = %q, want %q (history check should push past deleted slug)", second.ID, "fix-it-2")
	}
}

// TestNewExplicitSlugRefusesHistoryWithSuggestion: --id is never
// auto-suffixed, but if the caller picks a slug that's already in
// history we should refuse loudly *and* hand back a free alternative
// so the operator doesn't have to play the suffix game by hand.
func TestNewExplicitSlugRefusesHistoryWithSuggestion(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	if _, err := New(root, "tele", "Fix it", Options{Workflow: "sdlc", ID: "fix-it"}); err != nil {
		t.Fatalf("first New: %v", err)
	}
	deleteRunDir(t, root, "tele", "fix-it")

	_, err := New(root, "tele", "Fix it", Options{Workflow: "quick", ID: "fix-it"})
	if err == nil {
		t.Fatal("expected error reusing a historical slug explicitly, got nil")
	}
	msg := err.Error()
	for _, want := range []string{`"fix-it"`, "tele", "fix-it-2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestNewSlugNotInOtherProject is the guard against over-eager
// uniqueness: a slug used in project A must still be usable in project
// B. The history check is per-project.
func TestNewSlugNotInOtherProject(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "a")
	seedProject(t, root, "b")

	if _, err := New(root, "a", "Fix it", Options{Workflow: "sdlc"}); err != nil {
		t.Fatalf("project a New: %v", err)
	}
	md, err := New(root, "b", "Fix it", Options{Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("project b New: %v", err)
	}
	if md.ID != "fix-it" {
		t.Fatalf("project b id = %q, want %q (cross-project slug reuse is legal)", md.ID, "fix-it")
	}
}

// deleteRunDir removes a run dir and commits the removal, so the
// working tree is clean again while the original `Open run` commit
// still sits in history — the state a manual `rm -rf` + commit leaves
// behind.
func deleteRunDir(t *testing.T, root, projectID, id string) {
	t.Helper()
	for _, args := range [][]string{
		{"rm", "-rf", "--", Dir(projectID, id)},
		{"commit", "-m", "delete run " + projectID + "/" + id},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// TestNewIDBaseFreeSlugNoSuffix covers the no-collision path through
// the IDBase branch: when runs/<base> is free, the slug is the base
// verbatim, no date. Unreachable via the CLI today (idea run dirs
// persist post-promote) but the code path exists and stays covered.
func TestNewIDBaseFreeSlugNoSuffix(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	md, err := New(root, "tele", "Whatever", Options{
		Workflow: "sdlc",
		IDBase:   "my-idea-slug",
		Now:      fixedNow(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if md.ID != "my-idea-slug" {
		t.Fatalf("id = %q, want %q (free base should not date-suffix)", md.ID, "my-idea-slug")
	}
}

// TestNewIDBaseCollisionGetsDateSuffix is the main IDBase behavior:
// when the base is already taken, the slug becomes base-YYYY-MM-DD.
// Unlike Slugify-derived collisions (which get -N), IDBase collisions
// get an honest date that tells a reader *when* the second run opened.
func TestNewIDBaseCollisionGetsDateSuffix(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	// Seed the idea-shaped occupant at my-idea-slug.
	if _, err := New(root, "tele", "First", Options{
		Workflow: "idea",
		ID:       "my-idea-slug",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Promote-style call with IDBase set; collision should resolve to
	// the dated form.
	md, err := New(root, "tele", "Second", Options{
		Workflow: "sdlc",
		IDBase:   "my-idea-slug",
		Now:      fixedNow(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if md.ID != "my-idea-slug-2026-04-22" {
		t.Fatalf("id = %q, want %q", md.ID, "my-idea-slug-2026-04-22")
	}
}

// TestNewIDBaseSameDayDoubleCollisionFallsBackToCounter locks in the
// rare-but-real case where two promotes of the same idea slug happen
// on the same calendar day. The first resolves to base-YYYY-MM-DD; the
// second falls back to base-YYYY-MM-DD-2.
func TestNewIDBaseSameDayDoubleCollisionFallsBackToCounter(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	fixedDay := fixedNow(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC))

	// Occupy both my-idea-slug and my-idea-slug-2026-04-22.
	if _, err := New(root, "tele", "First", Options{
		Workflow: "idea",
		ID:       "my-idea-slug",
	}); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	if _, err := New(root, "tele", "Second", Options{
		Workflow: "sdlc",
		IDBase:   "my-idea-slug",
		Now:      fixedDay,
	}); err != nil {
		t.Fatalf("seed dated: %v", err)
	}

	md, err := New(root, "tele", "Third", Options{
		Workflow: "sdlc",
		IDBase:   "my-idea-slug",
		Now:      fixedDay,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if md.ID != "my-idea-slug-2026-04-22-2" {
		t.Fatalf("id = %q, want %q", md.ID, "my-idea-slug-2026-04-22-2")
	}
}

// TestNewRecordsOpenedFrom: New must capture HEAD before its open
// commit lands and persist that SHA to run.json. moe follow uses the
// field as hunk's diff base for non-code stages, so a missing or
// stale value would silently break the seed-visible-in-pane case.
func TestNewRecordsOpenedFrom(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	parent, err := git.RevParse(root, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse pre-open: %v", err)
	}

	md, err := New(root, "tele", "Fix it", Options{Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if md.OpenedFrom != parent {
		t.Errorf("returned OpenedFrom = %q, want HEAD pre-open %q", md.OpenedFrom, parent)
	}

	// Persisted on disk so resolveFollowTarget can read it after
	// the operator restarts moe.
	loaded, err := Load(root, "tele", md.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.OpenedFrom != parent {
		t.Errorf("persisted OpenedFrom = %q, want %q", loaded.OpenedFrom, parent)
	}

	// And it really is the parent of the open commit, not HEAD now.
	headNow, err := git.RevParse(root, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse post-open: %v", err)
	}
	if headNow == parent {
		t.Fatal("open commit did not advance HEAD; test setup is wrong")
	}
}

func TestLoadRequiresWorkflow(t *testing.T) {
	root := newTestRoot(t)
	runDir := filepath.Join(root, Dir("tele", "fix-it"))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Intentionally omit the "workflow" key.
	md := map[string]any{
		"id":        "fix-it",
		"project":   "tele",
		"title":     "Fix it",
		"status":    StatusInProgress,
		"created":   "2026-04-01",
		"documents": map[string]any{},
	}
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Load(root, "tele", "fix-it")
	if err == nil {
		t.Fatal("expected error loading run.json without workflow key, got nil")
	}
	if !strings.Contains(err.Error(), "workflow is required") {
		t.Fatalf("error should name the required field, got: %v", err)
	}
}

// TestJournalIndexLastActivityMatchesLastActivity is the load-bearing
// equivalence check: the batched index and the per-slug git log must
// agree for every run on disk. moe dash relies on that — replacing
// N×LastActivity with one map lookup is only safe if both return the
// same answer.
func TestJournalIndexLastActivityMatchesLastActivity(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string, when time.Time) {
		t.Helper()
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", subject+"\n\n"+body)
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
	// Two slugs, multiple commits each, including a backdated commit on
	// HEAD — that's the case `git log -1 --grep` resolves topologically
	// rather than by committer date, and the index has to agree.
	commitWith("Open run x/alpha", "MoE-Run: alpha\n", time.Time{})
	commitWith("Open run x/beta", "MoE-Run: beta\n", time.Time{})
	commitWith("work on alpha", "MoE-Run: alpha\n", time.Now().Add(-2*time.Hour))
	commitWith("work on beta backdated", "MoE-Run: beta\n",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	for _, slug := range []string{"alpha", "beta"} {
		want, err := LastActivity(root, slug)
		if err != nil {
			t.Fatalf("LastActivity %q: %v", slug, err)
		}
		if !idx.LastActivity[slug].Equal(want) {
			t.Errorf("slug %q: index=%v LastActivity=%v", slug, idx.LastActivity[slug], want)
		}
	}
	// Slugs not present in any commit are absent (zero time on lookup).
	if v, ok := idx.LastActivity["never"]; ok {
		t.Errorf("expected unknown slug to be absent, got %v", v)
	}
}

// TestJournalIndexCapturesPromotedToAndPRURL pins the multi-trailer
// extraction: PromotedTo/PRURL on a run-scoped commit must surface in
// the index without a second git log walk. Replaces N trailerValue
// forks dash used to do per row.
func TestJournalIndexCapturesPromotedToAndPRURL(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", subject+"\n\n"+body)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v\n%s", err, out)
		}
	}
	// idea promoted to a run; the promotion commit carries both trailers.
	commitWith("Promote idea p/idea-x → p/run-y",
		"MoE-Run: idea-x\nMoE-Project: p\nMoE-Workflow: idea\nMoE-Promoted-To: p/run-y\n")
	// run pushed; the push commit carries MoE-PR alongside MoE-Run.
	commitWith("push: shipped",
		"MoE-Run: run-y\nMoE-PR: https://example.com/pr/42\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.PromotedTo["idea-x"]; got != "p/run-y" {
		t.Errorf("PromotedTo[idea-x] = %q, want %q", got, "p/run-y")
	}
	if got := idx.PRURL["run-y"]; got != "https://example.com/pr/42" {
		t.Errorf("PRURL[run-y] = %q, want %q", got, "https://example.com/pr/42")
	}
	// Unrelated slugs read as the zero value, so callers don't need
	// presence checks.
	if got := idx.PromotedTo["never"]; got != "" {
		t.Errorf("PromotedTo[never] = %q, want \"\"", got)
	}
}

// TestJournalIndexPicksMostRecentTrailerValue: when a slug shows up
// on multiple commits each carrying a different MoE-PR (the closed →
// reopened case), the most recent value wins — same answer the
// per-row trailerValue used to give.
func TestJournalIndexPicksMostRecentTrailerValue(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", subject+"\n\n"+body)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v\n%s", err, out)
		}
	}
	commitWith("push: first attempt", "MoE-Run: r\nMoE-PR: https://example.com/pr/1\n")
	commitWith("push: re-pushed after close", "MoE-Run: r\nMoE-PR: https://example.com/pr/2\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.PRURL["r"]; got != "https://example.com/pr/2" {
		t.Errorf("PRURL[r] = %q, want %q (most recent wins)", got, "https://example.com/pr/2")
	}
}
