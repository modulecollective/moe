package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// twinSpawnFixture stands up a project whose twin wiki builds, so
// mintReflectRun can actually mint — the seam a `workflow: "twin"` spawn
// entry dispatches into.
func twinSpawnFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	return root
}

// TestSpawnTwinMintsAHarnessNamedReflect: a twin spec's slug is not the
// run's name — the reflect's real slug stays harness-minted
// (reflect-YYYY-MM-DD). Nothing has to name it: a twin spec is written
// where the reflect goes, so the slug is only a handle for the warnings.
func TestSpawnTwinMintsAHarnessNamedReflect(t *testing.T) {
	root := twinSpawnFixture(t)

	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "bring-the-twin-current", Workflow: "twin", Why: "boundary move the twin docs miss"},
	}, io.Discard, os.Stderr)

	id, ok := minted["bring-the-twin-current"]
	if !ok {
		t.Fatalf("minted = %v, want the spec to resolve to a reflect", minted)
	}
	if !strings.HasPrefix(id, "reflect") {
		t.Errorf("minted id %q, want the harness-minted reflect slug, not the proposed one", id)
	}
	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want exactly one reflect", tw)
	}
	if _, ok := tw[id]; !ok {
		t.Errorf("twin runs = %v, want the id the spec resolved to (%s)", tw, id)
	}
}

// TestSpawnTwinWithoutASlugStillMints is the regression for the trap that
// killed pulse-2026-07-21-15: the gate's one kicked thread held
// `{"workflow": "twin", "why": ...}` with no slug, slug validation ran
// ahead of the workflow dispatch, and the entry was skipped — so the
// thread resolved to zero members, the groom placed nothing, and the kick
// had nothing to start. The slug is documented as a meaningless handle on
// this path, so omitting it must not cost the reflect, the thread, or the
// generation behind it.
func TestSpawnTwinWithoutASlugStillMints(t *testing.T) {
	root := twinSpawnFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{
			Kick: true,
			Runs: []pulseThreadEntry{{Spec: &pulseRunSpec{Workflow: "twin", Why: "six observations stacked"}}},
		}},
	}, io.Discard, &errb)

	if strings.Contains(errb.String(), "unusable slug") {
		t.Errorf("stderr = %q, want no slug complaint on a twin entry", errb.String())
	}
	if len(groups) != 1 || len(groups[0].Runs) != 1 {
		t.Fatalf("groups = %+v, want one thread carrying the reflect", groups)
	}
	reflectID := groups[0].Runs[0].mintedID
	if !strings.HasPrefix(reflectID, "reflect") {
		t.Fatalf("thread member = %q, want the harness-minted reflect", reflectID)
	}

	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, &errb)

	if len(groomed.threads) != 1 || !groomed.threads[0].Kick {
		t.Fatalf("threads = %+v, want the thread groomed and asking for a kick", groomed.threads)
	}
	reflectKey := "moe/" + reflectID
	if groomed.threads[0].Root != reflectKey {
		t.Errorf("thread root = %q, want the reflect %q", groomed.threads[0].Root, reflectKey)
	}
	// A freshly minted reflect has no work-turns, so the spawn edge is
	// the only leg of pulseSelfKick's settled-design admit it can pass —
	// the machine baked this run's design, and that edge is what makes
	// the thread kickable at all.
	md, err := run.Load(root, "moe", reflectID)
	if err != nil {
		t.Fatal(err)
	}
	if md.SpawnedBy != "moe/pulse-one" {
		t.Errorf("reflect SpawnedBy = %q, want the minting pulse — without it a seed-only root is never kicked", md.SpawnedBy)
	}
}

// TestSpawnTwoTwinEntriesMintOne: two asks, one run — and both specs
// resolve to it. The first mints; the second finds that run in flight
// and *maps* onto it rather than resolving to nothing, so either thread
// position still orders the reflect.
func TestSpawnTwoTwinEntriesMintOne(t *testing.T) {
	root := twinSpawnFixture(t)

	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "twin-a", Workflow: "twin", Why: "first"},
		{Slug: "twin-b", Workflow: "twin", Why: "second"},
	}, io.Discard, io.Discard)

	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want exactly one — the second ask maps, it does not mint", tw)
	}
	if minted["twin-a"] == "" || minted["twin-b"] == "" {
		t.Fatalf("minted = %v, want both specs to resolve", minted)
	}
	if minted["twin-a"] != minted["twin-b"] {
		t.Errorf("minted = %v, want both specs on the one reflect", minted)
	}
	if _, ok := tw[minted["twin-b"]]; !ok {
		t.Errorf("twin runs = %v, want the mapped spec to name the open reflect (%s)", tw, minted["twin-b"])
	}
}

