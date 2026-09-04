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

// seedTerminalSDLCRun stamps a single sdlc run in the given terminal
// status onto a fresh bureaucracy, with MOE_HOME pointed at it. No
// lineage, no descendants — the plain "operator re-typed the stage verb
// on a run that already shipped" fixture.
func seedTerminalSDLCRun(t *testing.T, projectID, runID, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, projectID)
	trailerstest.SeedRun(t, root, projectID, runID, "sdlc", status)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	return root
}

// TestSDLCStageRefusesTerminalRunAtInteractiveDoor is the regression
// this run exists for: re-typing a stage verb on a merged run used to
// open a session on it, leaving the dash showing COMPLETED while agents
// committed turns that `moe sdlc push` would later refuse to ship. All
// four interactive openers now refuse, with the reopen hint. Table
// covers the three terminal statuses on one verb plus every verb on
// merged (the status the trap was found in).
func TestSDLCStageRefusesTerminalRunAtInteractiveDoor(t *testing.T) {
	cases := []struct {
		verb   string
		status string
	}{
		{verb: "design", status: run.StatusMerged},
		{verb: "code", status: run.StatusMerged},
		{verb: "review", status: run.StatusMerged},
		{verb: "test", status: run.StatusMerged},
		{verb: "code", status: run.StatusClosed},
		{verb: "code", status: run.StatusPromoted},
	}
	for _, tc := range cases {
		t.Run(tc.verb+"-"+tc.status, func(t *testing.T) {
			seedTerminalSDLCRun(t, "tele", "zombie", tc.status)

			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", tc.verb, "tele/zombie"}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s run; stdout=%q", tc.status, out.String())
			}
			want := "sdlc " + tc.verb + ": tele/zombie is " + tc.status + "; reopen it to keep iterating"
			if !strings.Contains(errb.String(), want) {
				t.Fatalf("missing refusal %q:\n%s", want, errb.String())
			}
			if !strings.Contains(errb.String(), "hint: moe sdlc reopen tele/zombie") {
				t.Fatalf("missing reopen hint:\n%s", errb.String())
			}
		})
	}
}

// TestSDLCStageAllowsPushedRunAtInteractiveDoor: `pushed` is not part of
// the zombie class. Iterating against PR feedback before the merge lands
// is a real flow — push on a pushed run re-pushes the same PR, and the
// dash renders pushed as ACTIVE — so the guard passes it through
// verbatim.
func TestSDLCStageAllowsPushedRunAtInteractiveDoor(t *testing.T) {
	seedTerminalSDLCRun(t, "tele", "awaiting", run.StatusPushed)

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCReentryWithMode("sdlc code", "tele", "awaiting", false, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d, want 0; stderr=%q", code, errb.String())
	}
	if resolved != "awaiting" {
		t.Fatalf("resolved=%q, want awaiting", resolved)
	}
}

// TestSDLCStageInProgressPassesThrough: the overwhelmingly common case
// costs one run.Load and returns the typed slug unchanged.
func TestSDLCStageInProgressPassesThrough(t *testing.T) {
	seedTerminalSDLCRun(t, "tele", "live", run.StatusInProgress)

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCReentryWithMode("sdlc code", "tele", "live", true, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d, want 0; stderr=%q", code, errb.String())
	}
	if resolved != "live" {
		t.Fatalf("resolved=%q, want live", resolved)
	}
	if out.Len() != 0 {
		t.Fatalf("a live run must not prompt; stdout=%q", out.String())
	}
}

// seedMergedWithLiveDescendant builds the chained fixture and marks its
// sdlc run merged, so `foo-2026-05-14` is a terminal run whose reopen
// descendant `foo-2026-05-14-2` is still live.
func seedMergedWithLiveDescendant(t *testing.T) string {
	t.Helper()
	root := seedChainedFixture(t, true)
	markRunStatus(t, root, "tele", "foo-2026-05-14", run.StatusMerged)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	return root
}

// TestSDLCStageForwardHintNoTTY: a terminal run whose reopen chain is
// already extended points at the descendant, not at another reopen — a
// second reopen off the same prior would fork the lineage.
func TestSDLCStageForwardHintNoTTY(t *testing.T) {
	seedMergedWithLiveDescendant(t)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/foo-2026-05-14"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero; stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "sdlc code: tele/foo-2026-05-14 is merged; foo-2026-05-14-2 carries it forward") {
		t.Fatalf("missing forward refusal:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "hint: moe sdlc code tele/foo-2026-05-14-2") {
		t.Fatalf("missing forward hint:\n%s", errb.String())
	}
	if strings.Contains(errb.String(), "sdlc reopen") {
		t.Fatalf("extended chain must not advertise a second reopen:\n%s", errb.String())
	}
}

