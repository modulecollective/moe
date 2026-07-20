package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/workspace"
)

// seedClosedSDLCRun composes a closed sdlc run with a populated design
// canvas — the precondition reopen reads from. Registers the design
// doc + commits the canvas (so close's clean-tree gate passes), then
// runs `sdlc close` to flip status. The resulting fixture has the
// run.json status=closed, the design canvas on disk, and a clean
// working tree.
func seedClosedSDLCRun(t *testing.T, projectID, runID, designBody string) string {
	t.Helper()
	return seedClosedSDLCRunWithFields(t, projectID, runID, designBody, nil)
}

// seedClosedSDLCRunWithFields is seedClosedSDLCRun plus a hook to stamp
// extra metadata (Workspace, Agent, …) onto the prior run.json before
// close. Used by reopen tests that need to assert inherit / override /
// detach semantics against a non-default prior state.
func seedClosedSDLCRunWithFields(t *testing.T, projectID, runID, designBody string, mutate func(*run.Metadata)) string {
	t.Helper()
	root := seedCloseFixture(t, projectID, runID, "sdlc", run.StatusInProgress)
	if mutate != nil {
		md, err := run.Load(root, projectID, runID)
		if err != nil {
			t.Fatalf("run.Load: %v", err)
		}
		mutate(md)
		if err := run.Save(root, md); err != nil {
			t.Fatalf("run.Save: %v", err)
		}
		runJSONRel := filepath.Join(run.Dir(projectID, runID), "run.json")
		gittest.Run(t, root, "add", runJSONRel)
		gittest.Run(t, root, "commit", "-m", "stamp metadata on "+runID)
	}
	addDocEntryAndCommit(t, root, projectID, runID, "design", designBody)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", projectID + "/" + runID}, &out, &errb); code != 0 {
		t.Fatalf("close failed: exit=%d stderr=%q", code, errb.String())
	}
	return root
}

