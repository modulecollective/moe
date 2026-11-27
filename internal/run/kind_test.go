package run

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestReadersClassifyByKindOnly holds the rule that moe reads a commit
// through its trailers and nothing else. Every commit the readers should
// see carries a junk headline and its MoE-Kind; every commit they should
// not see carries a real headline — current or legacy — and no kind.
func TestReadersClassifyByKindOnly(t *testing.T) {
	root := newTestRoot(t)
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	i := 0
	commit := func(subject, trailers string) string {
		t.Helper()
		i++
		stamp := t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		gittest.RunWithEnv(t, root, []string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp},
			"commit", "--allow-empty", "-m", subject+"\n\nMoE-Run: loop\nMoE-Project: moe\n"+trailers)
		return gittest.HeadSHA(t, root)
	}
	doc := func(d string) string { return "MoE-Workflow: sdlc\nMoE-Document: " + d + "\n" }

	commit("x", "MoE-Kind: open\nMoE-Consent: dynamic\n")
	design := commit("x", doc("design")+"MoE-Kind: turn\n")
	advance := commit("x", doc("design")+"MoE-Kind: advance\n")
	build := commit("x", doc("build")+"MoE-Kind: turn\n")
	// Kind-less commits whose headlines every old matcher accepted.
	commit("Open run moe/loop", "MoE-Consent: static\n")
	commit("Update review for moe/loop", doc("review"))
	commit("work: update review", doc("review"))
	commit("Advance review for moe/loop", doc("review"))
	commit("advance: build", doc("build"))
	// A session start: same keys as a turn, no kind.
	commit("Start build for moe/loop", doc("build")+"MoE-Session: s\n")

	for _, tc := range []struct {
		name, doc, want string
		latest          func(root, projectID, runID, docID string) (string, time.Time, error)
	}{
		{"turn", "design", design, LatestWorkTurnSHA},
		{"turn, newest", "build", build, LatestWorkTurnSHA},
		{"turn, headline only", "review", "", LatestWorkTurnSHA},
		{"advance", "design", advance, LatestAdvanceSHA},
		{"advance, headline only", "review", "", LatestAdvanceSHA},
		{"advance, legacy headline only", "build", "", LatestAdvanceSHA},
	} {
		got, _, err := tc.latest(root, "moe", "loop", tc.doc)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	docs, err := WorkTurnDocs(root, "moe", "loop")
	if err != nil || strings.Join(docs, ",") != "build,design" {
		t.Errorf("WorkTurnDocs = %v, %v; want [build design]", docs, err)
	}

	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []WorkTurnKey{{"moe", "loop", "design"}, {"moe", "loop", "build"}} {
		if idx.WorkTurnTime[k].IsZero() {
			t.Errorf("WorkTurnTime[%v] missing", k)
		}
	}
	if _, ok := idx.WorkTurnTime[WorkTurnKey{"moe", "loop", "review"}]; ok {
		t.Errorf("a kind-less headline registered as a work turn")
	}
	if idx.AdvanceTime[WorkTurnKey{"moe", "loop", "design"}].IsZero() {
		t.Errorf("AdvanceTime[design] missing")
	}
	for _, d := range []string{"review", "build"} {
		if _, ok := idx.AdvanceTime[WorkTurnKey{"moe", "loop", d}]; ok {
			t.Errorf("a kind-less headline registered as an advance of %s", d)
		}
	}
	// The kind-less "Open run" is newer and carries consent; only the
	// kinded open counts.
	if got := idx.OpenConsent["moe/loop"]; got != "dynamic" {
		t.Errorf("OpenConsent = %q, want dynamic from the kinded open", got)
	}
}

// TestKindlessOpenHasNoOpenConsent is the other half: history written
// before MoE-Kind shows no machine-open, whatever its headline says.
func TestKindlessOpenHasNoOpenConsent(t *testing.T) {
	root := newTestRoot(t)
	gittest.Commit(t, root, "Open run moe/old\n\nMoE-Run: old\nMoE-Project: moe\nMoE-Consent: dynamic\n")
	idx, err := BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := idx.OpenConsent["moe/old"]; ok {
		t.Errorf("OpenConsent = %q from a kind-less open", got)
	}
}
