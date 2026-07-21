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
	minted := maybeSpawnRuns(root, "moe", "pulse-groom", spawns, io.Discard, os.Stderr)
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

	groomed := groomChains(root, "moe", "pulse-groom",
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
	if len(groomed.threads) != 1 || groomed.threads[0].Root != a {
		t.Fatalf("threads = %+v, want one rooted at %s", groomed.threads, a)
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

	groomed := groomChains(root, "moe", "pulse-groom",
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
	if len(groomed.threads) != 1 || groomed.threads[0].Root != headKey {
		t.Fatalf("threads = %+v, want one rooted at the minted head", groomed.threads)
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
	groomed := groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "no-such-run", Runs: []string{"fix-a"}}},
		minted, "", io.Discard, &errb)

	if len(groomed.threads) != 0 {
		t.Fatalf("threads = %+v, want the group skipped", groomed.threads)
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

// TestGroomStaticRideFencesAMoveOut: the other direction of the same
// contract. A group that names a still-parked member of the ridden unit
// in `runs` would detach it and re-chain it elsewhere — shrinking the
// ride the operator kicked, so the entry is dropped instead.
func TestGroomStaticRideFencesAMoveOut(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "elsewhere")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	elsewhereKey := "moe/" + minted["elsewhere"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideStatic)()
	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "elsewhere", Runs: []string{"member"}}}, minted, spawnerKey, io.Discard, &errb)

	edges := liveEdges(t, root)
	if edges[spawnerKey] != memberKey {
		t.Fatalf("edges = %v, want the ridden unit intact (%s -> %s)", edges, spawnerKey, memberKey)
	}
	if edges[elsewhereKey] == memberKey {
		t.Fatalf("edges = %v, want %s not moved out of the ride", edges, memberKey)
	}
	if !strings.Contains(errb.String(), "static ride") {
		t.Errorf("stderr = %q, want the dropped entry named", errb.String())
	}
}

// TestGroomStaticRideRedirectDoesNotYankMembers: a group whose `onto`
// and whose `runs` both sit inside the ridden unit is a reorder of the
// ride. The anchor-side redirect alone would self-root the group, which
// detaches the members and shrinks the ride — exactly what the fence
// exists to prevent. With the members fenced too, the group resolves to
// nothing and dies before any edge is stamped.
func TestGroomStaticRideRedirectDoesNotYankMembers(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "member2")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	member2Key := "moe/" + minted["member2"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member", "member2"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideStatic)()
	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "member", Runs: []string{"member2"}}}, minted, spawnerKey, io.Discard, &errb)

	edges := liveEdges(t, root)
	if edges[spawnerKey] != memberKey || edges[memberKey] != member2Key {
		t.Fatalf("edges = %v, want the ridden unit fully intact (%s -> %s -> %s)",
			edges, spawnerKey, memberKey, member2Key)
	}
}

// TestGroomStaticRideDropsOnlyTheRiddenEntry: the fence is per-entry,
// matching the sweep's warn-and-continue grain for member problems. A
// mixed group still grooms its groomable runs.
func TestGroomStaticRideDropsOnlyTheRiddenEntry(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "elsewhere", "fix-new")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	elsewhereKey := "moe/" + minted["elsewhere"]
	newKey := "moe/" + minted["fix-new"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideStatic)()
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "elsewhere", Runs: []string{"member", "fix-new"}}},
		minted, spawnerKey, io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges[elsewhereKey] != newKey {
		t.Fatalf("edges = %v, want the groomable %s placed onto %s", edges, newKey, elsewhereKey)
	}
	if edges[spawnerKey] != memberKey {
		t.Fatalf("edges = %v, want %s still in the ride", edges, memberKey)
	}
}

// TestGroomDynamicRideAllowsAMoveOut: the consent notch. `!!!!` licenses
// the machine to reshape the ride, so move-out stays legal there — this
// guards the new fence against over-firing.
func TestGroomDynamicRideAllowsAMoveOut(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "elsewhere")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	elsewhereKey := "moe/" + minted["elsewhere"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: "elsewhere", Runs: []string{"member"}}}, minted, spawnerKey, io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges[elsewhereKey] != memberKey {
		t.Fatalf("edges = %v, want %s moved onto %s under a dynamic ride", edges, memberKey, elsewhereKey)
	}
	if edges[spawnerKey] == memberKey {
		t.Fatalf("edges = %v, want %s detached from the old unit", edges, memberKey)
	}
}

