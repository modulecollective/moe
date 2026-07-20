package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

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

// TestSpawnTwinMintsUnderTheAgentsAlias: a twin entry's slug is a
// batch-local alias, not the run's name. The reflect's real slug stays
// harness-minted (reflect-YYYY-MM-DD); the alias exists only so a chain
// group can name it.
func TestSpawnTwinMintsUnderTheAgentsAlias(t *testing.T) {
	root := twinSpawnFixture(t)

	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "bring-the-twin-current", Workflow: "twin", Why: "boundary move the twin docs miss"},
	}, io.Discard, os.Stderr)

	id, ok := minted["bring-the-twin-current"]
	if !ok {
		t.Fatalf("minted = %v, want the alias keyed to the reflect", minted)
	}
	if !strings.HasPrefix(id, "reflect") {
		t.Errorf("minted id %q, want the harness-minted reflect slug, not the alias", id)
	}
	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want exactly one reflect", tw)
	}
	if _, ok := tw[id]; !ok {
		t.Errorf("twin runs = %v, want the id the alias points at (%s)", tw, id)
	}
}

// TestSpawnTwoTwinEntriesMintOne: two asks, one run — but both aliases
// resolve. The first mints; the second finds that run in flight and
// *maps* onto it rather than resolving to nothing, so a chain group
// naming either alias still orders the reflect.
func TestSpawnTwoTwinEntriesMintOne(t *testing.T) {
	root := twinSpawnFixture(t)

	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "twin-a", Workflow: "twin", Why: "first"},
		{Slug: "twin-b", Workflow: "twin", Why: "second"},
	}, io.Discard, io.Discard)

	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want exactly one — the second ask maps, it does not mint", tw)
	}
	if minted["twin-a"] == "" || minted["twin-b"] == "" {
		t.Fatalf("minted = %v, want both aliases to resolve", minted)
	}
	if minted["twin-a"] != minted["twin-b"] {
		t.Errorf("minted = %v, want both aliases on the one reflect", minted)
	}
	if _, ok := tw[minted["twin-b"]]; !ok {
		t.Errorf("twin runs = %v, want the mapped alias to name the open reflect (%s)", tw, minted["twin-b"])
	}
}

// TestSpawnTwinWarnsAndIgnoresTitleAndDesign: a due reflect is worth
// more than tidy stderr, so extra fields warn rather than reject —
// matching the tagged-idea path's "ignoring design body" posture.
func TestSpawnTwinWarnsAndIgnoresTitleAndDesign(t *testing.T) {
	root := twinSpawnFixture(t)

	var errb bytes.Buffer
	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
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
	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
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
// buys. A group naming the twin alias grooms the reflect into the thread
// and hands it back as an ordinary kick candidate — no reflect-specific
// placement, no reflect-specific consent path.
func TestSpawnTwinChainsAndKicksLikeAnyThread(t *testing.T) {
	root := twinSpawnFixture(t)

	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-a", Title: "Fix A", Why: "red check"},
		{Slug: "bring-the-twin-current", Workflow: "twin", Why: "boundary move"},
	}, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	threads := groomChains(root, "moe", "pulse-one",
		[]pulseChainGroup{{Runs: []string{"fix-a", "bring-the-twin-current"}, Kick: true}},
		minted, "" /*spawner*/, io.Discard, os.Stderr)

	if len(threads) != 1 || !threads[0].Kick {
		t.Fatalf("threads = %+v, want one kick candidate", threads)
	}
	fixKey := "moe/" + minted["fix-a"]
	reflectKey := "moe/" + minted["bring-the-twin-current"]
	if threads[0].Root != fixKey {
		t.Errorf("thread root = %q, want the group's first run %q", threads[0].Root, fixKey)
	}
	if got := liveEdges(t, root)[fixKey]; got != reflectKey {
		t.Errorf("%s chains to %q, want the reflect %q — the agent's ordering claim, not a harness rule",
			fixKey, got, reflectKey)
	}
}

// TestSpawnTwinNamedInNoGroupParks: the behavior change this run
// accepts. With placeReflect gone, a reflect the survey didn't chain
// parks standalone and unridden, exactly like an sdlc spawn named in no
// group. Riding it is an ordering claim the agent has to make.
func TestSpawnTwinNamedInNoGroupParks(t *testing.T) {
	root := twinSpawnFixture(t)

	defer withRideMode(rideDynamic)()
	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift"},
	}, io.Discard, os.Stderr)
	threads := groomChains(root, "moe", "pulse-one", nil /*no groups*/, minted, "", io.Discard, os.Stderr)

	if len(threads) != 0 {
		t.Fatalf("threads = %+v, want none — an unchained reflect is not a kick candidate", threads)
	}
	reflectKey := "moe/" + minted["twin-refresh"]
	for parent, child := range liveEdges(t, root) {
		if child == reflectKey || parent == reflectKey {
			t.Fatalf("reflect has a live edge (%s → %s), want it parked standalone", parent, child)
		}
	}
}

// TestSpawnTwinMapsOntoOpenReflectAndChainsIt: the defect this run
// fixes. With a reflect already parked the nomination maps onto it
// instead of resolving to nothing, so the chain group orders the open
// reflect in position rather than silently shedding the member.
func TestSpawnTwinMapsOntoOpenReflectAndChainsIt(t *testing.T) {
	root := twinSpawnFixture(t)
	// A reflect already parked — the nomination maps onto this run.
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")

	minted := maybeSpawnRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-a", Title: "Fix A", Why: "red check"},
		{Slug: "fix-b", Title: "Fix B", Why: "stale doc"},
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift"},
	}, io.Discard, io.Discard)

	if got := minted["twin-refresh"]; got != "reflect-2026-05-14" {
		t.Fatalf("minted[twin-refresh] = %q, want the open reflect", got)
	}
	if tw := twinRuns(t, root, "moe"); len(tw) != 1 {
		t.Fatalf("twin runs = %v, want no second reflect minted", tw)
	}

	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-one",
		[]pulseChainGroup{{Runs: []string{"fix-a", "twin-refresh", "fix-b"}}},
		minted, "", io.Discard, &errb)

	if strings.Contains(errb.String(), "which is not a parked run") {
		t.Errorf("stderr = %q, want no dropped member", errb.String())
	}
	// The reflect sits in the position the group named, mid-thread.
	edges := liveEdges(t, root)
	if got := edges["moe/"+minted["fix-a"]]; got != "moe/reflect-2026-05-14" {
		t.Errorf("fix-a chains to %q, want the mapped reflect", got)
	}
	if got := edges["moe/reflect-2026-05-14"]; got != "moe/"+minted["fix-b"] {
		t.Errorf("reflect chains to %q, want fix-b", got)
	}
}
