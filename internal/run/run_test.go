package run

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// fixedNow returns an Options.Now that always yields t — so tests that
// assert on a dated slug suffix aren't sensitive to wall-clock drift.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newTestRoot initializes a throwaway git repo with scoped config so
// run.New can commit without touching ~/.gitconfig. The "-b main"
// rename matches the production root layout — tests that assert on
// branch names depend on it.
func newTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
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

	_, err := New(root, "tele", Options{ID: "fix-it", Workflow: ""})
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
	gittest.Run(t, root, "add", "--", rel)
	gittest.Run(t, root, "commit", "-m", "register project "+projectID)
}

// TestNewExplicitSlugRefusesHistoryWithSuggestion: an operator-typed
// slug is never auto-suffixed, but if the caller picks a slug that's
// already in history we should refuse loudly *and* hand back a free
// alternative so the operator doesn't have to play the suffix game by
// hand.
func TestNewExplicitSlugRefusesHistoryWithSuggestion(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	if _, err := New(root, "tele", Options{Workflow: "sdlc", ID: "fix-it"}); err != nil {
		t.Fatalf("first New: %v", err)
	}
	deleteRunDir(t, root, "tele", "fix-it")

	_, err := New(root, "tele", Options{Workflow: "kb", ID: "fix-it"})
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

	if _, err := New(root, "a", Options{ID: "fix-it", Workflow: "sdlc"}); err != nil {
		t.Fatalf("project a New: %v", err)
	}
	md, err := New(root, "b", Options{ID: "fix-it", Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("project b New: %v", err)
	}
	if md.ID != "fix-it" {
		t.Fatalf("project b id = %q, want %q (cross-project slug reuse is legal)", md.ID, "fix-it")
	}
}

// TestNewPersistsReopenOf pins that Options.ReopenOf round-trips
// through run.json. The stage prompt assembler reads Metadata.ReopenOf
// to surface prior-run lineage without walking git per turn — if the
// field is lost on write, the section silently disappears for every
// reopen and the agent loses the cue.
func TestNewPersistsReopenOf(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	if _, err := New(root, "tele", Options{
		Workflow: "sdlc",
		ID:       "prior-slug",
	}); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	md, err := New(root, "tele", Options{
		Workflow: "sdlc",
		ID:       "new-slug",
		ReopenOf: "prior-slug",
	})
	if err != nil {
		t.Fatalf("New reopen: %v", err)
	}
	if md.ReopenOf != "prior-slug" {
		t.Fatalf("returned metadata ReopenOf = %q, want %q", md.ReopenOf, "prior-slug")
	}

	// And the value is durable on disk — the stage prompt re-loads
	// run.json on every turn, so the field must round-trip the JSON
	// boundary, not just live on the in-memory struct.
	loaded, err := Load(root, "tele", "new-slug")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ReopenOf != "prior-slug" {
		t.Errorf("loaded ReopenOf = %q, want %q", loaded.ReopenOf, "prior-slug")
	}

	// Read the raw run.json so the json tag is pinned too — a typo
	// in the struct tag would still let the value round-trip through
	// the same encoder/decoder pair, but break any external reader.
	raw, err := os.ReadFile(filepath.Join(root, Dir("tele", "new-slug"), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reopen_of": "prior-slug"`) {
		t.Errorf("run.json missing reopen_of field:\n%s", raw)
	}

	// And the field is omitempty: a non-reopen run's run.json must
	// not carry an empty "reopen_of" key.
	fresh, err := New(root, "tele", Options{Workflow: "sdlc", ID: "no-reopen"})
	if err != nil {
		t.Fatalf("fresh New: %v", err)
	}
	if fresh.ReopenOf != "" {
		t.Errorf("non-reopen ReopenOf = %q, want empty", fresh.ReopenOf)
	}
	rawFresh, err := os.ReadFile(filepath.Join(root, Dir("tele", "no-reopen"), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawFresh), "reopen_of") {
		t.Errorf("non-reopen run.json should omit reopen_of:\n%s", rawFresh)
	}
}

func TestNewPersistsPromoteTo(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	md, err := New(root, "tele", Options{
		Workflow:  "idea",
		ID:        "cleanup-foo",
		PromoteTo: "sdlc",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if md.PromoteTo != "sdlc" {
		t.Fatalf("returned PromoteTo = %q, want sdlc", md.PromoteTo)
	}
	loaded, err := Load(root, "tele", "cleanup-foo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.PromoteTo != "sdlc" {
		t.Fatalf("loaded PromoteTo = %q, want sdlc", loaded.PromoteTo)
	}
	raw, err := os.ReadFile(filepath.Join(root, Dir("tele", "cleanup-foo"), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"promote_to": "sdlc"`) {
		t.Fatalf("run.json missing promote_to:\n%s", raw)
	}

	untagged, err := New(root, "tele", Options{Workflow: "idea", ID: "needs-triage"})
	if err != nil {
		t.Fatalf("New untagged: %v", err)
	}
	raw, err = os.ReadFile(filepath.Join(root, Dir("tele", untagged.ID), "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "promote_to") {
		t.Fatalf("untagged run.json should omit promote_to:\n%s", raw)
	}
}

// deleteRunDir removes a run dir and commits the removal, so the
// working tree is clean again while the original `Open run` commit
// still sits in history — the state a manual `rm -rf` + commit leaves
// behind.
func deleteRunDir(t *testing.T, root, projectID, id string) {
	t.Helper()
	gittest.Run(t, root, "rm", "-rf", "--", Dir(projectID, id))
	gittest.Run(t, root, "commit", "-m", "delete run "+projectID+"/"+id)
}

// TestNewIDBaseFreeSlugNoSuffix covers the no-collision path through
// the IDBase branch: when runs/<base> is free, the slug is the base
// verbatim, no date. Unreachable via the CLI today (idea run dirs
// persist post-promote) but the code path exists and stays covered.
func TestNewIDBaseFreeSlugNoSuffix(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	md, err := New(root, "tele", Options{
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
	if _, err := New(root, "tele", Options{
		Workflow: "idea",
		ID:       "my-idea-slug",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Promote-style call with IDBase set; collision should resolve to
	// the dated form.
	md, err := New(root, "tele", Options{
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
	if _, err := New(root, "tele", Options{
		Workflow: "idea",
		ID:       "my-idea-slug",
	}); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	if _, err := New(root, "tele", Options{
		Workflow: "sdlc",
		IDBase:   "my-idea-slug",
		Now:      fixedDay,
	}); err != nil {
		t.Fatalf("seed dated: %v", err)
	}

	md, err := New(root, "tele", Options{
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

// TestLoadMissingRunIsErrRunNotFound: a typo in the project or run id
// must surface as ErrRunNotFound so callers can render a clean message
// instead of leaking the per-turn worktree path through the raw os
// error. The default %v formatting must include both ids and not the
// path, since that path is what the operator actually sees on stderr.
func TestLoadMissingRunIsErrRunNotFound(t *testing.T) {
	root := newTestRoot(t)
	_, err := Load(root, "ghost", "missing")
	if err == nil {
		t.Fatal("expected error loading nonexistent run, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("error should match ErrRunNotFound, got: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ghost/missing") {
		t.Fatalf("error should name the project/run pair, got: %v", err)
	}
	if strings.Contains(msg, root) {
		t.Fatalf("error should not leak the on-disk path, got: %v", err)
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
		args := []string{"commit", "--allow-empty", "-m", subject + "\n\n" + body}
		if !when.IsZero() {
			stamp := when.Format(time.RFC3339)
			gittest.RunWithEnv(t, root, []string{
				"GIT_AUTHOR_DATE=" + stamp,
				"GIT_COMMITTER_DATE=" + stamp,
			}, args...)
			return
		}
		gittest.Run(t, root, args...)
	}
	// Two slugs, multiple commits each, including a backdated commit on
	// HEAD — that's the case `git log -1 --grep` resolves topologically
	// rather than by committer date, and the index has to agree. Both
	// carry MoE-Project so the index and the oracle key runs the same
	// qualified way.
	commitWith("Open run x/alpha", "MoE-Project: x\nMoE-Run: alpha\n", time.Time{})
	commitWith("Open run x/beta", "MoE-Project: x\nMoE-Run: beta\n", time.Time{})
	commitWith("work on alpha", "MoE-Project: x\nMoE-Run: alpha\n", time.Now().Add(-2*time.Hour))
	commitWith("work on beta backdated", "MoE-Project: x\nMoE-Run: beta\n",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	for _, slug := range []string{"alpha", "beta"} {
		want, err := LastActivity(root, "x", slug)
		if err != nil {
			t.Fatalf("LastActivity %q: %v", slug, err)
		}
		key := "x/" + slug
		if !idx.LastActivity[key].Equal(want) {
			t.Errorf("slug %q: index=%v LastActivity=%v", slug, idx.LastActivity[key], want)
		}
	}
	// Runs not present in any commit are absent (zero time on lookup).
	if v, ok := idx.LastActivity["x/never"]; ok {
		t.Errorf("expected unknown run to be absent, got %v", v)
	}
}

// TestJournalIndexDailyRunCount pins the histogram metric: a day's
// count is the number of *distinct* run slugs that committed that UTC
// day, so a run committing twice in a day counts once and two runs the
// same day count two.
func TestJournalIndexDailyRunCount(t *testing.T) {
	root := newTestRoot(t)
	commitOn := func(slug string, when time.Time) {
		t.Helper()
		stamp := when.Format(time.RFC3339)
		gittest.RunWithEnv(t, root, []string{
			"GIT_AUTHOR_DATE=" + stamp,
			"GIT_COMMITTER_DATE=" + stamp,
		}, "commit", "--allow-empty", "-m", "work\n\nMoE-Run: "+slug+"\n")
	}

	day1 := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	// day1: alpha commits twice (counts once), beta once → 2 distinct.
	commitOn("alpha", day1)
	commitOn("alpha", day1.Add(3*time.Hour))
	commitOn("beta", day1)
	// day2: only alpha → 1 distinct.
	commitOn("alpha", day2)

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.DailyRunCount["2026-06-18"]; got != 2 {
		t.Errorf("day1 count = %d, want 2 (alpha+beta, alpha deduped)", got)
	}
	if got := idx.DailyRunCount["2026-06-19"]; got != 1 {
		t.Errorf("day2 count = %d, want 1", got)
	}
	// A day with no activity is absent (zero on lookup).
	if got, ok := idx.DailyRunCount["2026-06-20"]; ok {
		t.Errorf("quiet day present with %d, want absent", got)
	}
}

// TestJournalIndexDailyRunCountByProject pins the per-(project,day)
// slice and the cross-project collision fix: two different runs that
// happen to share a slug, active the same day in different projects,
// count as two distinct runs globally and one in each project — keying
// on bare slug would have collapsed them to one.
func TestJournalIndexDailyRunCountByProject(t *testing.T) {
	root := newTestRoot(t)
	commitOn := func(project, slug string, when time.Time) {
		t.Helper()
		stamp := when.Format(time.RFC3339)
		gittest.RunWithEnv(t, root, []string{
			"GIT_AUTHOR_DATE=" + stamp,
			"GIT_COMMITTER_DATE=" + stamp,
		}, "commit", "--allow-empty", "-m",
			"work\n\nMoE-Project: "+project+"\nMoE-Run: "+slug+"\n")
	}

	day := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	// Same slug "shared" in two projects on the same day, plus an extra
	// run in alpha — global is the count of distinct (project, slug) pairs.
	commitOn("alpha", "shared", day)
	commitOn("beta", "shared", day)
	commitOn("alpha", "solo", day)

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	// Global: alpha/shared, beta/shared, alpha/solo → 3 distinct pairs.
	// A bare-slug key would have collapsed the two "shared" runs to 2.
	if got := idx.DailyRunCount["2026-06-18"]; got != 3 {
		t.Errorf("global count = %d, want 3 (alpha/shared, beta/shared, alpha/solo)", got)
	}
	if got := idx.DailyRunCountByProject["alpha"]["2026-06-18"]; got != 2 {
		t.Errorf("alpha count = %d, want 2 (shared+solo)", got)
	}
	if got := idx.DailyRunCountByProject["beta"]["2026-06-18"]; got != 1 {
		t.Errorf("beta count = %d, want 1 (shared)", got)
	}
	// An unknown project's slice is nil — reads as all-zero, which the
	// gather collapses to the (quiet) state.
	if got := idx.DailyRunCountByProject["ghost"]["2026-06-18"]; got != 0 {
		t.Errorf("unknown project count = %d, want 0", got)
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
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	// idea promoted to a run; the promotion commit carries both trailers.
	commitWith("Promote idea p/idea-x → p/run-y",
		"MoE-Run: idea-x\nMoE-Project: p\nMoE-Workflow: idea\nMoE-Promoted-To: p/run-y\n")
	// run pushed; the push commit carries MoE-PR alongside MoE-Run.
	commitWith("push: shipped",
		"MoE-Run: run-y\nMoE-Project: p\nMoE-PR: https://example.com/pr/42\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.PromotedTo["p/idea-x"]; got != "p/run-y" {
		t.Errorf("PromotedTo[p/idea-x] = %q, want %q", got, "p/run-y")
	}
	if got := idx.PRURL["p/run-y"]; got != "https://example.com/pr/42" {
		t.Errorf("PRURL[p/run-y] = %q, want %q", got, "https://example.com/pr/42")
	}
	// Unrelated runs read as the zero value, so callers don't need
	// presence checks.
	if got := idx.PromotedTo["p/never"]; got != "" {
		t.Errorf("PromotedTo[p/never] = %q, want \"\"", got)
	}
}

// TestJournalIndexWorkTurnTimeMatchesLatestWorkTurnSHA pins the
// load-bearing equivalence for the indexed stage-satisfaction path:
// every (project, run, doc) the index claims a time for must match
// what LatestWorkTurnSHA would return for the same key. Workflow.Next
// WithIndex relies on that — collapsing M×N forks into a map lookup
// is only safe when both produce the same answer.
func TestJournalIndexWorkTurnTimeMatchesLatestWorkTurnSHA(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string, when time.Time) {
		t.Helper()
		args := []string{"commit", "--allow-empty", "-m", subject + "\n\n" + body}
		if !when.IsZero() {
			stamp := when.Format(time.RFC3339)
			gittest.RunWithEnv(t, root, []string{
				"GIT_AUTHOR_DATE=" + stamp,
				"GIT_COMMITTER_DATE=" + stamp,
			}, args...)
			return
		}
		gittest.Run(t, root, args...)
	}
	workTurn := func(projectID, runID, docID string, when time.Time) {
		commitWith(
			"work: update "+docID,
			"MoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Workflow: sdlc\nMoE-Document: "+docID+"\n",
			when,
		)
	}
	// Two projects, same slug — the project leg of WorkTurnKey is the
	// thing keeping these from cross-satisfying each other (the same
	// invariant TestWorkflowNextIgnoresOtherProjectSameSlug pins for
	// the forking path).
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	workTurn("a", "fix-bug", "design", t0)
	workTurn("a", "fix-bug", "code", t0.Add(time.Hour))
	workTurn("b", "fix-bug", "design", t0.Add(2*time.Hour))
	// Session-start commit must not register a work turn — the subject
	// pin keeps it out, same as LatestWorkTurnSHA's anchored grep.
	commitWith(
		"work: start session for code",
		"MoE-Run: fix-bug\nMoE-Project: c\nMoE-Workflow: sdlc\nMoE-Document: code\n",
		t0.Add(3*time.Hour),
	)

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	cases := []struct {
		project, run, doc string
	}{
		{"a", "fix-bug", "design"},
		{"a", "fix-bug", "code"},
		{"b", "fix-bug", "design"},
		{"b", "fix-bug", "code"}, // never committed; both paths return zero
		{"c", "fix-bug", "code"}, // only a session-start; both paths return zero
		{"never", "nope", "design"},
	}
	for _, tc := range cases {
		_, want, err := LatestWorkTurnSHA(root, tc.project, tc.run, tc.doc)
		if err != nil {
			t.Fatalf("LatestWorkTurnSHA(%v): %v", tc, err)
		}
		got := idx.WorkTurnTime[WorkTurnKey{Project: tc.project, Run: tc.run, Doc: tc.doc}]
		if !got.Equal(want) {
			t.Errorf("(%s/%s/%s): index=%v LatestWorkTurnSHA=%v", tc.project, tc.run, tc.doc, got, want)
		}
	}
}

// TestJournalIndexAdvanceTimeMatchesLatestAdvanceSHA is the advance-marker
// twin of TestJournalIndexWorkTurnTimeMatchesLatestWorkTurnSHA: every
// (project, run, doc) the index claims an AdvanceTime for must match what
// LatestAdvanceSHA returns for the same key. stageSatisfied's advance
// check reads AdvanceTime on the dash index path and LatestAdvanceSHA on
// the per-call fork — they must agree or dash picks a different next stage
// than the chain prompt. Also pins that a `work: update` commit does not
// register an advance time (and vice versa): the two subjects key disjoint
// maps.
func TestJournalIndexAdvanceTimeMatchesLatestAdvanceSHA(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string, when time.Time) {
		t.Helper()
		args := []string{"commit", "--allow-empty", "-m", subject + "\n\n" + body}
		if !when.IsZero() {
			stamp := when.Format(time.RFC3339)
			gittest.RunWithEnv(t, root, []string{
				"GIT_AUTHOR_DATE=" + stamp,
				"GIT_COMMITTER_DATE=" + stamp,
			}, args...)
			return
		}
		gittest.Run(t, root, args...)
	}
	commit := func(subject, projectID, runID, docID string, when time.Time) {
		commitWith(
			subject,
			"MoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Workflow: sdlc\nMoE-Document: "+docID+"\n",
			when,
		)
	}
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	// design advanced; code worked but never advanced.
	commit("work: update design", "a", "fix-bug", "design", t0)
	commit("advance: design", "a", "fix-bug", "design", t0.Add(time.Minute))
	commit("work: update code", "a", "fix-bug", "code", t0.Add(time.Hour))
	// Same slug in another project must not cross-satisfy.
	commit("advance: design", "b", "fix-bug", "design", t0.Add(2*time.Hour))

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	cases := []struct {
		project, run, doc string
	}{
		{"a", "fix-bug", "design"}, // has an advance marker
		{"a", "fix-bug", "code"},   // work-turn only, no advance → zero
		{"b", "fix-bug", "design"}, // advance in the other project
		{"never", "nope", "design"},
	}
	for _, tc := range cases {
		_, want, err := LatestAdvanceSHA(root, tc.project, tc.run, tc.doc)
		if err != nil {
			t.Fatalf("LatestAdvanceSHA(%v): %v", tc, err)
		}
		got := idx.AdvanceTime[WorkTurnKey{Project: tc.project, Run: tc.run, Doc: tc.doc}]
		if !got.Equal(want) {
			t.Errorf("(%s/%s/%s): index=%v LatestAdvanceSHA=%v", tc.project, tc.run, tc.doc, got, want)
		}
	}
	// A work-turn commit must not leak into AdvanceTime, and the advance
	// marker must not leak into WorkTurnTime — disjoint subjects, disjoint
	// maps.
	codeKey := WorkTurnKey{Project: "a", Run: "fix-bug", Doc: "code"}
	if v, ok := idx.AdvanceTime[codeKey]; ok {
		t.Errorf("code has no advance marker but AdvanceTime[code]=%v", v)
	}
	designKey := WorkTurnKey{Project: "a", Run: "fix-bug", Doc: "design"}
	if idx.WorkTurnTime[designKey].IsZero() {
		t.Errorf("design has a work-turn but WorkTurnTime[design] is zero")
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
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("push: first attempt", "MoE-Run: r\nMoE-Project: p\nMoE-PR: https://example.com/pr/1\n")
	commitWith("push: re-pushed after close", "MoE-Run: r\nMoE-Project: p\nMoE-PR: https://example.com/pr/2\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.PRURL["p/r"]; got != "https://example.com/pr/2" {
		t.Errorf("PRURL[p/r] = %q, want %q (most recent wins)", got, "https://example.com/pr/2")
	}
}

// TestJournalIndexCapturesReopenedFrom: a reopen commit's
// MoE-Reopen-Of trailer lands in the index keyed by the new run's
// slug (the commit's MoE-Run value) → the prior slug. Mirrors the
// PromotedTo shape but read in the opposite direction; dash inverts
// the map to find unreopened candidates.
func TestJournalIndexCapturesReopenedFrom(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	// A reopen lands as an open commit on the new slug carrying
	// MoE-Reopen-Of pointing back at the prior slug.
	commitWith("Open run p/run-new from reopen of run-old",
		"MoE-Run: run-new\nMoE-Project: p\nMoE-Reopen-Of: run-old\n")
	// An unrelated run with no Reopen-Of must not pollute the map.
	commitWith("Open run p/loose",
		"MoE-Run: loose\nMoE-Project: p\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	// Key is qualified "<project>/<new-slug>"; the value stays a bare slug.
	if got := idx.ReopenedFrom["p/run-new"]; got != "run-old" {
		t.Errorf("ReopenedFrom[p/run-new] = %q, want %q", got, "run-old")
	}
	if _, ok := idx.ReopenedFrom["p/loose"]; ok {
		t.Errorf("ReopenedFrom[p/loose] should be absent for a run without the trailer")
	}
}

// TestJournalIndexCapturesSpawnedBy: a spawned run's open commit
// carries MoE-Spawned-By pointing at its spawner; the index keys it by
// the spawned run's slug → the qualified spawner, the same shape and
// direction as ReopenedFrom. A qualified trailer is kept verbatim; a
// legacy bare trailer normalizes to the spawned run's own project.
func TestJournalIndexCapturesSpawnedBy(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	// A tailed pulse opens carrying a qualified MoE-Spawned-By naming the
	// run that triggered it (the format new writers emit).
	commitWith("Open run p/pulse-2026-07-17",
		"MoE-Run: pulse-2026-07-17\nMoE-Project: p\nMoE-Spawned-By: p/ship-it\n")
	// A legacy commit carries a bare spawner slug; the builder qualifies it
	// with the spawned run's own project (all historical spawns were
	// same-project, so this is exact, not a guess).
	commitWith("Open run p/pulse-legacy",
		"MoE-Run: pulse-legacy\nMoE-Project: p\nMoE-Spawned-By: ship-it\n")
	// A run opened by hand carries no edge and must not pollute the map.
	commitWith("Open run p/loose",
		"MoE-Run: loose\nMoE-Project: p\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.SpawnedBy["p/pulse-2026-07-17"]; got != "p/ship-it" {
		t.Errorf("SpawnedBy[p/pulse-2026-07-17] = %q, want %q", got, "p/ship-it")
	}
	if got := idx.SpawnedBy["p/pulse-legacy"]; got != "p/ship-it" {
		t.Errorf("SpawnedBy[p/pulse-legacy] = %q, want legacy bare value normalized to %q", got, "p/ship-it")
	}
	if _, ok := idx.SpawnedBy["p/loose"]; ok {
		t.Errorf("SpawnedBy[p/loose] should be absent for a run without the trailer")
	}
}

// TestSlugTakenIgnoresPrefixAndCrossProjectSiblings: the anchored,
// project-scoped grep means `auth` is free when only `auth-2` (same
// project) or `auth` in another project carries history — no run dir,
// no exact same-project trailer. The old unanchored grep reported `auth`
// taken off `auth-2`'s history, walking NextFreeID further than needed
// and refusing valid slugs.
func TestSlugTakenIgnoresPrefixAndCrossProjectSiblings(t *testing.T) {
	root := newTestRoot(t)
	commit := func(body string) {
		gittest.Run(t, root, "commit", "--allow-empty", "-m", "open\n\n"+body)
	}
	// Prefix-extending sibling in the same project, and the same slug in
	// another project — neither should make alpha/auth "taken".
	commit("MoE-Project: alpha\nMoE-Run: auth-2\n")
	commit("MoE-Project: beta\nMoE-Run: auth\n")

	taken, err := SlugTaken(root, "alpha", "auth")
	if err != nil {
		t.Fatalf("SlugTaken: %v", err)
	}
	if taken {
		t.Error("SlugTaken(alpha, auth) = true; want false (only auth-2 and beta/auth have history)")
	}
	// The exact same-project run is still reported taken.
	if taken, err := SlugTaken(root, "alpha", "auth-2"); err != nil || !taken {
		t.Errorf("SlugTaken(alpha, auth-2) = (%v, %v); want (true, nil)", taken, err)
	}
}

// TestLatestWorkTurnSHAIgnoresPrefixSibling: a work turn on `auth-2`
// must not satisfy `auth`'s stage lookup — the anchored MoE-Run grep
// keeps the prefix sibling out, so a run with no work turn reads zero.
func TestLatestWorkTurnSHAIgnoresPrefixSibling(t *testing.T) {
	root := newTestRoot(t)
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"work: update design\n\nMoE-Project: alpha\nMoE-Run: auth-2\nMoE-Workflow: sdlc\nMoE-Document: design\n")

	sha, when, err := LatestWorkTurnSHA(root, "alpha", "auth", "design")
	if err != nil {
		t.Fatalf("LatestWorkTurnSHA: %v", err)
	}
	if sha != "" || !when.IsZero() {
		t.Errorf("LatestWorkTurnSHA(alpha, auth, design) = (%q, %v); want empty (auth-2's turn must not leak)", sha, when)
	}
}

// TestJournalIndexCrossProjectSameSlugDistinct: two runs sharing a slug
// in different projects get their own qualified index entries — a
// bare-slug key would have collapsed them, letting one project's PR /
// promotion / age read for the other's run.
func TestJournalIndexCrossProjectSameSlugDistinct(t *testing.T) {
	root := newTestRoot(t)
	commit := func(subject, body string, when time.Time) {
		stamp := when.Format(time.RFC3339)
		gittest.RunWithEnv(t, root, []string{
			"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
		}, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	tA := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// alpha/shared: a promoted idea with a PR. beta/shared: a plain run,
	// active a month later.
	commit("push: alpha shared",
		"MoE-Project: alpha\nMoE-Run: shared\nMoE-PR: https://example.com/pr/alpha\nMoE-Promoted-To: alpha/impl\n", tA)
	commit("work: update code",
		"MoE-Project: beta\nMoE-Run: shared\nMoE-Workflow: sdlc\nMoE-Document: code\n", tB)

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got := idx.LastActivity["alpha/shared"]; !got.Equal(tA) {
		t.Errorf("LastActivity[alpha/shared] = %v, want %v", got, tA)
	}
	if got := idx.LastActivity["beta/shared"]; !got.Equal(tB) {
		t.Errorf("LastActivity[beta/shared] = %v, want %v", got, tB)
	}
	if got := idx.PromotedTo["alpha/shared"]; got != "alpha/impl" {
		t.Errorf("PromotedTo[alpha/shared] = %q, want alpha/impl", got)
	}
	// beta/shared carries no promotion — its entry must stay empty, not
	// borrow alpha's.
	if got := idx.PromotedTo["beta/shared"]; got != "" {
		t.Errorf("PromotedTo[beta/shared] = %q, want empty", got)
	}
	if got := idx.PRURL["beta/shared"]; got != "" {
		t.Errorf("PRURL[beta/shared] = %q, want empty (alpha's PR must not leak)", got)
	}
}

// TestJournalIndexChainedChildLiveEdge: a chain edit commit lands
// MoE-Chained-To trailers without a MoE-Run scope (one edit touches
// several parents — no single canonical run). The index must pick
// them up regardless. The widening of BuildJournalIndex's grep is
// load-bearing for this — without it the commit is invisible.
func TestJournalIndexChainedChildLiveEdge(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("chain: edit (2 added, 0 removed)",
		"MoE-Chained-To: a/parent1 a/child1\nMoE-Chained-To: a/parent2 b/child2\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got, want := idx.ChainedChild["a/parent1"], "a/child1"; got != want {
		t.Errorf("ChainedChild[a/parent1] = %q, want %q", got, want)
	}
	if got, want := idx.ChainedChild["a/parent2"], "b/child2"; got != want {
		t.Errorf("ChainedChild[a/parent2] = %q, want %q (cross-project edge)", got, want)
	}
	if _, ok := idx.ChainedChild["a/unknown"]; ok {
		t.Errorf("ChainedChild[a/unknown] should be absent for a parent with no trailer")
	}
}

// TestJournalIndexChainedChildNewerCommitWins: when a parent has
// trailers across multiple commits, the most recent one decides the
// live state. Mirrors PromotedTo's "first encountered (HEAD-side)
// wins" semantics.
func TestJournalIndexChainedChildNewerCommitWins(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("chain: edit (older)",
		"MoE-Chained-To: a/parent a/old-child\n")
	commitWith("chain: edit (newer)",
		"MoE-Chained-To-Removed: a/parent a/old-child\nMoE-Chained-To: a/parent a/new-child\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got, want := idx.ChainedChild["a/parent"], "a/new-child"; got != want {
		t.Errorf("ChainedChild[a/parent] = %q, want %q (newer commit wins, replace pair: added beats removed within commit)", got, want)
	}
}

// TestJournalIndexChainedChildClearPinsEmpty: a `chain clear` commit
// stamps Removed for a previously-live edge. The index must remember
// the clear (entry present, value empty) so an older Chained-To
// commit can't re-assert the edge during the walk.
func TestJournalIndexChainedChildClearPinsEmpty(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("chain: edit (older)",
		"MoE-Chained-To: a/parent a/child\n")
	commitWith("chain: clear",
		"MoE-Chained-To-Removed: a/parent a/child\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	got, ok := idx.ChainedChild["a/parent"]
	if !ok {
		t.Fatalf("ChainedChild[a/parent]: expected entry pinning the clear, got absent")
	}
	if got != "" {
		t.Errorf("ChainedChild[a/parent] = %q, want \"\" (cleared)", got)
	}
}

// TestJournalIndexChainedChildIgnoresMalformed: a trailer that doesn't
// parse as two whitespace-separated <project>/<slug> tokens must be
// dropped silently rather than fail the whole index build. Other
// trailer parsers here take the same posture.
func TestJournalIndexChainedChildIgnoresMalformed(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("chain: edit (malformed)",
		"MoE-Chained-To: not-qualified-token\n"+
			"MoE-Chained-To: a/ok b/ok\n"+
			"MoE-Chained-To: bare-slug-only\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got, want := idx.ChainedChild["a/ok"], "b/ok"; got != want {
		t.Errorf("well-formed edge missing: ChainedChild[a/ok] = %q, want %q", got, want)
	}
	if len(idx.ChainedChild) != 1 {
		t.Errorf("ChainedChild should contain only the well-formed edge: got %v", idx.ChainedChild)
	}
}

// TestJournalIndexChoreSkipped: a `moe chore skip` commit lands a
// MoE-Chore-Skipped trailer with no MoE-Run scope (like chain edits).
// The index must pick it up via the "^MoE-Chore" grep widening, and
// the most recent skip per chore wins (HEAD-first, first-seen).
func TestJournalIndexChoreSkipped(t *testing.T) {
	root := newTestRoot(t)
	commitWith := func(subject, body string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	commitWith("chore: skip moe/readme-refresh",
		"MoE-Chore-Skipped: moe/readme-refresh\n")

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if _, ok := idx.ChoreSkipped["moe/readme-refresh"]; !ok {
		t.Fatalf("ChoreSkipped[moe/readme-refresh] absent: %v", idx.ChoreSkipped)
	}
	// A bare MoE-Chore: line must not be misparsed as a skip.
	if _, ok := idx.ChoreSkipped["moe/other"]; ok {
		t.Errorf("ChoreSkipped should not contain an unrelated chore")
	}
}
