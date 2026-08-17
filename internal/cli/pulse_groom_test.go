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

// mintSpecs opens one run per spec through the gate's minter and returns
// proposed-slug → minted-id.
//
// The harness keeps no such map — that was the alias namespace this run
// deleted, and a minted run now travels as its own id — but a test that
// asks for a run still needs to name what it got back, so the mapping
// lives here where a test's own bookkeeping belongs. Specs skipped by
// the minter are absent, which is what the dedupe assertions read.
func mintSpecs(root, projectID, pulseSlug string, specs []pulseRunSpec, stdout, stderr io.Writer) map[string]string {
	m := &pulseMinter{root: root, projectID: projectID, pulseSlug: pulseSlug}
	out := map[string]string{}
	for _, s := range specs {
		if id := m.mint(s, stdout, stderr); id != "" {
			out[strings.TrimSpace(s.Slug)] = id
		}
	}
	return out
}

// runsFrom builds a thread's members from bare slugs — the string form
// of a `runs` entry, resolved against disk at apply time.
func runsFrom(slugs ...string) []groomMember {
	out := make([]groomMember, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, groomMember{slug: s})
	}
	return out
}

// groomFixture mints n parked sdlc runs named fix-1..fix-n and returns
// the slug→id map grooming resolves against. Deliberately mints through
// the real spawn step, so the dating-on-collision path the resolver has
// to see through is exercised rather than simulated.
func groomFixture(t *testing.T, root string, slugs ...string) map[string]string {
	t.Helper()
	spawns := make([]pulseRunSpec, 0, len(slugs))
	for _, s := range slugs {
		spawns = append(spawns, pulseRunSpec{Slug: s, Title: s})
	}
	minted := mintSpecs(root, "moe", "pulse-groom", spawns, io.Discard, os.Stderr)
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
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b", "fix-c")}},
		nil /*kickoff edges*/, io.Discard, os.Stderr)

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
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
	groomChains(root, "moe", "pulse-groom-2",
		[]groomGroup{{Onto: "fix-b", Runs: runsFrom("fix-c")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

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
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
	groomChains(root, "moe", "pulse-groom-2",
		[]groomGroup{{Onto: "fix-a", Runs: runsFrom("fix-c")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

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
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b", "fix-c")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	// Steal the middle member onto d.
	groomChains(root, "moe", "pulse-groom-2",
		[]groomGroup{{Onto: "fix-d", Runs: runsFrom("fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

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
		[]groomGroup{{Head: "perf-cleanups", Runs: runsFrom("fix-a", "fix-b")}},
		nil /*kickoff edges*/, io.Discard, os.Stderr)

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
	groomFixture(t, root, "fix-a")

	var errb bytes.Buffer
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: "no-such-run", Runs: runsFrom("fix-a")}},
		nil /*kickoff edges*/, io.Discard, &errb)

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
	groomFixture(t, root, "fix-a", "fix-b")

	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: "fix-a", Head: "topic", Runs: runsFrom("fix-b")}},
		nil /*kickoff edges*/, io.Discard, &errb)

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

// TestGroomResolvesADatedSlug: the survey names the slug it proposed;
// the harness may have minted a dated sibling. The resolver has to see
// through that or every collision would silently drop its group.
func TestGroomResolvesADatedSlug(t *testing.T) {
	root := spawnFixture(t)

	// Mint fix-a, close it out of the live set, then mint fix-a again —
	// the second one gets a dated slug.
	first := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "fix-a", Title: "A"}}, io.Discard, os.Stderr)
	setRunStatus(t, root, "moe", first["fix-a"], run.StatusMerged)
	minted := mintSpecs(root, "moe", "pulse-two",
		[]pulseRunSpec{{Slug: "fix-a", Title: "A again"}, {Slug: "fix-b", Title: "B"}}, io.Discard, os.Stderr)
	if minted["fix-a"] == "fix-a" {
		t.Fatalf("second mint reused the bare slug %q — fixture assumption broken", minted["fix-a"])
	}

	// Groom with an empty `minted` map, so resolution has to go through
	// the on-disk lookup rather than this batch's own mints.
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	edges := liveEdges(t, root)
	if edges["moe/"+minted["fix-a"]] != "moe/"+minted["fix-b"] {
		t.Fatalf("edges = %v, want the dated %s chained to %s", edges, minted["fix-a"], minted["fix-b"])
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

	groomed := groomChains(root, "moe", "pulse-groom", []groomGroup{
		{Runs: runsFrom("fix-a", "fix-b")},
		{Onto: "fix-c", Runs: runsFrom("fix-a")},
	}, nil /*kickoff edges*/, io.Discard, os.Stderr)

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

// TestGroomSkipsWhenChainEdgesMovedUnderTheSweep: the ordering-drift
// guard. The survey formed its opinion against the edge set it was
// handed at kickoff; if an operator `chain edit` (or a concurrent sweep)
// moved those edges while the agent was thinking, the opinion is about a
// picture that no longer exists. Groom and kick are skipped, with one
// line naming what moved. Spawns are unaffected — they already landed,
// and they carry their own dedupe.
func TestGroomSkipsWhenChainEdgesMovedUnderTheSweep(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")
	a, b := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"]

	// What the agent saw: nothing chained. Then the operator chains
	// a → b while the survey is running.
	kickoff := map[string]string{}
	groomChains(root, "moe", "operator-edit",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil, io.Discard, os.Stderr)

	var errb bytes.Buffer
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: "fix-c", Runs: runsFrom("fix-a")}},
		kickoff, io.Discard, &errb)

	if len(groomed.threads) != 0 {
		t.Fatalf("threads = %+v, want none — a stale ordering opinion kicks nothing", groomed.threads)
	}
	if edges := liveEdges(t, root); edges[a] != b {
		t.Errorf("edges = %v, want the operator's %s → %s untouched", edges, a, b)
	}
	if !strings.Contains(errb.String(), "chain edges moved under this sweep") {
		t.Errorf("stderr = %q, want the drift skip named", errb.String())
	}
	if !strings.Contains(errb.String(), a) {
		t.Errorf("stderr = %q, want it to name the parent that moved (%s)", errb.String(), a)
	}
}