// loadDatedRunJSON reads the freshly-opened reopen successor's run.json
// (slug = base + "-" + todayDateSuffix()) and returns the parsed
// metadata. Tests use it to assert Workspace / Agent inherit / override.
func loadDatedRunJSON(t *testing.T, root, projectID, base string) run.Metadata {
	t.Helper()
	slug := base + "-" + todayDateSuffix()
	body, err := os.ReadFile(filepath.Join(root, "projects", projectID, "runs", slug, "run.json"))
	if err != nil {
		t.Fatalf("read successor run.json: %v", err)
	}
	var md run.Metadata
	if err := json.Unmarshal(body, &md); err != nil {
		t.Fatalf("parse successor run.json: %v", err)
	}
	return md
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
	code := Run([]string{"sdlc", "reopen", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "fix-it-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele/"+dated+" (reopen of fix-it)") {
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
	if !strings.Contains(head, "Open run tele/"+dated+" from reopen of fix-it") {
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

	// New run.json carries the reopen_of field so the stage prompt
	// assembler can name the prior without walking git per turn.
	newRun, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", dated, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(newRun), `"reopen_of": "fix-it"`) {
		t.Fatalf("new run.json missing reopen_of pointer to prior:\n%s", newRun)
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
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/blank"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("setup close failed")
	}
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele/blank"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "blank-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele/"+dated+" (reopen of blank)") {
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
	code := Run([]string{"sdlc", "reopen", "tele/still-here"}, &out, &errb)
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
	code := Run([]string{"sdlc", "reopen", "tele/ghost"}, &out, &errb)
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
	code := Run([]string{"sdlc", "reopen", "tele/kb-prior"}, &out, &errb)
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
//
// The base run `search` is seeded alongside the dated one on purpose: a
// `search-2025-12-01` slug is only ever minted when base `search` was
// already taken, and runs aren't deleted, so the base dir persists. With
// it present, run.New's dated-collision path fires off the stripped base
// (`os.Stat` on the dir reports taken) — the same real-lifecycle state
// the old unanchored SlugTaken used to fake via a prefix match on the
// dated slug's own history.
func TestSDLCReopenStripsDatedSuffix(t *testing.T) {
	const design = "# Original\n"
	root := seedClosedSDLCRun(t, "tele", "search-2025-12-01", design)
	trailerstest.SeedRun(t, root, "tele", "search", "sdlc", run.StatusClosed)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele/search-2025-12-01"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "search-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele/"+dated) {
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
	if code := Run([]string{"sdlc", "reopen", "tele/fix-it"}, &bytes.Buffer{}, &errb); code != 0 {
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
	trailerstest.CommitTrailer(t, root, "Close kb run tele/kb-dead",
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

// TestSDLCReopenWorkspaceFlagOverridesPrior pins the override path: a
// prior without a workspace (or with a different one) + --workspace=dev
// lands dev in the successor's run.json. This is the "switch on a
// retake" story from the design.
func TestSDLCReopenWorkspaceFlagOverridesPrior(t *testing.T) {
	root := seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--workspace=dev", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Workspace != "dev" {
		t.Fatalf("Workspace = %q, want %q", md.Workspace, "dev")
	}
}

// TestSDLCReopenWorkspaceFlagInheritedWhenOmitted regresses the
// inherit-by-default behavior on the workspace field. Prior carried
// "foo"; no flag → successor reads "foo".
func TestSDLCReopenWorkspaceFlagInheritedWhenOmitted(t *testing.T) {
	root := seedClosedSDLCRunWithFields(t, "tele", "fix-it", "# design\n", func(md *run.Metadata) {
		md.Workspace = "foo"
	})
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Workspace != "foo" {
		t.Fatalf("Workspace = %q, want inherited %q", md.Workspace, "foo")
	}
}

// TestSDLCReopenNoWorkspaceClearsInheritedName covers the detach form.
// Prior used "dev"; --no-workspace lands an empty Workspace so the
// successor gets a fresh per-run sandbox.
func TestSDLCReopenNoWorkspaceClearsInheritedName(t *testing.T) {
	root := seedClosedSDLCRunWithFields(t, "tele", "fix-it", "# design\n", func(md *run.Metadata) {
		md.Workspace = "dev"
	})
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--no-workspace", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Workspace != "" {
		t.Fatalf("Workspace = %q, want empty", md.Workspace)
	}
}

// TestSDLCReopenWorkspaceAndNoWorkspaceConflict: the flag pair is
// mutually exclusive, exit 2 if both set. No state mutated.
func TestSDLCReopenWorkspaceAndNoWorkspaceConflict(t *testing.T) {
	_ = seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--workspace=dev", "--no-workspace", "tele/fix-it"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got: %q", errb.String())
	}
}

// TestSDLCReopenWorkspaceFlagRefusesClaimed: the override name is
// already claimed by another run. The pre-flight must refuse with the
// shared wording (proving the helper is shared with `new`).
func TestSDLCReopenWorkspaceFlagRefusesClaimed(t *testing.T) {
	root := seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	// Plant a claim directly on disk — the reopen pre-flight just
	// reads it (no need for a real submodule under the workspace).
	plantClaim(t, root, "tele", "dev", "tele/other")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--workspace=dev", "tele/fix-it"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "run tele/other") {
		t.Fatalf("expected error to name the holder, got: %q", errb.String())
	}
}

// plantClaim writes a workspace claim file under the layout
// workspace.ReadClaim expects, without requiring a project submodule on
// disk. Use in tests that only need to exercise the claim-refusal path.
func plantClaim(t *testing.T, root, projectID, name, runRef string) {
	t.Helper()
	wp := workspace.Path(root, projectID, name)
	if err := os.MkdirAll(filepath.Join(wp, ".moe"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	c := workspace.Claim{Project: projectID, Name: name, Run: runRef}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wp, ".moe", "claim.json"), b, 0o644); err != nil {
		t.Fatalf("write claim: %v", err)
	}
}

// TestSDLCReopenWorkspaceFlagRejectsInvalidName: workspace.ValidateName
// runs up-front so a typo surfaces at the verb the operator typed
// rather than at first stage attach.
func TestSDLCReopenWorkspaceFlagRejectsInvalidName(t *testing.T) {
	_ = seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--workspace=NOT VALID", "tele/fix-it"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "workspace") {
		t.Fatalf("expected workspace error, got: %q", errb.String())
	}
}

