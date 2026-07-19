package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// groomFixture mints n parked sdlc runs named fix-1..fix-n and returns
// the slug→id map grooming resolves against. Deliberately mints through
// the real spawn step, so the dating-on-collision path the resolver has
// to see through is exercised rather than simulated.
func groomFixture(t *testing.T, root string, slugs ...string) map[string]string {
	t.Helper()
	spawns := make([]pulseSpawn, 0, len(slugs))
	for _, s := range slugs {
		spawns = append(spawns, pulseSpawn{Slug: s, Title: s})
	}
	minted := maybeSpawnFixRuns(root, "moe", "pulse-groom", spawns, io.Discard, os.Stderr)
	if len(minted) != len(slugs) {
		t.Fatalf("minted %v, want all of %v", minted, slugs)
	}
	return minted
}

// liveEdges reads the current effective chain edges as a parent→child
// map, the way every edge reader does.
func liveEdges(t *testing.T, root string) map[string]string {
	t.Helper()
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	out := map[string]string{}
	for parent, child := range idx.ChainedChild {
		if child != "" && run.ChainChildLive(child, byKey) {
			out[parent] = child
		}
	}
	return out
}

// TestGroomSelfRootsAHeadlessThread: a group with no `onto` and no
// `head`, from an unchained spawner, parks as a bare chain of ordinary
// runs. No placeholder is minted — headless is the default shape.
func TestGroomSelfRootsAHeadlessThread(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")

	threads := groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b", "fix-c"}}},
		minted, "" /*spawner*/, io.Discard, os.Stderr)

	if got := runsWithWorkflow(t, root, "moe", chainWorkflow); len(got) != 0 {
		t.Fatalf("chain heads %v, want none — a self-rooted thread is headless", got)
	}
	edges := liveEdges(t, root)
	a, b, c := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"], "moe/"+minted["fix-c"]
	if edges[a] != b || edges[b] != c {
		t.Fatalf("edges = %v, want %s -> %s -> %s", edges, a, b, c)
	}
	if edges[c] != "" {
		t.Errorf("tail %s chains to %q, want nothing", c, edges[c])
	}
	if len(threads) != 1 || threads[0].Root != a {
		t.Fatalf("threads = %+v, want one rooted at %s", threads, a)
	}
}