// TestSpawnTwinWarnsAndIgnoresTitleAndDesign: a due reflect is worth
// more than tidy stderr, so extra fields warn rather than reject —
// matching the tagged-idea path's "ignoring design body" posture.
func TestSpawnTwinWarnsAndIgnoresTitleAndDesign(t *testing.T) {
	root := twinSpawnFixture(t)

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift", Title: "Refresh the twin", Design: "# not a seed\n"},
	}, io.Discard, &errb)

	if _, ok := minted["twin-refresh"]; !ok {
		t.Fatalf("minted = %v, want the reflect minted anyway", minted)
	}
	for _, want := range []string{"ignoring title", "ignoring design body"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr = %q, want %q warned", errb.String(), want)
		}
	}
}

// TestSpawnUnsupportedWorkflowSkips: the allowlist is sdlc + twin. chat
// is perpetual and pulse would be recursion; nothing else has a use case
// yet, and widening is a decision, not a default.
func TestSpawnUnsupportedWorkflowSkips(t *testing.T) {
	root := twinSpawnFixture(t)

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "recurse", Workflow: "pulse", Why: "no"},
		{Slug: "chatty", Workflow: "chat", Why: "also no"},
	}, io.Discard, &errb)

	if len(minted) != 0 {
		t.Fatalf("minted = %v, want nothing off an unsupported workflow", minted)
	}
	if got := strings.Count(errb.String(), "only sdlc and twin are spawnable"); got != 2 {
		t.Errorf("stderr = %q, want both entries warned", errb.String())
	}
}

// TestSpawnTwinChainsAndKicksLikeAnyThread: the uniformity this run
// buys. A twin spec written at a thread position grooms the reflect into
// the thread and hands it back as an ordinary kick candidate — no
// reflect-specific placement, no reflect-specific consent path, and no
// alias for the thread to name it by — the spec is the position.
func TestSpawnTwinChainsAndKicksLikeAnyThread(t *testing.T) {
	root := twinSpawnFixture(t)

	defer withRideMode(rideDynamic)()
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{
			Kick: true,
			Runs: []pulseThreadEntry{
				{Spec: &pulseRunSpec{Slug: "fix-a", Title: "Fix A", Why: "red check"}},
				{Spec: &pulseRunSpec{Slug: "bring-the-twin-current", Workflow: "twin", Why: "boundary move"}},
			},
		}},
	}, io.Discard, os.Stderr)
	if len(groups) != 1 || len(groups[0].Runs) != 2 {
		t.Fatalf("groups = %+v, want one thread of two minted runs", groups)
	}
	fixKey := "moe/" + groups[0].Runs[0].mintedID
	reflectKey := "moe/" + groups[0].Runs[1].mintedID

	groomed := groomChains(root, "moe", "pulse-one", groups, "" /*spawner*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	if len(groomed.threads) != 1 || !groomed.threads[0].Kick {
		t.Fatalf("threads = %+v, want one kick candidate", groomed.threads)
	}
	if groomed.threads[0].Root != fixKey {
		t.Errorf("thread root = %q, want the thread's first run %q", groomed.threads[0].Root, fixKey)
	}
	if got := liveEdges(t, root)[fixKey]; got != reflectKey {
		t.Errorf("%s chains to %q, want the reflect %q — the agent's ordering claim, not a harness rule",
			fixKey, got, reflectKey)
	}
}

// TestSpawnTwinInLooseParks: the behavior change this run
// accepts. With placeReflect gone, a reflect the survey put in `loose`
// parks standalone and unridden, exactly like an sdlc spec there.
// Riding it is an ordering claim the agent has to make.
func TestSpawnTwinInLooseParks(t *testing.T) {
	root := twinSpawnFixture(t)

	defer withRideMode(rideDynamic)()
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift"},
	}, io.Discard, os.Stderr)
	groomed := groomChains(root, "moe", "pulse-one", nil /*no groups*/, "", nil /*kickoff edges*/, io.Discard, os.Stderr)

	if len(groomed.threads) != 0 {
		t.Fatalf("threads = %+v, want none — an unchained reflect is not a kick candidate", groomed.threads)
	}
	reflectKey := "moe/" + minted["twin-refresh"]
	for parent, child := range liveEdges(t, root) {
		if child == reflectKey || parent == reflectKey {
			t.Fatalf("reflect has a live edge (%s → %s), want it parked standalone", parent, child)
		}
	}
}