// TestSDLCReentryTTYForwardsToDescendant: on a tty the guard acts rather
// than only naming the forward — Y resolves to the live descendant and
// the typed stage continues there.
func TestSDLCReentryTTYForwardsToDescendant(t *testing.T) {
	seedMergedWithLiveDescendant(t)
	withStdinLine(t, "y\n")

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCReentryWithMode("sdlc code", "tele", "foo-2026-05-14", true, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d, want 0; stderr=%q", code, errb.String())
	}
	if resolved != "foo-2026-05-14-2" {
		t.Fatalf("resolved=%q, want foo-2026-05-14-2", resolved)
	}
	if !strings.Contains(out.String(), "is merged — did you mean foo-2026-05-14-2? [Y/n]") {
		t.Fatalf("prompt text missing:\n%s", out.String())
	}
}

// TestSDLCReentryTTYDeclineRefuses: N at the forward prompt lands on the
// refusal, not on a session. The safe default at a "did you mean" prompt
// is to bail — same contract readChainAccept already carries for the
// not-found path.
func TestSDLCReentryTTYDeclineRefuses(t *testing.T) {
	seedMergedWithLiveDescendant(t)
	withStdinLine(t, "n\n")

	var out, errb bytes.Buffer
	_, code := resolveSDLCReentryWithMode("sdlc code", "tele", "foo-2026-05-14", true, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on decline; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "carries it forward") {
		t.Fatalf("decline should surface the forward refusal:\n%s", errb.String())
	}
}

// TestSDLCReentryTTYMintsReopen is the no-descendant tty leg: Y runs the
// same mint `moe sdlc reopen` runs and returns the fresh slug, so the
// typed stage opens on a run whose pushes can actually ship. The design
// canvas carries over byte-for-byte, which is also what makes the
// straight-to-code path legal (requireDesignCanvas is satisfied).
func TestSDLCReentryTTYMintsReopen(t *testing.T) {
	const design = "# widget\n\nprior design body\n"
	root := seedClosedSDLCRun(t, "tele", "widget", design)
	withStdinLine(t, "y\n")

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCReentryWithMode("sdlc code", "tele", "widget", true, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d, want 0; stderr=%q", code, errb.String())
	}
	wantSlug := "widget-" + todayDateSuffix()
	if resolved != wantSlug {
		t.Fatalf("resolved=%q, want %q", resolved, wantSlug)
	}
	if !strings.Contains(out.String(), "is closed — reopen it as a fresh run? [Y/n]") {
		t.Fatalf("prompt text missing:\n%s", out.String())
	}
	md := loadDatedRunJSON(t, root, "tele", "widget")
	if md.Status != run.StatusInProgress {
		t.Fatalf("fresh run status=%q, want in_progress", md.Status)
	}
	if md.ReopenOf != "widget" {
		t.Fatalf("fresh run reopen_of=%q, want widget", md.ReopenOf)
	}
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", wantSlug, "design")))
	if err != nil {
		t.Fatalf("read seeded design canvas: %v", err)
	}
	if string(body) != design {
		t.Fatalf("seeded design canvas = %q, want %q", string(body), design)
	}
}

// TestSDLCReentryTTYDeclineOfMintRefuses: N at the reopen prompt refuses
// with the hint — the operator may have meant a different run, and
// minting on a shrug is worse than one retyped command.
func TestSDLCReentryTTYDeclineOfMintRefuses(t *testing.T) {
	root := seedTerminalSDLCRun(t, "tele", "zombie", run.StatusMerged)
	withStdinLine(t, "n\n")

	var out, errb bytes.Buffer
	_, code := resolveSDLCReentryWithMode("sdlc code", "tele", "zombie", true, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on decline; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "hint: moe sdlc reopen tele/zombie") {
		t.Fatalf("decline should surface the reopen hint:\n%s", errb.String())
	}
	if entries, err := os.ReadDir(filepath.Join(root, "projects", "tele", "runs")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("decline minted something: %d run dirs, want 1", len(entries))
	}
}