// TestGroomOntoAppendsAtATail: `onto` naming the last member of an
// existing thread appends behind it.
func TestGroomOntoAppendsAtATail(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")
	a, b, c := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"], "moe/"+minted["fix-c"]

	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b"}}}, minted, "", io.Discard, os.Stderr)
	groomChains(root, "moe", "pulse-groom-2",
		[]pulseChainGroup{{Onto: "fix-b", Runs: []string{"fix-c"}}}, minted, "", io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges[a] != b || edges[b] != c || edges[c] != "" {
		t.Fatalf("edges = %v, want %s -> %s -> %s and nothing after", edges, a, b, c)
	}
}

// TestGroomOntoSplicesMidChain: `onto` naming a mid-chain member
// inserts between it and its child rather than appending at the end.
// Mid-ride this is the queue jump — work attached after an
// already-merged member runs before the hop that was next.
func TestGroomOntoSplicesMidChain(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")
	a, b, c := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"], "moe/"+minted["fix-c"]

	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b"}}}, minted, "", io.Discard, os.Stderr)
	groomChains(root, "moe", "pulse-groom-2",
		[]pulseChainGroup{{Onto: "fix-a", Runs: []string{"fix-c"}}}, minted, "", io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges[a] != c || edges[c] != b {
		t.Fatalf("edges = %v, want %s -> %s -> %s (spliced)", edges, a, c, b)
	}
	if edges[b] != "" {
		t.Errorf("tail %s chains to %q, want nothing", b, edges[b])
	}
}

// TestGroomMoveRestitchesTheOldUnit: naming a run that is already
// chained elsewhere moves it, and the thread it left closes over the
// gap. This is the no-source-filter decision's mechanical half — an
// operator-minted head is not special here.
func TestGroomMoveRestitchesTheOldUnit(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c", "fix-d")
	a := "moe/" + minted["fix-a"]
	b := "moe/" + minted["fix-b"]
	c := "moe/" + minted["fix-c"]
	d := "moe/" + minted["fix-d"]

	// Two threads: a → b → c, and d alone.
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b", "fix-c"}}}, minted, "", io.Discard, os.Stderr)

	// Steal the middle member onto d.
	groomChains(root, "moe", "pulse-groom-2",
		[]pulseChainGroup{{Onto: "fix-d", Runs: []string{"fix-b"}}}, minted, "", io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges[d] != b {
		t.Fatalf("edges = %v, want %s -> %s after the move", edges, d, b)
	}
	if edges[a] != c {
		t.Fatalf("edges = %v, want the old unit restitched %s -> %s", edges, a, c)
	}
	if edges[b] != "" {
		t.Errorf("moved run %s still chains to %q, want it a tail", b, edges[b])
	}
}

// TestGroomExplicitHeadMintsAndCarriesProvenance: a `head` group opens
// a placeholder, chains the group under it, and seeds the purpose note
// with the survey that spawned it. Provenance goes on machine-minted
// heads and nowhere else.
func TestGroomExplicitHeadMintsAndCarriesProvenance(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")

	threads := groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Head: "perf-cleanups", Runs: []string{"fix-a", "fix-b"}}},
		minted, "", io.Discard, os.Stderr)

	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain heads %v, want the one the group named", heads)
	}
	if !strings.HasPrefix(heads[0], "perf-cleanups") {
		t.Errorf("head slug = %q, want it derived from the group's name", heads[0])
	}
	headKey := "moe/" + heads[0]
	edges := liveEdges(t, root)
	if edges[headKey] != "moe/"+minted["fix-a"] {
		t.Fatalf("edges = %v, want the head chained to the first member", edges)
	}
	if len(threads) != 1 || threads[0].Root != headKey {
		t.Fatalf("threads = %+v, want one rooted at the minted head", threads)
	}
	canvas, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", heads[0], chainDoc)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canvas), "pulse-groom") {
		t.Errorf("head canvas missing its provenance line:\n%s", canvas)
	}
}

// TestGroomOntoUnknownWarnsAndSkips: an `onto` that resolves to nothing
// drops the group with a stderr line, matching the spawn path's
// warn-only ethos. The group's runs stay where they were.
func TestGroomOntoUnknownWarnsAndSkips(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a")

	var errb bytes.Buffer
	threads := groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "no-such-run", Runs: []string{"fix-a"}}},
		minted, "", io.Discard, &errb)

	if len(threads) != 0 {
		t.Fatalf("threads = %+v, want the group skipped", threads)
	}
	if len(liveEdges(t, root)) != 0 {
		t.Fatalf("edges = %v, want none stamped", liveEdges(t, root))
	}
	if !strings.Contains(errb.String(), "names no run") {
		t.Errorf("stderr = %q, want the skip named", errb.String())
	}
}

// TestGroomRejectsOntoAndHeadTogether: two different answers to the
// same question — skip rather than pick one.
func TestGroomRejectsOntoAndHeadTogether(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")

	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "fix-a", Head: "topic", Runs: []string{"fix-b"}}},
		minted, "", io.Discard, &errb)

	if got := runsWithWorkflow(t, root, "moe", chainWorkflow); len(got) != 0 {
		t.Errorf("chain heads %v, want none — the group was skipped", got)
	}
	if len(liveEdges(t, root)) != 0 {
		t.Errorf("edges = %v, want none", liveEdges(t, root))
	}
	if !strings.Contains(errb.String(), "both `onto` and `head`") {
		t.Errorf("stderr = %q, want the conflict named", errb.String())
	}
}