// TestGroomResolvesADatedSlug: the survey names the slug it proposed;
// the harness may have minted a dated sibling. The resolver has to see
// through that or every collision would silently drop its group.
func TestGroomResolvesADatedSlug(t *testing.T) {
	root := spawnFixture(t)

	// Mint fix-a, close it out of the live set, then mint fix-a again —
	// the second one gets a dated slug.
	first := maybeSpawnRuns(root, "moe", "pulse-one",
		[]pulseSpawn{{Slug: "fix-a", Title: "A"}}, io.Discard, os.Stderr)
	setRunStatus(t, root, "moe", first["fix-a"], run.StatusMerged)
	minted := maybeSpawnRuns(root, "moe", "pulse-two",
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

// TestGroomOpportunisticNeverStampsASelfEdge: a dynamic ride whose group
// names the very run the opportunistic placement would attach after. The
// `onto` branch has always guarded the anchor against the group's own
// members; this branch did not, so the tail got chained to itself — a
// durable `X -> X` edge that every walker afterwards reads as a
// one-run cycle.
func TestGroomOpportunisticNeverStampsASelfEdge(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	a, b := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"]

	// A thread for the ride to be walking: a -> b, spawner a, tail b.
	groomChains(root, "moe", "pulse-one",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-two",
		[]pulseChainGroup{{Runs: []string{"fix-b"}}}, minted, a, io.Discard, &errb)

	for parent, child := range liveEdges(t, root) {
		if parent == child {
			t.Fatalf("stamped a self-edge on %s; edges = %v", parent, liveEdges(t, root))
		}
	}
	if !strings.Contains(errb.String(), "self-rooting instead") {
		t.Errorf("stderr = %q, want the redirect named", errb.String())
	}
	// Redirected, not dropped: b left the ride and self-roots.
	if edges := liveEdges(t, root); edges[a] == b {
		t.Errorf("edges = %v, want %s detached from the ride", edges, b)
	}
}

// TestGroomKickRootFollowsALaterGroupsMove: thread roots are derived from
// the final graph, not captured mid-walk. Group 1 self-roots a thread and
// asks for a kick; group 2 then moves that thread's first run under
// another anchor. The recorded root has to be the anchor's thread head —
// a root captured at group 1's time would name a run that no longer heads
// anything, and the kick would silently park.
func TestGroomKickRootFollowsALaterGroupsMove(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")
	a, c := "moe/"+minted["fix-a"], "moe/"+minted["fix-c"]

	groomed := groomChains(root, "moe", "pulse-groom", []pulseChainGroup{
		{Runs: []string{"fix-a", "fix-b"}, Kick: true},
		{Onto: "fix-c", Runs: []string{"fix-a"}},
	}, minted, "", io.Discard, os.Stderr)

	if len(groomed.threads) != 2 {
		t.Fatalf("threads = %+v, want two", groomed.threads)
	}
	if groomed.threads[0].Root == a {
		t.Fatalf("thread root is still %s, captured before group 2 moved it", a)
	}
	if groomed.threads[0].Root != c {
		t.Errorf("thread root = %q, want %q — the head of the thread fix-a ended up in",
			groomed.threads[0].Root, c)
	}
}

// TestGroomReportsSpawnerChainMembership: the re-entrancy answer the kick
// step keys on comes off the groom's own final graph rather than a second
// read of the journal. Post-groom membership is the pinned policy: a
// sweep that chains work onto its own spawner suppresses its other kicks.
func TestGroomReportsSpawnerChainMembership(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	a := "moe/" + minted["fix-a"]

	loose := groomChains(root, "moe", "pulse-one",
		[]pulseChainGroup{{Runs: []string{"fix-b"}}}, minted, a, io.Discard, os.Stderr)
	if loose.spawnerChained {
		t.Errorf("spawnerChained = true, want false — %s has no live edges either way", a)
	}

	joined := groomChains(root, "moe", "pulse-two",
		[]pulseChainGroup{{Onto: "fix-a", Runs: []string{"fix-b"}}}, minted, a, io.Discard, os.Stderr)
	if !joined.spawnerChained {
		t.Errorf("spawnerChained = false, want true — the sweep just chained work onto %s", a)
	}
}
