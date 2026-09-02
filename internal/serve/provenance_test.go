package serve

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

func provenanceServer(t *testing.T, hops []ProvHop, gatherErr error) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-ci", "sdlc")
	return newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		RunProvenance: func(string, string) ([]ProvHop, error) { return hops, gatherErr },
	})
}

// TestRunPageRendersProvenance: the origin story reaches the page as a
// descent chain — root actor on top, an arrow per step down, spawner
// linked, `agent` badge on the machine hop, ride level, recorded reason.
func TestRunPageRendersProvenance(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{Subject: "operator"},
		{
			Verb: "opened", Object: "alpha/pulse-2026-07-20",
			ObjectURL: "/run/alpha/pulse-2026-07-20",
		},
		{
			Verb: "spawned", Object: "this run",
			Agent: true, Consent: "dynamic",
			Why: "reflect flagged due",
		},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	for _, want := range []string{
		`<h2>provenance</h2>`,
		`operator`,
		`opened`,
		`href="/run/alpha/pulse-2026-07-20"`,
		`spawned`,
		`class="badge agent"`,
		`>dynamic<`,
		`why: reflect flagged due`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q\nbody:\n%s", want, body)
		}
	}
	// One arrow per hop below the root: the root actor is where the story
	// starts, so it must not point at anything above it.
	if got := strings.Count(body, `class="arrow"`); got != 2 {
		t.Errorf("arrow count = %d, want 2 — one per hop below the root\nbody:\n%s", got, body)
	}
	// "this run" is the page itself; linking it back to the reader is the
	// noise the elided subject already avoids.
	if strings.Contains(body, `>this run</a>`) {
		t.Errorf("the terminal hop must not link to the page it's on\nbody:\n%s", body)
	}
}

// TestRunPageNamesAGoneRunWithoutLinkingIt: the walk hands over an empty
// URL for a run that no longer loads, and the template is the only thing
// standing between that and a link to a 404. Both sides of the branch —
// root subject and object line — are pinned here, because every test on
// the cli side asserts hops, not markup.
func TestRunPageNamesAGoneRunWithoutLinkingIt(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{Subject: "alpha/pruned-pulse"},
		{Verb: "spawned", Object: "alpha/pruned-mid", Agent: true},
		{Verb: "spawned", Object: "this run", Agent: true},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	if strings.Contains(body, `href="/run/`) {
		t.Errorf("a chain of gone runs must carry no run link\nbody:\n%s", body)
	}
	if !strings.Contains(body, `<span class="slug">alpha/pruned-pulse</span>`) {
		t.Errorf("the gone root must still be named, unlinked\nbody:\n%s", body)
	}
	if !strings.Contains(body, "alpha/pruned-mid") {
		t.Errorf("the gone mid-chain run must still be named\nbody:\n%s", body)
	}
}

