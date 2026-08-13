package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// The gate's `runs` entries have two spellings — an object that mints, a
// string that names a run already parked — and a tagged idea is a parked
// run, so a survey naming one reaches for the string. Every test here is
// about that spelling reaching the promotion seam the object one already
// had, and about the three shapes it must *not* change.

// seedTaggedIdea parks an in-progress idea carrying an sdlc workflow tag
// — what `moe idea tag` and a filer's `(sdlc)` stamp both write.
func seedTaggedIdea(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, projectID, run.Options{
		ID:        slug,
		Workflow:  "idea",
		PromoteTo: "sdlc",
		SeedDocs:  map[string]string{"idea": "# " + slug + "\n\nThe canvas that becomes the design seed.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// stringThread is one thread naming its members the way a survey
// naturally writes them: bare strings, no specs.
func stringThread(slugs ...string) pulseThread {
	entries := make([]pulseThreadEntry, 0, len(slugs))
	for _, s := range slugs {
		entries = append(entries, pulseThreadEntry{Existing: s})
	}
	return pulseThread{Runs: entries}
}

// TestGateStringEntryPromotesTaggedIdea is the defect. The survey wrote
// `"runs": ["cleanup-foo"]`, which the docs define as "any parked run in
// the project" and a tagged idea is one — and the entry went straight to
// the groom as a slug, where resolveMember admits only chainable runs and
// dropped it with "not a parked run in moe". The promotion seam has to
// get its turn before the slug does.
func TestGateStringEntryPromotesTaggedIdea(t *testing.T) {
	root := spawnFixture(t)
	idea := seedTaggedIdea(t, root, "moe", "cleanup-foo")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status:  "ok",
		Threads: []pulseThread{stringThread("cleanup-foo")},
	}, io.Discard, &errb)

	if len(groups) != 1 || len(groups[0].Runs) != 1 {
		t.Fatalf("groups = %+v, want one thread with one member; stderr=%q", groups, errb.String())
	}
	destID := groups[0].Runs[0].mintedID
	if destID == "" {
		t.Fatalf("member = %+v, want the promoted run's id, not a slug; stderr=%q", groups[0].Runs[0], errb.String())
	}
	dest, err := run.Load(root, "moe", destID)
	if err != nil {
		t.Fatal(err)
	}
	if dest.Workflow != "sdlc" || dest.SpawnedBy != "moe/pulse-one" {
		t.Errorf("destination = %+v, want an sdlc run spawned by the pulse", dest)
	}
	source, err := run.Load(root, "moe", idea.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != run.StatusPromoted {
		t.Errorf("idea status = %q, want promoted", source.Status)
	}

	// And it grooms: travelling as a minted id is what makes the rest of
	// the sweep byte-identical to the object-spec path that always worked.
	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, &errb)
	if len(groomed.threads) != 1 {
		t.Fatalf("groomed threads = %+v, want the promoted run placed; stderr=%q", groomed.threads, errb.String())
	}
	if got, want := groomed.threads[0].Root, "moe/"+destID; got != want {
		t.Errorf("thread root = %q, want %q", got, want)
	}
	if strings.Contains(errb.String(), "not a parked run") {
		t.Errorf("stderr = %q, want no drop — that message is the bug", errb.String())
	}
}

// TestGateStringEntryLeavesAnUntaggedIdeaAlone: untagged is the
// structural human-triage fence, and it holds from this grammar too. The
// entry drops — but it says why. "Not a parked run in moe" is actively
// misleading for an idea that *is* parked and merely unlicensed.
func TestGateStringEntryLeavesAnUntaggedIdeaAlone(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{
		ID: "needs-triage", Workflow: "idea", SeedDocs: map[string]string{"idea": "# Needs triage\n"},
	}); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status:  "ok",
		Threads: []pulseThread{stringThread("needs-triage")},
	}, io.Discard, &errb)

	if len(groups) != 1 || len(groups[0].Runs) != 1 || groups[0].Runs[0].slug != "needs-triage" {
		t.Fatalf("groups = %+v, want the entry left as a slug", groups)
	}
	if !strings.Contains(errb.String(), "requires operator triage") {
		t.Errorf("stderr = %q, want the untagged idea named as triage, not as missing", errb.String())
	}
	idea, err := run.Load(root, "moe", "needs-triage")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusInProgress {
		t.Errorf("idea status = %q, want it left parked", idea.Status)
	}
	if len(runsWithWorkflow(t, root, "moe", "sdlc")) != 0 {
		t.Error("an sdlc run was opened for an untagged idea")
	}
}

