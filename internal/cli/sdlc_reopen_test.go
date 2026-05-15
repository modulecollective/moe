package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// seedClosedSDLCRun composes a closed sdlc run with a populated design
// canvas — the precondition reopen reads from. Registers the design
// doc + commits the canvas (so close's clean-tree gate passes), then
// runs `sdlc close` to flip status. The resulting fixture has the
// run.json status=closed, the design canvas on disk, and a clean
// working tree.
func seedClosedSDLCRun(t *testing.T, projectID, runID, designBody string) string {
	t.Helper()
	root := seedCloseFixture(t, projectID, runID, "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, projectID, runID, "design", designBody)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", projectID, runID}, &out, &errb); code != 0 {
		t.Fatalf("close failed: exit=%d stderr=%q", code, errb.String())
	}
	return root
}

// TestSDLCReopenSeedsDesignAndCarriesTrailer pins the happy path: a
// closed sdlc run's design canvas survives byte-for-byte into the new
// run, the new run carries a MoE-Reopen-Of trailer, and the slug
// date-suffixes off the prior slug's base.
func TestSDLCReopenSeedsDesignAndCarriesTrailer(t *testing.T) {
	const design = "# Fix it\n\nThe write-up.\n"
	root := seedClosedSDLCRun(t, "tele", "fix-it", design)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "fix-it-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele "+dated+" (reopen of fix-it)") {
		t.Fatalf("missing open confirmation for %q in %q", dated, out.String())
	}

	// Seed: design canvas survives byte-for-byte.
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", dated, "design")))
	if err != nil {
		t.Fatalf("seeded design missing: %v", err)
	}
	if string(body) != design {
		t.Fatalf("design not carried verbatim: got %q want %q", body, design)
	}

	// Code canvas explicitly not seeded — reopen carries forward design only.
	if _, err := os.Stat(filepath.Join(root, run.ContentPath("tele", dated, "code"))); !os.IsNotExist(err) {
		t.Fatalf("code canvas should not be seeded by reopen, stat err=%v", err)
	}

	// Open commit subject + trailers.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Open run tele "+dated+" from reopen of fix-it") {
		t.Fatalf("open subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: " + dated,
		"MoE-Project: tele",
		"MoE-Reopen-Of: fix-it",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("open commit missing %q:\n%s", want, head)
		}
	}

	// Prior run.json still says "closed" — reopen does not mutate the source.
	prior, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "fix-it", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prior), `"status": "closed"`) {
		t.Fatalf("source run.json status mutated under reopen:\n%s", prior)
	}
}

// TestSDLCReopenSeedsKickoffWhenSourceEmpty: operator opens a run,
// bails without writing any design, closes. Reopen of that run must
// not refuse — the slug-base + workspace inheritance is the verb's
// value even when there's no canvas to carry. New run gets an
// engine-written kickoff naming the prior slug.
func TestSDLCReopenSeedsKickoffWhenSourceEmpty(t *testing.T) {
	root := seedCloseFixture(t, "tele", "blank", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele", "blank"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup close failed")
	}
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "blank"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "blank-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele "+dated+" (reopen of blank)") {
		t.Fatalf("missing open confirmation for %q in %q", dated, out.String())
	}

	// Seed: design canvas non-empty and names the prior slug, so the
	// design agent's first turn sees why this retake exists.
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", dated, "design")))
	if err != nil {
		t.Fatalf("seeded design missing: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("seeded design is empty; expected kickoff text")
	}
	if !strings.Contains(string(body), "blank") {
		t.Fatalf("kickoff seed should name the prior slug, got:\n%s", body)
	}

	// Trailer carries the link regardless of canvas presence.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "MoE-Reopen-Of: blank") {
		t.Fatalf("open commit missing MoE-Reopen-Of trailer:\n%s", head)
	}
}

// TestSDLCReopenRefusesInProgress: the design's "just keep working"
// guidance — reopen of an in-progress run is a usage error, not a
// silent no-op.
func TestSDLCReopenRefusesInProgress(t *testing.T) {
	root := seedCloseFixture(t, "tele", "still-here", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	beforeHead := gitLog(t, root, "-1", "--format=%H")
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "still-here"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "in_progress") {
		t.Fatalf("expected in-progress refusal, got: %q", errb.String())
	}
	if afterHead := gitLog(t, root, "-1", "--format=%H"); beforeHead != afterHead {
		t.Fatalf("refused reopen created a commit")
	}
}

// TestSDLCReopenRefusesMissingRun: a slug that was never opened
// surfaces as a clean error, not a panic or filesystem leak.
func TestSDLCReopenRefusesMissingRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "run not found") {
		t.Fatalf("expected run-not-found error, got: %q", errb.String())
	}
}