// TestRunPageOmitsArrowOnAOneLineStory: a plain operator-opened run has
// no chain to draw, and an arrow pointing at nothing would be worse than
// the prose it replaced.
func TestRunPageOmitsArrowOnAOneLineStory(t *testing.T) {
	s := provenanceServer(t, []ProvHop{{Verb: "opened by operator"}}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	if !strings.Contains(body, "opened by operator") {
		t.Errorf("the one-hop story must still render\nbody:\n%s", body)
	}
	if strings.Contains(body, `class="arrow"`) {
		t.Errorf("no arrow theater for a one-hop story\nbody:\n%s", body)
	}
}

// TestRunPageOmitsProvenanceSectionWhenEmpty: no hops, no section —
// including when the gather fails. Provenance is enrichment; a journal
// read that errored must not cost the operator the page.
func TestRunPageOmitsProvenanceSectionWhenEmpty(t *testing.T) {
	for name, s := range map[string]*Server{
		"no hops":       provenanceServer(t, nil, nil),
		"gather failed": provenanceServer(t, nil, errors.New("git log exploded")),
	} {
		body := getRunPage(t, s, "/run/alpha/fix-ci")
		if strings.Contains(body, "<h2>provenance</h2>") {
			t.Errorf("%s: provenance section rendered anyway", name)
		}
	}
}

// TestRunPageOmitsConsentWhenUnrecorded: a hop from before the
// MoE-Consent trailer landed still badges (spawned_by has always been
// machine-only) but must not invent a ride level. Absence is unknown,
// and the page says nothing rather than guessing.
func TestRunPageOmitsConsentWhenUnrecorded(t *testing.T) {
	s := provenanceServer(t, []ProvHop{{
		Verb: "opened by", Object: "alpha/pulse-old", ObjectURL: "/run/alpha/pulse-old",
		Agent: true,
	}}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	if !strings.Contains(body, `class="badge agent"`) {
		t.Error("the agent badge must survive an unrecorded consent level")
	}
	if strings.Contains(body, `class="consent"`) {
		t.Error("an unrecorded consent level must render nothing, not a guess")
	}
}

// TestAgentMarkPrefersTheSpawnClaim: a run can be both machine-opened
// and machine-chained. One badge carries both, and the stronger claim —
// how the run came to exist — wins the hover.
func TestAgentMarkPrefersTheSpawnClaim(t *testing.T) {
	cases := []struct {
		name      string
		row       dash.Row
		wantBadge bool
		wantTitle string
	}{
		{"operator run", dash.Row{}, false, ""},
		{"spawned", dash.Row{Agent: true}, true, "opened by the machine"},
		{"groomed edge", dash.Row{EdgeAgent: true, EdgeConsent: "static"}, true,
			"chained here by a pulse groom (static ride)"},
		{"groomed edge, consent unrecorded", dash.Row{EdgeAgent: true}, true,
			"chained here by a pulse groom"},
		{"both", dash.Row{Agent: true, EdgeAgent: true, EdgeConsent: "dynamic"}, true,
			"opened by the machine"},
	}
	for _, tc := range cases {
		gotBadge, gotTitle := agentMark(tc.row)
		if gotBadge != tc.wantBadge || gotTitle != tc.wantTitle {
			t.Errorf("%s: agentMark = (%v, %q), want (%v, %q)",
				tc.name, gotBadge, gotTitle, tc.wantBadge, tc.wantTitle)
		}
	}
}

// TestDashBadgesAgentRows: the headline requirement — the operator can
// see at a glance what the agent added, on the board itself. The badge
// is independent of nesting: a spawned run whose spawner isn't on the
// board still wears it.
func TestDashBadgesAgentRows(t *testing.T) {
	now := time.Now().UTC()
	gather := func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			{Project: "p", Run: "spawned", Bucket: dash.BucketActiveRuns, When: now, Agent: true},
			{Project: "p", Run: "groomed", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Minute),
				Chained: true, EdgeAgent: true, EdgeConsent: "static"},
			{Project: "p", Run: "by-hand", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Hour)},
		}, 1, 1, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if got := strings.Count(body, `class="badge agent"`); got != 2 {
		t.Errorf("agent badges = %d, want 2 (the operator's own run stays unbadged)\n%s", got, body)
	}
	for _, want := range []string{
		`title="opened by the machine"`,
		`title="chained here by a pulse groom (static ride)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dash missing %q", want)
		}
	}
}

// TestRunPageRendersACaptureChain: the reported shape, end to end. A
// harvested idea's page draws the run its followup came from, and the
// capture hop wears both marks the walk hands over — the agent badge and
// a `none` consent chip. `none` is the one ride level that reads like an
// absence, and the template's `{{if .Consent}}` is all that stands
// between it and a hop that silently drops the level it was given: a
// bang cascade did the close, which is a different fact from an operator
// closing the run by hand, and only the chip tells them apart.
func TestRunPageRendersACaptureChain(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{Subject: "operator"},
		{
			Verb: "opened", Object: "alpha/design-only-spawn",
			ObjectURL: "/run/alpha/design-only-spawn",
		},
		{
			Verb: "promoted to", Object: "alpha/design-only-spawn-2026-09-01",
			ObjectURL: "/run/alpha/design-only-spawn-2026-09-01",
		},
		{Verb: "captured", Object: "this run", Agent: true, Consent: "none"},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	for _, want := range []string{
		`<h2>provenance</h2>`,
		`href="/run/alpha/design-only-spawn"`,
		`promoted to`,
		`captured`,
		`class="badge agent"`,
		`>none<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capture chain missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, "opened by operator") {
		t.Errorf("a harvested idea must not still credit the operator\nbody:\n%s", body)
	}
}

// TestRunPageNamesAPrunedCaptureSourceWithoutLinkingIt: the honesty rule
// on the arm the fix added. A harvesting run that has since been pruned
// is still named — the journal remembers it — but a link would claim a
// page that 404s.
func TestRunPageNamesAPrunedCaptureSourceWithoutLinkingIt(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{Subject: "alpha/pruned-harvester"},
		{Verb: "captured", Object: "this run", Agent: true, Consent: "none"},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	if !strings.Contains(body, `<span class="slug">alpha/pruned-harvester</span>`) {
		t.Errorf("the pruned harvester must be named, unlinked\nbody:\n%s", body)
	}
	if strings.Contains(body, `href="/run/alpha/pruned-harvester"`) {
		t.Errorf("a pruned harvester must carry no link\nbody:\n%s", body)
	}
}

// TestRunPageRendersAMachineWalkOpen: the one-line story the fix minted.
// It has no chain and no source, so nothing but the badge and the chip
// distinguishes it from the `opened by operator` line it replaced.
func TestRunPageRendersAMachineWalkOpen(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{Verb: "opened by a machine walk", Agent: true, Consent: "dynamic"},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	for _, want := range []string{"opened by a machine walk", `class="badge agent"`, `>dynamic<`} {
		if !strings.Contains(body, want) {
			t.Errorf("machine-walk open missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="arrow"`) {
		t.Errorf("no arrow on a one-line story\nbody:\n%s", body)
	}
}