// TestSpawnTwinMapsOntoOpenReflectAndChainsIt: with a reflect already
// parked the nomination maps onto it instead of resolving to nothing, so
// the thread orders the open reflect in position rather than silently
// shedding the member. A twin spec sits mid-thread here, which is the
// whole point of the positional grammar — the reflect never needed a
// name for the ordering to reach it.
func TestSpawnTwinMapsOntoOpenReflectAndChainsIt(t *testing.T) {
	root := twinSpawnFixture(t)
	// A reflect already parked — the nomination maps onto this run.
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			{Spec: &pulseRunSpec{Slug: "fix-a", Title: "Fix A", Why: "red check"}},
			{Spec: &pulseRunSpec{Slug: "twin-refresh", Workflow: "twin", Why: "drift"}},
			{Spec: &pulseRunSpec{Slug: "fix-b", Title: "Fix B", Why: "stale doc"}},
		}}},
	}, io.Discard, io.Discard)

	if len(groups) != 1 || len(groups[0].Runs) != 3 {
		t.Fatalf("groups = %+v, want one thread of three", groups)
	}
	if got := groups[0].Runs[1].mintedID; got != "reflect-2026-05-14" {
		t.Fatalf("mid-thread twin spec resolved to %q, want the open reflect", got)
	}
	if tw := twinRuns(t, root, "moe"); len(tw) != 1 {
		t.Fatalf("twin runs = %v, want no second reflect minted", tw)
	}

	groomChains(root, "moe", "pulse-one", groups, "", nil /*kickoff edges*/, io.Discard, &errb)

	if strings.Contains(errb.String(), "which is not a parked run") {
		t.Errorf("stderr = %q, want no dropped member", errb.String())
	}
	// The reflect sits in the position the thread gave it, mid-thread.
	edges := liveEdges(t, root)
	if got := edges["moe/"+groups[0].Runs[0].mintedID]; got != "moe/reflect-2026-05-14" {
		t.Errorf("fix-a chains to %q, want the mapped reflect", got)
	}
	if got := edges["moe/reflect-2026-05-14"]; got != "moe/"+groups[0].Runs[2].mintedID {
		t.Errorf("reflect chains to %q, want fix-b", got)
	}
}

// TestApplyGateSkipsADuplicateSlugAcrossSpecs: the dedupe that used to
// be a side effect of the alias map's last-write-wins is now the
// minter's own live-slug claim. Two specs proposing one slug in a single
// gate mint one run; the second warns and leaves a hole in its thread
// rather than minting a dated sibling.
func TestApplyGateSkipsADuplicateSlugAcrossSpecs(t *testing.T) {
	root := twinSpawnFixture(t)

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Loose:  []pulseRunSpec{{Slug: "fix-a", Title: "Fix A", Why: "red check"}},
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			{Spec: &pulseRunSpec{Slug: "fix-a", Title: "Fix A again", Why: "same thing"}},
			{Spec: &pulseRunSpec{Slug: "fix-b", Title: "Fix B", Why: "stale doc"}},
		}}},
	}, io.Discard, &errb)

	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != 2 {
		t.Fatalf("sdlc runs = %v, want exactly fix-a and fix-b", got)
	}
	if !strings.Contains(errb.String(), `already has a live run for "fix-a"`) {
		t.Errorf("stderr = %q, want the duplicate slug warned", errb.String())
	}
	if len(groups) != 1 || len(groups[0].Runs) != 1 {
		t.Fatalf("groups = %+v, want the thread carrying only fix-b", groups)
	}
	if got := groups[0].Runs[0].mintedID; !strings.HasPrefix(got, "fix-b") {
		t.Errorf("thread member = %q, want fix-b", got)
	}
}
