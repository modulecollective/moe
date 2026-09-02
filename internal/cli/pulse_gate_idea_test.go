package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
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
	groomed := groomChains(root, "moe", "pulse-one", groups, nil /*kickoff edges*/, io.Discard, &errb)
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

	groomed := groomChains(root, "moe", "pulse-one", groups, nil /*kickoff edges*/, io.Discard, &errb)
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

	groomed := groomChains(root, "moe", "pulse-one", groups, nil /*kickoff edges*/, io.Discard, &errb)
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
	groomed := groomChains(root, "moe", "pulse-one", groups, nil /*kickoff edges*/, io.Discard, &errb)
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

// seedDesignOnlyIdea parks a tagged idea whose licence is the narrower
// one: promote it, but ride one design turn and hold. Written the way
// TagIdea writes it — both fields, one claim.
func seedDesignOnlyIdea(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	seedTaggedIdea(t, root, projectID, slug)
	// Through the real writer, so the fixture can't drift from what the
	// operator's `moe idea tag --design-only` actually lands — and so it
	// leaves the tree clean, which the promote below requires.
	if err := runopen.TagIdea(root, projectID, slug, "sdlc", true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// TestPromoteCarriesTheDesignOnlyTag is the whole carry: the operator
// stamped the licence on the idea from their phone, and the run the
// pulse mints has to come out byte-identical in shape to a design-only
// spawn — SpawnedBy plus DesignOnly — because every mechanism
// downstream (the kick floor's hold, the one-stage ride, the stage
// prompt) reads only those two and knows nothing about where the run
// came from.
func TestPromoteCarriesTheDesignOnlyTag(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "worth-a-think", Why: "the operator tagged it"}}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the idea's licence carried onto the run")
	}
	if dest.SpawnedBy == "" {
		t.Errorf("spawned_by = %q, want the run to still mark itself machine-minted", dest.SpawnedBy)
	}
	if !strings.Contains(errb.String(), "to sdlc, design only run") {
		t.Errorf("stderr = %q, want the promote line to say the ride is short", errb.String())
	}
}

// TestPlainTaggedIdeaPromotesWithoutTheBit is the regression guard on
// the carry: the ordinary tag is still a licence to ship, so the run it
// mints must not hold at design.
func TestPlainTaggedIdeaPromotesWithoutTheBit(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "just-ship-it")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "just-ship-it", Why: "the operator tagged it"}}, io.Discard, &errb)
	dest, err := run.Load(root, "moe", minted["just-ship-it"])
	if err != nil {
		t.Fatal(err)
	}
	if dest.DesignOnly {
		t.Errorf("run.json design_only = true, want a plain tag to stay a licence to ship")
	}
}

// TestDesignOnlySpecOnADesignOnlyTaggedIdeaIsIgnored: the survey reads
// the open backlog, where a design-only tag now renders as "design
// only" — so it will echo the marker back as `design_only: true` on the
// spec. Skipping for that would make the operator's own licence
// unusable the moment the board is quoted, and the spec and the tag
// agree anyway. Warn and promote.
func TestDesignOnlySpecOnADesignOnlyTaggedIdeaIsIgnored(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	spec := designOnlySpec("worth-a-think")
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{spec}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "ignoring design_only for tagged idea moe/worth-a-think; the tag carries it") {
		t.Errorf("stderr = %q, want the redundant flag named and ignored", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the tag's bit on the run")
	}
	// The idea canvas is the seed, not the spec's brief — unchanged by
	// this run, and the reason the tag can mean "the seed is a brief".
	if !strings.Contains(errb.String(), "ignoring design body for tagged idea") {
		t.Errorf("stderr = %q, want the spec's design body still ignored", errb.String())
	}
}

// TestDesignOnlySpecWithNoBodyOnADesignOnlyTaggedIdeaPromotes is the
// same quote with the survey's other half followed: the guidance says
// omit `design` for a tagged idea, and the board says "design only", so
// the honest spec is a bare slug plus the marker. The no-body refusal
// exists because a fresh mint would have nothing but a title and a why
// for a seed — an idea's canvas *is* that brief, so there is nothing
// missing here to refuse over.
func TestDesignOnlySpecWithNoBodyOnADesignOnlyTaggedIdeaPromotes(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	spec := designOnlySpec("worth-a-think")
	spec.Design = ""
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{spec}, io.Discard, &errb)
	id := minted["worth-a-think"]
	if id == "" {
		t.Fatalf("nothing promoted; stderr=%q", errb.String())
	}
	if strings.Contains(errb.String(), "no design body") {
		t.Errorf("stderr = %q, want no no-body refusal — the idea canvas is the brief", errb.String())
	}
	if !strings.Contains(errb.String(), "ignoring design_only for tagged idea moe/worth-a-think; the tag carries it") {
		t.Errorf("stderr = %q, want the redundant flag named and ignored", errb.String())
	}
	dest, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the tag's bit on the run")
	}
}

// TestDesignOnlyTaggedIdeaSkippedAtAThreadPosition applies the spec
// rule to the tag that carries the bit: a design-only root is unsettled
// by definition until the operator advances it, so every member queued
// behind it would strand. The idea is left parked, and the next sweep
// may propose it loose — which is where design-only work belongs.
func TestDesignOnlyTaggedIdeaSkippedAtAThreadPosition(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")

	var errb bytes.Buffer
	applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status:  "ok",
		Threads: []pulseThread{stringThread("worth-a-think")},
	}, io.Discard, &errb)

	if !strings.Contains(errb.String(), "a design-only root strands the runs behind it; put it in loose") {
		t.Errorf("stderr = %q, want the spec rule's phrasing", errb.String())
	}
	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != 0 {
		t.Errorf("sdlc runs = %v, want the promote refused at this position", got)
	}
	md, err := run.Load(root, "moe", "worth-a-think")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Errorf("idea status = %q, want it left parked for the next sweep", md.Status)
	}
}