// TestSDLCReopenAgentFlagOverridesPrior: prior carried claude; the
// reopen flag overrides to codex on the successor.
func TestSDLCReopenAgentFlagOverridesPrior(t *testing.T) {
	root := seedClosedSDLCRunWithFields(t, "tele", "fix-it", "# design\n", func(md *run.Metadata) {
		md.Agent = "claude"
	})
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--agent=codex", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Agent != "codex" {
		t.Fatalf("Agent = %q, want %q", md.Agent, "codex")
	}
}

// TestSDLCReopenAgentFlagInheritsPriorWhenOmitted pins the new
// inherit-by-default semantics: the prior silent-drop bug is fixed.
// Prior had codex, no --agent flag, successor reads codex.
func TestSDLCReopenAgentFlagInheritsPriorWhenOmitted(t *testing.T) {
	root := seedClosedSDLCRunWithFields(t, "tele", "fix-it", "# design\n", func(md *run.Metadata) {
		md.Agent = "codex"
	})
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Agent != "codex" {
		t.Fatalf("Agent = %q, want inherited %q", md.Agent, "codex")
	}
}

// TestSDLCReopenNoAgentClearsInheritedAgent: --no-agent leaves the
// successor's Agent empty so the usual $MOE_AGENT → claude precedence
// runs at first stage turn.
func TestSDLCReopenNoAgentClearsInheritedAgent(t *testing.T) {
	root := seedClosedSDLCRunWithFields(t, "tele", "fix-it", "# design\n", func(md *run.Metadata) {
		md.Agent = "codex"
	})
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--no-agent", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "fix-it")
	if md.Agent != "" {
		t.Fatalf("Agent = %q, want empty", md.Agent)
	}
}

// TestSDLCReopenAgentAndNoAgentConflict: the agent flag pair is
// mutually exclusive, exit 2 if both set.
func TestSDLCReopenAgentAndNoAgentConflict(t *testing.T) {
	_ = seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--agent=codex", "--no-agent", "tele/fix-it"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got: %q", errb.String())
	}
}

// TestSDLCReopenAgentFlagRejectsUnknown: an agent name not in the
// registry is refused at the verb instead of at first stage turn.
func TestSDLCReopenAgentFlagRejectsUnknown(t *testing.T) {
	_ = seedClosedSDLCRun(t, "tele", "fix-it", "# design\n")
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--agent=nope", "tele/fix-it"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "unknown backend") {
		t.Fatalf("expected unknown-backend error, got: %q", errb.String())
	}
}