// TestSDLCReentryTTYEOFRefusesMint is the end-to-end leg for the bare-EOF
// branch: Ctrl-D at the reopen prompt used to fall through to the
// blank-line default and mint a dated successor, so an abort key made a
// state change on disk. It must now refuse exactly as `n` does, leaving
// the closed run the only run under the project.
func TestSDLCReentryTTYEOFRefusesMint(t *testing.T) {
	root := seedClosedSDLCRun(t, "tele", "widget", "# widget\n\nprior design body\n")
	withStdinLine(t, "")

	var out, errb bytes.Buffer
	_, code := resolveSDLCReentryWithMode("sdlc code", "tele", "widget", true, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on EOF; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "hint: moe sdlc reopen tele/widget") {
		t.Fatalf("EOF should surface the reopen hint:\n%s", errb.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "projects", "tele", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "widget" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("EOF minted a successor: run dirs = %v, want [widget]", names)
	}
}

// TestSDLCReentryAmbiguousChainLists: two live descendants off the same
// terminal prior is ambiguous — the guard lists both as runnable
// invocations and picks nothing, mirroring resolveSDLCRunSlug's
// multi-descendant shape.
func TestSDLCReentryAmbiguousChainLists(t *testing.T) {
	root := seedMergedWithLiveDescendant(t)
	trailerstest.SeedRun(t, root, "tele", "foo-2026-05-14-3", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "Open run tele foo-2026-05-14-3 from reopen of foo-2026-05-14: T",
		"MoE-Run: foo-2026-05-14-3\nMoE-Project: tele\nMoE-Workflow: sdlc\nMoE-Reopen-Of: foo-2026-05-14",
		time.Now().UTC().Add(-1*time.Hour))

	var out, errb bytes.Buffer
	// tty=true: ambiguity refuses on both stdin modes, no default to offer.
	_, code := resolveSDLCReentryWithMode("sdlc code", "tele", "foo-2026-05-14", true, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on ambiguous chain; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "more than one live run") {
		t.Fatalf("missing ambiguity preamble:\n%s", errb.String())
	}
	for _, slug := range []string{"foo-2026-05-14-2", "foo-2026-05-14-3"} {
		if !strings.Contains(errb.String(), "  moe sdlc code tele/"+slug) {
			t.Fatalf("missing candidate %s:\n%s", slug, errb.String())
		}
	}
}

// TestSDLCReentrySkipsTerminalDescendant: a descendant that has itself
// terminated isn't a forward target — the walk is transitive, so its own
// live successor is the one offered. Here `-2` is merged and `-3` was
// reopened from it, so the single live link is `-3`.
func TestSDLCReentrySkipsTerminalDescendant(t *testing.T) {
	root := seedMergedWithLiveDescendant(t)
	markRunStatus(t, root, "tele", "foo-2026-05-14-2", run.StatusMerged)
	trailerstest.SeedRun(t, root, "tele", "foo-2026-05-14-3", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "Open run tele foo-2026-05-14-3 from reopen of foo-2026-05-14-2: T",
		"MoE-Run: foo-2026-05-14-3\nMoE-Project: tele\nMoE-Workflow: sdlc\nMoE-Reopen-Of: foo-2026-05-14-2",
		time.Now().UTC().Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/foo-2026-05-14"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero; stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "hint: moe sdlc code tele/foo-2026-05-14-3") {
		t.Fatalf("expected the live tail of the chain in the hint:\n%s", errb.String())
	}
}

// TestSDLCStageGuardRunsBeforeAgentPersist pins the ordering: --agent
// used to be written to run.json before the opener ran, so a refused (or
// forwarded) re-entry would still have stamped the agent onto the run the
// operator typed. The guard now runs first, so a refusal leaves run.json
// untouched.
func TestSDLCStageGuardRunsBeforeAgentPersist(t *testing.T) {
	root := seedTerminalSDLCRun(t, "tele", "zombie", run.StatusMerged)

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "--agent", "codex", "tele/zombie"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero; stdout=%q", out.String())
	}
	md, err := run.Load(root, "tele", "zombie")
	if err != nil {
		t.Fatal(err)
	}
	if md.Agent != "" {
		t.Fatalf("agent persisted onto a refused run: %q", md.Agent)
	}
	if strings.Contains(out.String(), "switched run agent") {
		t.Fatalf("agent switch committed on a refused run:\n%s", out.String())
	}
}