// TestGateStringEntryStillResolvesAnOrdinaryParkedRun is the regression
// pin, and the load-bearing half of the fix: a string naming a real
// parked sdlc run must keep travelling as a slug for the groom to
// resolve against disk. Minting it here instead would be a second run.
func TestGateStringEntryStillResolvesAnOrdinaryParkedRun(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status:  "ok",
		Threads: []pulseThread{stringThread(minted["fix-a"])},
	}, io.Discard, &errb)

	if len(groups) != 1 || len(groups[0].Runs) != 1 {
		t.Fatalf("groups = %+v, want one thread with one member", groups)
	}
	if got := groups[0].Runs[0]; got.mintedID != "" || got.slug != minted["fix-a"] {
		t.Fatalf("member = %+v, want the slug untouched", got)
	}
	if len(runsWithWorkflow(t, root, "moe", "sdlc")) != 1 {
		t.Error("the gate opened a second run for a slug that already names one")
	}

	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, &errb)
	if len(groomed.threads) != 1 || groomed.threads[0].Root != "moe/"+minted["fix-a"] {
		t.Errorf("groomed threads = %+v, want the existing run placed; stderr=%q", groomed.threads, errb.String())
	}
}

// TestGateStringEntriesPromoteEveryTaggedIdea is 2026-08-13 replayed:
// three tagged ideas, three single-run threads, every entry a string.
// The survey saw all three and nominated all three; the harness landed
// none of them, and two sat parked until a human intervened.
func TestGateStringEntriesPromoteEveryTaggedIdea(t *testing.T) {
	root := spawnFixture(t)
	slugs := []string{"pending-marker-comment-claims-untracked-wedge", "rebase-failure-doubled-prefix", "not-picking-up-tagged-ideas"}
	for _, slug := range slugs {
		seedTaggedIdea(t, root, "moe", slug)
	}

	var errb bytes.Buffer
	threads := make([]pulseThread, 0, len(slugs))
	for _, slug := range slugs {
		threads = append(threads, stringThread(slug))
	}
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{Status: "ok", Threads: threads}, io.Discard, &errb)

	if len(groups) != len(slugs) {
		t.Fatalf("groups = %+v, want one per thread", groups)
	}
	for i, grp := range groups {
		if len(grp.Runs) != 1 || grp.Runs[0].mintedID == "" {
			t.Fatalf("thread %d (%s) = %+v, want a promoted run; stderr=%q", i, slugs[i], grp.Runs, errb.String())
		}
	}
	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != len(slugs) {
		t.Fatalf("sdlc runs = %v, want one per tagged idea", got)
	}
	if got := runsWithWorkflow(t, root, "moe", "idea"); len(got) != 0 {
		t.Errorf("ideas still parked = %v, want all three promoted", got)
	}

	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, &errb)
	if len(groomed.threads) != len(slugs) {
		t.Errorf("groomed threads = %+v, want three; stderr=%q", groomed.threads, errb.String())
	}
}

// TestGateStringEntryTwicePromotesOnce: a survey may name the same
// parked idea at two positions. The first promotes; the second finds the
// destination beside the now-promoted idea, which is not a lone idea
// match, so it stays a slug — and the groom's dated-form lookup places
// that same destination rather than minting a sibling.
func TestGateStringEntryTwicePromotesOnce(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "cleanup-foo")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status:  "ok",
		Threads: []pulseThread{stringThread("cleanup-foo", "cleanup-foo")},
	}, io.Discard, &errb)

	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != 1 {
		t.Fatalf("sdlc runs = %v, want exactly one — the second position is the same work", got)
	}
	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, &errb)
	if len(groomed.threads) != 1 {
		t.Fatalf("groomed threads = %+v, want one; stderr=%q", groomed.threads, errb.String())
	}
	if got, want := groomed.threads[0].Root, "moe/"+groups[0].Runs[0].mintedID; got != want {
		t.Errorf("thread root = %q, want the promoted run %q", got, want)
	}
	if strings.Contains(errb.String(), "not a parked run") {
		t.Errorf("stderr = %q, want the second position resolved, not dropped", errb.String())
	}
}