// TestSDLCReopenRefusesNonSDLC: reopen lives under the sdlc verb and
// only seeds a "design" doc. A kb prior would either error at
// run.New (no design stage) or silently land in the wrong workflow.
// Refuse early and explicitly.
func TestSDLCReopenRefusesNonSDLC(t *testing.T) {
	root := seedCloseFixture(t, "tele", "kb-prior", "kb", run.StatusClosed)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "kb-prior"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not sdlc") {
		t.Fatalf("expected workflow-mismatch error, got: %q", errb.String())
	}
}

// TestSDLCReopenStripsDatedSuffix: reopening a slug that already has a
// `-YYYY-MM-DD` suffix lands the new run on `<base>-<today>` rather
// than stacking dates. Mirrors the design's "Slug naturally lands as
// <base>-YYYY-MM-DD" clause.
func TestSDLCReopenStripsDatedSuffix(t *testing.T) {
	const design = "# Original\n"
	_ = seedClosedSDLCRun(t, "tele", "search-2025-12-01", design)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele", "search-2025-12-01"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "search-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele "+dated) {
		t.Fatalf("expected dated suffix stripped to %q, got: %q", dated, out.String())
	}
}

// TestStripDateSuffix pins the slug rewrite: dated and same-day-count
// suffixes drop; plain `-N` collision suffixes don't (they aren't
// shaped like dates and the base-stripping path in run.New already
// handles them).
func TestStripDateSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", "foo"},
		{"foo-bar", "foo-bar"},
		{"foo-2025-12-01", "foo"},
		{"foo-2025-12-01-2", "foo"},
		{"foo-bar-2025-12-01", "foo-bar"},
		{"foo-2", "foo-2"},               // not a date
		{"foo-25-12-01", "foo-25-12-01"}, // 2-digit year is not the shape
		{"foo-2025-13-01", "foo"},        // out-of-range month accepted: shape match only.
		{"2025-12-01", "2025-12-01"},     // no `-` prefix on the suffix → no strip
	}
	for _, tc := range cases {
		if got := stripDateSuffix(tc.in); got != tc.want {
			t.Errorf("stripDateSuffix(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// TestSDLCReopenDashMarkerDropsAfterReopen: a closed sdlc run shows
// the "· reopen?" marker. After reopen creates a successor, the prior
// run's marker disappears — its MoE-Reopen-Of chain has been
// extended, so it's no longer a candidate. The successor (now
// in-progress) lands in ACTIVE without a marker either way.
func TestSDLCReopenDashMarkerDropsAfterReopen(t *testing.T) {
	const design = "# Carry me\n"
	_ = seedClosedSDLCRun(t, "tele", "fix-it", design)

	// Pre-reopen: marker present.
	var out, errb bytes.Buffer
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("dash 1 exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sdlc:closed · reopen?") {
		t.Fatalf("expected pre-reopen marker on closed run:\n%s", out.String())
	}

	// Reopen.
	suppressNextStagePrompt(t)
	if code := Run([]string{"sdlc", "reopen", "tele", "fix-it"}, &bytes.Buffer{}, &errb); code != 0 {
		t.Fatalf("reopen exit failed: stderr=%q", errb.String())
	}

	// Post-reopen: marker gone on prior, successor in ACTIVE.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("dash 2 exit=%d stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "sdlc:closed · reopen?") {
		t.Fatalf("reopen marker should be gone after successor is opened:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sdlc:closed") {
		t.Fatalf("prior closed run should still appear (without marker):\n%s", out.String())
	}
	if !strings.Contains(out.String(), "fix-it-"+todayDateSuffix()) {
		t.Fatalf("successor run should appear in dash:\n%s", out.String())
	}
}

// TestDashClosedNonSDLCHasNoReopenMarker: reopen is an sdlc verb;
// surfacing the marker on kb rows would advertise an action the
// operator can't take.
func TestDashClosedNonSDLCHasNoReopenMarker(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "kb-dead", "kb", run.StatusClosed)
	trailerstest.CommitTrailer(t, root, "Close kb run tele kb-dead",
		"MoE-Run: kb-dead\nMoE-Project: tele\nMoE-Workflow: kb",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "kb:closed") {
		t.Fatalf("expected kb closed row, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "reopen?") {
		t.Fatalf("non-sdlc closed row should not carry the reopen marker:\n%s", out.String())
	}
}

// TestSDLCReopenRegisteredInUsage: the sdlc usage listing surfaces
// reopen alongside new/design/code/push/close so an operator
// discovering the verb via `moe sdlc` sees it.
func TestSDLCReopenRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "reopen") {
		t.Fatalf("sdlc usage missing 'reopen':\n%s", out.String())
	}
}