// TestSDLCReopenParkPrintsHintWithoutPrompt: --park opens the reopened
// run and prints the next-stage hint, then stops without the chain
// prompt. The seeded design canvas is not a committed work turn, so
// Workflow.Next lands on design — the run's first live gate.
func TestSDLCReopenParkPrintsHintWithoutPrompt(t *testing.T) {
	seedClosedSDLCRun(t, "tele", "fix-it", "# Fix it\n\nThe write-up.\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--park", "tele/fix-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	dated := "fix-it-" + todayDateSuffix()
	if !strings.Contains(out.String(), "opened run tele/"+dated+" (reopen of fix-it)") {
		t.Fatalf("missing open confirmation for %q in %q", dated, out.String())
	}
	if !strings.Contains(out.String(), "next: moe sdlc design tele/"+dated) {
		t.Fatalf("missing next-stage hint: %q", out.String())
	}
	if strings.Contains(out.String(), "run now?") {
		t.Fatalf("--park must not print the chain prompt: %q", out.String())
	}
}

// The consent ladder on `sdlc reopen` is the same ladder new and the
// stage verbs carry: --ship / --chain / --dynamic map to `!!` / `!!!` /
// `!!!!`. A reopened run mints seeded at design; the cascade rides it
// from there under the flag's ride mode — the seam this verifies. The
// first dispatch must be design on the reopened successor, not the prior.
func TestSDLCReopenCascadeLadderCarriesConsent(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want rideMode
	}{
		{flag: "--ship", want: rideNone},
		{flag: "--chain", want: rideStatic},
		{flag: "--dynamic", want: rideDynamic},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			seedClosedSDLCRun(t, "tele", "fix-it", "# Fix it\n\nThe write-up.\n")

			var modes []rideMode
			type inv struct {
				stage, runID string
			}
			var stages []inv
			prev := openSdlcStage
			openSdlcStage = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
				modes = append(modes, currentRideMode)
				stages = append(stages, inv{stage, runID})
				return 0
			}
			t.Cleanup(func() { openSdlcStage = prev })
			prevGate := checkCascadeStageGate
			checkCascadeStageGate = func(_ *Workflow, _ *run.Metadata, _ string, _ io.Writer) (bool, int) {
				return true, 0
			}
			t.Cleanup(func() { checkCascadeStageGate = prevGate })
			stubPushFromCascade(t, 0, nil)

			var out, errb bytes.Buffer
			if code := Run([]string{"sdlc", "reopen", tc.flag, "tele/fix-it"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
			}
			if len(stages) == 0 {
				t.Fatalf("%s cascaded no stages", tc.flag)
			}
			dated := "fix-it-" + todayDateSuffix()
			if stages[0].stage != "design" || stages[0].runID != dated {
				t.Fatalf("cascade first step = %+v, want design on %s", stages[0], dated)
			}
			for i, got := range modes {
				if got != tc.want {
					t.Fatalf("stage %d ran under ride mode %v, want %v", i, got, tc.want)
				}
			}
			if strings.Contains(out.String(), "run now?") {
				t.Errorf("%s must not print the chain prompt: %q", tc.flag, out.String())
			}
		})
	}
}

// The ladder is a ladder, not a set of composable modifiers — two rungs
// at once is a usage error, refused before any mint.
func TestSDLCReopenLadderRungsMutuallyExclusive(t *testing.T) {
	seedClosedSDLCRun(t, "tele", "fix-it", "# Fix it\n\nThe write-up.\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--chain", "--dynamic", "tele/fix-it"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "one ladder") {
		t.Errorf("expected the ladder error, got: %q", errb.String())
	}
}

// --park is the opposite tail to every rung, not just --ship, and the
// message names the rung the operator actually typed.
func TestSDLCReopenParkExcludesEveryCascadeRung(t *testing.T) {
	for _, flag := range []string{"--ship", "--chain", "--dynamic"} {
		t.Run(flag, func(t *testing.T) {
			seedClosedSDLCRun(t, "tele", "fix-it", "# Fix it\n\nThe write-up.\n")

			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", "reopen", flag, "--park", "tele/fix-it"}, &out, &errb)
			if code != 2 {
				t.Fatalf("expected usage exit (2), got %d stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "opposite tails") || !strings.Contains(errb.String(), flag) {
				t.Errorf("expected an opposite-tails error naming %s, got: %q", flag, errb.String())
			}
		})
	}
}

// A cascade flag doesn't bypass reopen's status guard: an in-progress
// prior still refuses before any mint, so no successor run is created.
func TestSDLCReopenDynamicStillRefusesInProgress(t *testing.T) {
	root := seedCloseFixture(t, "tele", "still-here", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "reopen", "--dynamic", "tele/still-here"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on in-progress prior; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "in_progress") {
		t.Fatalf("expected in-progress refusal, got: %q", errb.String())
	}
	dated := "still-here-" + todayDateSuffix()
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", dated)); !os.IsNotExist(err) {
		t.Fatalf("refused reopen must mint nothing; successor dir exists (err=%v)", err)
	}
}
