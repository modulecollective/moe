package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestFindChainedDescendantsEmptyOnZeroLineage: a slug nobody promoted
// or reopened from yields no descendants. The literal-slug contract
// still wins downstream, but at this layer "nothing in the journal
// claims me as an ancestor" must surface as a clean empty result.
func TestFindChainedDescendantsEmptyOnZeroLineage(t *testing.T) {
	idx := &run.JournalIndex{
		LastActivity: map[string]time.Time{},
		PromotedTo:   map[string]string{},
		ReopenedFrom: map[string]string{},
		PRURL:        map[string]string{},
		WorkTurnTime: map[run.WorkTurnKey]time.Time{},
	}
	got := findChainedDescendants(idx, "tele", "no-one")
	if len(got) != 0 {
		t.Fatalf("expected no descendants, got %+v", got)
	}
}

// TestFindChainedDescendantsPromoteThenReopen: the design's worked
// example — typed slug `foo` was promoted to `foo-2026-05-14`, which
// terminated and was reopened as `foo-2026-05-14-2`. The walker
// reports both descendants, most-recent first.
func TestFindChainedDescendantsPromoteThenReopen(t *testing.T) {
	now := time.Now().UTC()
	idx := &run.JournalIndex{
		LastActivity: map[string]time.Time{
			"foo":              now.Add(-72 * time.Hour),
			"foo-2026-05-14":   now.Add(-24 * time.Hour),
			"foo-2026-05-14-2": now.Add(-1 * time.Hour),
		},
		PromotedTo: map[string]string{
			"foo": "tele/foo-2026-05-14",
		},
		ReopenedFrom: map[string]string{
			"foo-2026-05-14-2": "foo-2026-05-14",
		},
	}
	got := findChainedDescendants(idx, "tele", "foo")
	if len(got) != 2 {
		t.Fatalf("expected 2 descendants, got %+v", got)
	}
	if got[0].slug != "foo-2026-05-14-2" {
		t.Errorf("most-recent first: got[0]=%q, want foo-2026-05-14-2", got[0].slug)
	}
	if got[1].slug != "foo-2026-05-14" {
		t.Errorf("least-recent last: got[1]=%q, want foo-2026-05-14", got[1].slug)
	}
}

// TestFindChainedDescendantsDropsCrossProjectPromote: a future cross-
// project promote (none today, but the trailer carries the project
// field for a reason) must not surface as a same-project descendant.
func TestFindChainedDescendantsDropsCrossProjectPromote(t *testing.T) {
	idx := &run.JournalIndex{
		LastActivity: map[string]time.Time{},
		PromotedTo: map[string]string{
			"foo": "other-project/foo-2026-05-14",
		},
	}
	got := findChainedDescendants(idx, "tele", "foo")
	if len(got) != 0 {
		t.Fatalf("expected no descendants from cross-project promote, got %+v", got)
	}
}

