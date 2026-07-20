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
	return newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		RunProvenance: func(string, string) ([]ProvHop, error) { return hops, gatherErr },
	})
}

// TestRunPageRendersProvenance: the origin story reaches the page —
// spawner linked, `agent` badge on the machine hop, ride level, and the
// recorded reason.
func TestRunPageRendersProvenance(t *testing.T) {
	s := provenanceServer(t, []ProvHop{
		{
			Verb: "opened by", Object: "alpha/pulse-2026-07-20",
			ObjectURL: "/run/alpha/pulse-2026-07-20",
			Agent:     true, Consent: "dynamic",
			Why: "reflect flagged due",
		},
		{
			Subject: "alpha/pulse-2026-07-20", SubjectURL: "/run/alpha/pulse-2026-07-20",
			Verb: "opened by operator",
		},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/fix-ci")

	for _, want := range []string{
		`<h2>provenance</h2>`,
		`opened by`,
		`href="/run/alpha/pulse-2026-07-20"`,
		`class="badge agent"`,
		`>dynamic<`,
		`why: reflect flagged due`,
		`opened by operator`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q\nbody:\n%s", want, body)
		}
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