// TestGroomProceedsWhenTheEdgeSetIsUnchanged: the guard is narrow. The
// sweep's own spawns commit between kickoff and apply and move no chain
// edge, so the common case still grooms.
func TestGroomProceedsWhenTheEdgeSetIsUnchanged(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	a, b := "moe/"+minted["fix-a"], "moe/"+minted["fix-b"]

	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}},
		map[string]string{} /*nothing chained at kickoff*/, io.Discard, os.Stderr)

	if edges := liveEdges(t, root); edges[a] != b {
		t.Fatalf("edges = %v, want %s → %s stamped", edges, a, b)
	}
}

// stagedUnderHead mints an operator head (no SpawnedBy) with the named
// parked runs chained under it, the way an operator staging a batch by
// hand leaves the board. Returns the head's qualified key.
func stagedUnderHead(t *testing.T, root, headSlug string, members ...string) string {
	t.Helper()
	head, err := mintChainRun(root, "moe", headSlug, "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	prev := "moe/" + head.ID
	for _, m := range members {
		chainEdgeCommit(t, root, prev, "moe/"+m)
		prev = "moe/" + m
	}
	return "moe/" + head.ID
}

// TestGroomWontMoveAMemberOutOfAnOperatorHead is the staging fence's
// load-bearing direction. Grooming's move authority is what could
// re-stamp a run *out* of a unit the operator was still composing, and
// under a resident clock that would make hand-curation impossible.
func TestGroomWontMoveAMemberOutOfAnOperatorHead(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	headKey := stagedUnderHead(t, root, "operator-topic", minted["fix-a"])

	before := liveEdges(t, root)
	var errb bytes.Buffer
	// A later sweep tries to consolidate fix-a onto fix-b's thread.
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-b", "fix-a")}}, nil /*kickoff edges*/, io.Discard, &errb)

	after := liveEdges(t, root)
	if after[headKey] != before[headKey] {
		t.Errorf("head edge moved: %q → %q; the operator's staging fence must hold", before[headKey], after[headKey])
	}
	if !strings.Contains(errb.String(), "the operator is staging under "+headKey) {
		t.Errorf("stderr = %q, want the fence named", errb.String())
	}
}

// TestGroomWontSpliceIntoAnOperatorHead is the other direction: an
// `onto` aimed inside a hand-staged unit self-roots rather than
// splicing, the same redirect the static-ride fence takes. The work is
// still worth teeing up; it just doesn't join the operator's batch.
func TestGroomWontSpliceIntoAnOperatorHead(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	headKey := stagedUnderHead(t, root, "operator-topic", minted["fix-a"])

	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: "fix-a", Runs: runsFrom("fix-b")}}, nil /*kickoff edges*/, io.Discard, &errb)

	edges := liveEdges(t, root)
	if edges["moe/"+minted["fix-a"]] != "" {
		t.Errorf("fix-a gained a child %q; the fence must refuse the splice", edges["moe/"+minted["fix-a"]])
	}
	if edges[headKey] != "moe/"+minted["fix-a"] {
		t.Errorf("head → %q, want the operator's unit unchanged", edges[headKey])
	}
	if !strings.Contains(errb.String(), "self-rooting instead (the head is the fence)") {
		t.Errorf("stderr = %q, want the redirect named", errb.String())
	}
}

// TestGroomStillConsolidatesUnderAMachineHead: the fence is scoped to
// one unit-shape and nothing else. A pulse-minted head is SpawnedBy-
// stamped, so consolidation-by-moving keeps working exactly where it
// actually runs — which is the whole story the no-source-filter rule
// was written for.
func TestGroomStillConsolidatesUnderAMachineHead(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	head, err := mintChainRun(root, "moe", "machine-topic", "moe/pulse-1", "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/"+head.ID, "moe/"+minted["fix-a"])

	var errb bytes.Buffer
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-b", "fix-a")}}, nil /*kickoff edges*/, io.Discard, &errb)

	edges := liveEdges(t, root)
	if edges["moe/"+minted["fix-b"]] != "moe/"+minted["fix-a"] {
		t.Errorf("fix-b → %q, want fix-a moved onto it; machine units stay groomable\nstderr=%s",
			edges["moe/"+minted["fix-b"]], errb.String())
	}
}