// TestGroomOpportunisticNeedsDynamic covers both halves of the
// opportunistic placement rule: with the fourth bang the group lands on
// the spawner's unit tail, and without it the very same group
// self-roots instead. This is what keeps `!!!`'s contract — the machine
// cannot grow the ride — while letting `!!!!` extend it.
func TestGroomOpportunisticNeedsDynamic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    rideMode
		wantEnd string // "tail" (appended to the spawner's unit) or "root"
	}{
		{name: "static", mode: rideStatic, wantEnd: "root"},
		{name: "none", mode: rideNone, wantEnd: "root"},
		{name: "dynamic", mode: rideDynamic, wantEnd: "tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := spawnFixture(t)
			minted := groomFixture(t, root, "spawner", "member", "fix-new")
			spawnerKey := "moe/" + minted["spawner"]
			memberKey := "moe/" + minted["member"]
			newKey := "moe/" + minted["fix-new"]

			// The spawner heads a two-run thread — it is a chain member,
			// so there is a unit tail to extend.
			groomChains(root, "moe", "pulse-setup",
				[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

			defer withRideMode(tc.mode)()
			groomChains(root, "moe", "pulse-groom",
				[]pulseChainGroup{{Runs: []string{"fix-new"}}}, minted, spawnerKey, io.Discard, os.Stderr)

			edges := liveEdges(t, root)
			if tc.wantEnd == "tail" {
				if edges[memberKey] != newKey {
					t.Fatalf("edges = %v, want %s appended after the unit tail %s", edges, newKey, memberKey)
				}
				return
			}
			if edges[memberKey] != "" {
				t.Fatalf("edges = %v, want the ridden unit untouched (%s is still its tail)", edges, memberKey)
			}
			for parent, child := range edges {
				if child == newKey {
					t.Fatalf("%s was chained under %s, want it self-rooted", newKey, parent)
				}
			}
		})
	}
}

// TestGroomStaticRideRedirectsIntoTheRiddenUnit: under `!!!`, even an
// explicit `onto` aimed inside the unit being ridden is redirected to a
// self-rooted thread. The work is still teed up; it just doesn't join
// this ride. That is the safety property the old fresh-head-per-batch
// rule used to buy, now scoped to the one case that needs it.
func TestGroomStaticRideRedirectsIntoTheRiddenUnit(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "fix-new")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	newKey := "moe/" + minted["fix-new"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideStatic)()
	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "member", Runs: []string{"fix-new"}}}, minted, spawnerKey, io.Discard, &errb)

	edges := liveEdges(t, root)
	if edges[memberKey] == newKey {
		t.Fatalf("edges = %v, want the static ride's unit closed to grooming", edges)
	}
	if edges[spawnerKey] != memberKey {
		t.Fatalf("edges = %v, want the ridden unit intact", edges)
	}
	if !strings.Contains(errb.String(), "static ride") {
		t.Errorf("stderr = %q, want the redirect named", errb.String())
	}
}

// TestGroomResolvesADatedSlug: the survey names the slug it proposed;
// the harness may have minted a dated sibling. The resolver has to see
// through that or every collision would silently drop its group.
func TestGroomResolvesADatedSlug(t *testing.T) {
	root := spawnFixture(t)

	// Mint fix-a, close it out of the live set, then mint fix-a again —
	// the second one gets a dated slug.
	first := maybeSpawnFixRuns(root, "moe", "pulse-one",
		[]pulseSpawn{{Slug: "fix-a", Title: "A"}}, io.Discard, os.Stderr)
	setRunStatus(t, root, "moe", first["fix-a"], run.StatusMerged)
	minted := maybeSpawnFixRuns(root, "moe", "pulse-two",
		[]pulseSpawn{{Slug: "fix-a", Title: "A again"}, {Slug: "fix-b", Title: "B"}}, io.Discard, os.Stderr)
	if minted["fix-a"] == "fix-a" {
		t.Fatalf("second mint reused the bare slug %q — fixture assumption broken", minted["fix-a"])
	}

	// Groom with an empty `minted` map, so resolution has to go through
	// the on-disk lookup rather than this batch's own mints.
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b"}}}, nil, "", io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges["moe/"+minted["fix-a"]] != "moe/"+minted["fix-b"] {
		t.Fatalf("edges = %v, want the dated %s chained to %s", edges, minted["fix-a"], minted["fix-b"])
	}
}