// TestSplitPromotedToShape pins the parser: the only writer today
// emits `<project>/<id>`; anything else surfaces as parsed=false.
func TestSplitPromotedToShape(t *testing.T) {
	cases := []struct {
		in       string
		wantProj string
		wantID   string
		wantOK   bool
	}{
		{"tele/foo", "tele", "foo", true},
		{"tele/foo-2026-05-14", "tele", "foo-2026-05-14", true},
		{"", "", "", false},
		{"tele", "", "", false},
		{"tele/", "", "", false},
		{"/foo", "", "", false},
		{"a/b/c", "", "", false},
	}
	for _, tc := range cases {
		p, id, ok := splitPromotedTo(tc.in)
		if p != tc.wantProj || id != tc.wantID || ok != tc.wantOK {
			t.Errorf("splitPromotedTo(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, p, id, ok, tc.wantProj, tc.wantID, tc.wantOK)
		}
	}
}

// seedChainedFixture stamps the worked example onto a fresh
// bureaucracy: idea `foo` (status=promoted) → sdlc run
// `foo-2026-05-14`, optionally reopened to `foo-2026-05-14-2`. Returns
// the root so tests can drive sdlc verbs against it.
//
// Each MoE-Run-tagged commit is explicitly backdated so the journal
// index's LastActivity column orders the two descendants
// deterministically (older first → newer last is the natural creation
// order; the resolver sorts most-recent first, so the -2 variant must
// surface above the original).
func seedChainedFixture(t *testing.T, withReopen bool) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")

	now := time.Now().UTC()

	// Idea run (promoted). Original capture lands oldest; the promote
	// commit pins LastActivity for `foo` to -72h.
	trailerstest.SeedRun(t, root, "tele", "foo", "idea", run.StatusPromoted)
	trailerstest.CommitTrailer(t, root, "Promote idea tele foo → tele foo-2026-05-14",
		"MoE-Run: foo\nMoE-Project: tele\nMoE-Workflow: idea\nMoE-Promoted-To: tele/foo-2026-05-14",
		now.Add(-72*time.Hour))

	// Destination sdlc run — backdate its LastActivity to -48h with an
	// explicit MoE-Run-tagged commit so the resolver doesn't tie-break
	// on real-wall-clock SeedRun timing.
	trailerstest.SeedRun(t, root, "tele", "foo-2026-05-14", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update design",
		"MoE-Run: foo-2026-05-14\nMoE-Project: tele\nMoE-Workflow: sdlc",
		now.Add(-48*time.Hour))

	if withReopen {
		trailerstest.SeedRun(t, root, "tele", "foo-2026-05-14-2", "sdlc", run.StatusInProgress)
		// Reopen trailer at -2h is the most recent activity in the
		// fixture — places -2 above the original at the multi prompt.
		trailerstest.CommitTrailer(t, root, "Open run tele foo-2026-05-14-2 from reopen of foo-2026-05-14: T",
			"MoE-Run: foo-2026-05-14-2\nMoE-Project: tele\nMoE-Workflow: sdlc\nMoE-Reopen-Of: foo-2026-05-14",
			now.Add(-2*time.Hour))
	}
	return root
}

// TestSDLCCodeNotFoundLineageHintsNoTTY: operator typed `sdlc code
// tele foo` after foo was promoted. With stdin not a tty (the test
// process), the resolver prints the standard not-found error plus a
// `hint:` line pointing at the dated descendant. Non-zero exit.
func TestSDLCCodeNotFoundLineageHintsNoTTY(t *testing.T) {
	root := seedChainedFixture(t, false)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/foo"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q stderr=%q", out.String(), errb.String())
	}
	// Typed slug is the idea itself, so the resolver names the
	// workflow mismatch rather than a bare "run not found". The hint
	// line still points at the live descendant.
	if !strings.Contains(errb.String(), "sdlc code: tele/foo is a idea run, not sdlc") {
		t.Fatalf("missing workflow-mismatch preamble:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "hint: moe sdlc code tele/foo-2026-05-14") {
		t.Fatalf("missing hint:\n%s", errb.String())
	}
}

// TestSDLCCodeNotFoundMultiDescendantList: typed slug with two live
// descendants prints both candidates as runnable invocations, no
// prompt and no default. Most-recent first.
func TestSDLCCodeNotFoundMultiDescendantList(t *testing.T) {
	root := seedChainedFixture(t, true)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/foo"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "did you mean one of:") {
		t.Fatalf("missing multi-descendant header:\n%s", errb.String())
	}
	// foo-2026-05-14 is a prefix of foo-2026-05-14-2, so a plain
	// substring lookup can't disambiguate. Walk the lines that name
	// each invocation and assert the -2 variant appears first.
	var order []string
	for _, line := range strings.Split(errb.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "moe sdlc code tele/foo") {
			continue
		}
		order = append(order, trimmed)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 suggested invocations, got %d:\n%s", len(order), errb.String())
	}
	if order[0] != "moe sdlc code tele/foo-2026-05-14-2" {
		t.Fatalf("most-recent first: order[0]=%q, want foo-2026-05-14-2", order[0])
	}
	if order[1] != "moe sdlc code tele/foo-2026-05-14" {
		t.Fatalf("order[1]=%q, want foo-2026-05-14", order[1])
	}
}

// TestSDLCCodeNotFoundZeroDescendants: a slug with no lineage at all
// surfaces as the standard not-found error, no hint, no list.
func TestSDLCCodeNotFoundZeroDescendants(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "sdlc code: run not found: tele/ghost") {
		t.Fatalf("missing not-found:\n%s", errb.String())
	}
	if strings.Contains(errb.String(), "hint:") || strings.Contains(errb.String(), "did you mean") {
		t.Fatalf("zero-descendants path leaked a fallback hint:\n%s", errb.String())
	}
}

// TestResolveSDLCRunSlugTTYAcceptsDescendant: with the tty path mocked
// on (via the testable WithMode entry), Y / Enter resolves the typed
// slug to its dated descendant and the resolver returns code=0.
func TestResolveSDLCRunSlugTTYAcceptsDescendant(t *testing.T) {
	root := seedChainedFixture(t, false)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	withStdinLine(t, "y\n")

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCRunSlugWithMode("sdlc code", "tele", "foo", true, &out, &errb)
	if code != 0 {
		t.Fatalf("expected code=0, got %d (stderr=%q)", code, errb.String())
	}
	if resolved != "foo-2026-05-14" {
		t.Fatalf("resolved=%q, want foo-2026-05-14", resolved)
	}
	if !strings.Contains(out.String(), "did you mean foo-2026-05-14? [Y/n]") {
		t.Fatalf("prompt text missing:\n%s", out.String())
	}
}

// TestResolveSDLCRunSlugTTYAcceptsOnBlank: a bare newline (operator
// pressed Enter on the [Y/n] default) accepts.
func TestResolveSDLCRunSlugTTYAcceptsOnBlank(t *testing.T) {
	root := seedChainedFixture(t, false)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	withStdinLine(t, "\n")

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCRunSlugWithMode("sdlc code", "tele", "foo", true, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if resolved != "foo-2026-05-14" {
		t.Fatalf("resolved=%q, want foo-2026-05-14", resolved)
	}
}

// TestResolveSDLCRunSlugTTYDeclinesOnN: typing N (or anything starting
// with n) at the prompt declines — resolver returns the standard
// not-found error and a non-zero code.
func TestResolveSDLCRunSlugTTYDeclinesOnN(t *testing.T) {
	root := seedChainedFixture(t, false)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	withStdinLine(t, "n\n")

	var out, errb bytes.Buffer
	_, code := resolveSDLCRunSlugWithMode("sdlc code", "tele", "foo", true, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on decline, stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "sdlc code: tele/foo is a idea run, not sdlc") {
		t.Fatalf("decline should surface the workflow-mismatch error for the typed slug:\n%s", errb.String())
	}
}

// TestResolveSDLCRunSlugExactMatchPassthrough: the happy path is a
// straight passthrough — when the typed slug loads, the resolver
// returns it verbatim and doesn't reach the journal index.
func TestResolveSDLCRunSlugExactMatchPassthrough(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "tele", "live", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	resolved, code := resolveSDLCRunSlugWithMode("sdlc code", "tele", "live", false, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if resolved != "live" {
		t.Fatalf("resolved=%q, want live", resolved)
	}
}

// withStdinLine replaces os.Stdin with a pipe carrying line, restoring
// the original on cleanup. Mirrors the cascade_test pattern but kept
// local so the chain tests don't accidentally outlive the redirection.
func withStdinLine(t *testing.T, line string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, line); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		r.Close()
	})
}
