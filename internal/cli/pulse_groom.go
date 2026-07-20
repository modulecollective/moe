package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// Grooming is the pulse's ordering brain rendered as chain edges.
//
// The primitive is "chain after an existing item" — that is the whole
// mechanism. A group of runs attaches after any run in the project (a
// tail, so it appends; a mid-chain member, so it splices; a loose run,
// which thereby roots a thread), or self-roots as a new headless
// thread. There is no one-lane-per-project rule and no
// fresh-head-per-batch rule: a head is minted only when the survey asks
// for one by name, because naming the group helps the dash tell the
// story. Threads multiply and merge by judgment — stray parked threads
// are exactly what a later pulse consolidates by moving, so the pile-up
// self-heals. The groomer is the merge.
//
// Moving is the same act as placing. A `runs` slug may name a parked
// run already chained elsewhere — under any head, operator-minted
// included — and grooming re-stamps it to the group's placement,
// splicing the old unit around the gap. There is no source filter: any
// parked run in the project is groomable. That is the design's sharpest
// edge, and the placement bar in pulse.md ("would the operator kick
// these, in this order, unchanged?") is what carries it.
//
// Appending or moving onto a *parked* chain — anyone's — is curation,
// not execution: nothing moves until that chain's kick. The one place
// grooming is fenced is the unit a *static* ride is currently walking,
// in both directions: a placement aimed into it is redirected (see
// groomAnchor), and a `runs` slug naming one of its members is dropped
// rather than moved out (see placeGroup).

// pulseChainGroup is one entry in the gate's `chain` list: a group of
// run slugs in execution order, plus where they go.
//
// Onto attaches the group after that run, wherever it sits. Head mints
// a chain placeholder with that slug base and chains the group under
// it. Neither is the opportunistic placement — after the tail of the
// chain the pulse fired on, when there is one and the ride is dynamic;
// otherwise a self-rooted parked thread. Onto and Head together is a
// warn-and-skip: they are two different answers to the same question.
//
// Kick asks the harness to kick the group's thread once grooming is
// done. Two structural conditions gate it (see pulseSelfKick); the
// agent's `true` is a request, not an instruction.
type pulseChainGroup struct {
	Onto string   `json:"onto"`
	Head string   `json:"head"`
	Runs []string `json:"runs"`
	Kick bool     `json:"kick"`
}

// chainGraph is the live edge set, in memory, so a groom sweep can
// place, splice and move against a consistent picture and emit one
// commit at the end. Mutating a map and diffing it at the end is what
// makes "move a run out of one chain and into another" a single
// operation with the old unit restitched, rather than a bespoke
// unchain-then-rechain path beside `moe chain edit`'s.
//
// Only edges whose child is live are loaded (run.ChainChildLive — the
// same read-side rule every other edge reader applies), and only
// parents grooming actually wrote are diffed. An edge this sweep never
// touched can therefore never be cleared as a side effect, which is the
// difference between a groom step and a `chain clear`.
type chainGraph struct {
	child   map[string]string
	parents map[string][]string
	orig    map[string]string
	touched map[string]bool
}

func newChainGraph(idx *run.JournalIndex, byKey map[string]*run.Metadata) *chainGraph {
	g := &chainGraph{
		child:   map[string]string{},
		parents: map[string][]string{},
		orig:    map[string]string{},
		touched: map[string]bool{},
	}
	for parent, child := range idx.ChainedChild {
		if child == "" || !run.ChainChildLive(child, byKey) {
			continue
		}
		if _, ok := byKey[parent]; !ok {
			continue
		}
		g.child[parent] = child
		g.orig[parent] = child
		g.parents[child] = append(g.parents[child], parent)
	}
	for k := range g.parents {
		sort.Strings(g.parents[k])
	}
	return g
}

func (g *chainGraph) clearChild(parent string) {
	old, ok := g.child[parent]
	if !ok || old == "" {
		return
	}
	delete(g.child, parent)
	g.parents[old] = dropString(g.parents[old], parent)
	g.touched[parent] = true
}

func (g *chainGraph) setChild(parent, child string) {
	g.clearChild(parent)
	g.child[parent] = child
	g.parents[child] = append(g.parents[child], parent)
	sort.Strings(g.parents[child])
	g.touched[parent] = true
}

// detach lifts r out of its current position and restitches the hole:
// every run that chained to r now chains to whatever r chained to. This
// is the unchain-and-splice authority `moe chain edit` already has,
// expressed as one call so a move never leaves the old unit broken in
// half.
func (g *chainGraph) detach(r string) {
	after := g.child[r]
	before := append([]string(nil), g.parents[r]...)
	g.clearChild(r)
	for _, p := range before {
		if after != "" {
			g.setChild(p, after)
		} else {
			g.clearChild(p)
		}
	}
}

// tailFrom walks forward from a run to the end of its thread. The seen
// set is a cycle belt: chain edges are operator- and agent-writable and
// a loop would otherwise hang the sweep.
func (g *chainGraph) tailFrom(from string) string {
	seen := map[string]bool{from: true}
	cur := from
	for {
		next := g.child[cur]
		if next == "" || seen[next] {
			return cur
		}
		seen[next] = true
		cur = next
	}
}

// unit returns every member of the thread key belongs to: walk back to
// a root through the lowest-keyed parent (the deterministic choice
// liveChainParentOf already makes for fan-in), then forward.
func (g *chainGraph) unit(key string) map[string]bool {
	if key == "" {
		return nil
	}
	seen := map[string]bool{key: true}
	root := key
	for {
		ps := g.parents[root]
		if len(ps) == 0 || seen[ps[0]] {
			break
		}
		root = ps[0]
		seen[root] = true
	}
	members := map[string]bool{}
	for cur := root; cur != ""; cur = g.child[cur] {
		if members[cur] {
			break
		}
		members[cur] = true
	}
	return members
}

// diff renders the touched parents as chain trailers. Sorted by parent
// so commit bodies and tests are deterministic, matching diffChainEdit.
func (g *chainGraph) diff() (adds, removes []string) {
	parents := make([]string, 0, len(g.touched))
	for p := range g.touched {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	for _, p := range parents {
		before, now := g.orig[p], g.child[p]
		if before == now {
			continue
		}
		if before != "" {
			removes = append(removes, p+" "+before)
		}
		if now != "" {
			adds = append(adds, p+" "+now)
		}
	}
	return adds, removes
}

func dropString(xs []string, want string) []string {
	var out []string
	for _, x := range xs {
		if x != want {
			out = append(out, x)
		}
	}
	return out
}

// loadChainGraph builds the live edge graph off the journal. ok is
// false when the read fails; every caller treats that as "can't tell"
// and takes its conservative branch.
func loadChainGraph(root string) (*chainGraph, bool) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, false
	}
	mds, err := run.Scan(root)
	if err != nil {
		return nil, false
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	return newChainGraph(idx, byKey), true
}

// groomedThread records where a group landed, for the kick step. Root
// is the thread's kickable handle: the minted head when the group asked
// for one, else the group's first member.
type groomedThread struct {
	Root string
	Kick bool
}

// groomSweep carries the one-sweep state the group walk threads: the
// graph, the resolver's inputs, and the spawner context the placement
// rules key on.
type groomSweep struct {
	root       string
	projectID  string
	pulseSlug  string
	graph      *chainGraph
	byKey      map[string]*run.Metadata
	minted     map[string]string
	spawnerKey string
	// spawnerUnit is the thread the pulse fired on. Under a static ride
	// this is the unit being walked right now, and grooming is fenced
	// out of it.
	spawnerUnit map[string]bool
	mode        rideMode
}

// groomChains is the pulse's groom step: walk the gate's `chain` groups
// in order, stamp the edges they imply, and report the threads a kick
// step may root. minted maps each proposed spawn slug to the run id the
// harness actually opened (the slug is dated on collision), so a group
// can name a run from this same batch.
//
// Warn-only throughout, like the spawn step beside it: a group that
// can't be resolved drops with a stderr line and the rest of the sweep
// carries on. Grooming is an ordering opinion — losing one is a
// re-groom next pulse, not a lost sweep.
func groomChains(root, projectID, pulseSlug string, groups []pulseChainGroup, minted map[string]string, spawnerKey string, stdout, stderr io.Writer) []groomedThread {
	if len(groups) == 0 {
		return nil
	}
	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "pulse: groom: scan runs for %s: %v\n", projectID, err)
		return nil
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "pulse: groom: build index for %s: %v\n", projectID, err)
		return nil
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	sw := &groomSweep{
		root:      root,
		projectID: projectID,
		pulseSlug: pulseSlug,
		graph:     newChainGraph(idx, byKey),
		byKey:     byKey,
		minted:    minted,
		mode:      currentRideMode,
	}
	if spawnerKey != "" {
		sw.spawnerKey = spawnerKey
		sw.spawnerUnit = sw.graph.unit(spawnerKey)
		if len(sw.spawnerUnit) < 2 {
			// A run with no live edges either way is not a chain member;
			// there is no unit to extend and none to fence off.
			sw.spawnerUnit = nil
		}
	}

	var threads []groomedThread
	for i, grp := range groups {
		th, ok := sw.placeGroup(i, grp, stdout, stderr)
		if ok {
			threads = append(threads, th)
		}
	}

	adds, removes := sw.graph.diff()
	if len(adds) == 0 && len(removes) == 0 {
		return threads
	}
	msg := fmt.Sprintf("chain: groom %s/%s (%d added, %d removed)\n\n", projectID, pulseSlug, len(adds), len(removes)) +
		// Groom is always the pulse acting, ride or no ride — so the
		// consent trailer is unconditional here. It is what lets the
		// index tell a groomed edge from an operator `chain edit` one.
		trailers.Block{ChainedTo: adds, ChainedToRemoved: removes, Consent: currentRideMode.String()}.String()
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "pulse-groom",
		Run:     projectID + "/" + pulseSlug,
	}, stdout, stderr, func() error {
		// Same shape as `moe chain edit`'s save: the edges are the
		// truth, no file changed, so it is a trailer-only empty commit.
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "pulse: groom: stamp edges for %s: %v — the runs are open but ungroomed\n", projectID, err)
		return nil
	}
	moePrintf(stderr, "pulse: groomed %d chain edge(s) for %s\n", len(adds), projectID)
	return threads
}

// placeGroup resolves one group's members and anchor and rewrites the
// graph. Returns the thread it landed on, and false when the group was
// skipped.
func (sw *groomSweep) placeGroup(i int, grp pulseChainGroup, stdout, stderr io.Writer) (groomedThread, bool) {
	label := fmt.Sprintf("chain group %d", i+1)
	if grp.Onto != "" && grp.Head != "" {
		moePrintf(stderr, "pulse: groom: %s sets both `onto` and `head` — skipping\n", label)
		return groomedThread{}, false
	}

	var members []string
	for _, slug := range grp.Runs {
		key, ok := sw.resolveMember(slug)
		if !ok {
			moePrintf(stderr, "pulse: groom: %s names %q, which is not a parked run in %s — skipping that entry\n",
				label, slug, sw.projectID)
			continue
		}
		if sw.fenced(key) {
			// The other half of the static fence: a group may name a
			// still-parked member of the ridden unit and move it *out*,
			// which shrinks the ride the operator consented to just as
			// surely as an `onto` would grow it.
			moePrintf(stderr, "pulse: groom: %s names %s inside the chain this static ride is walking — dropping that entry (`!!!!` to reshape a ride)\n",
				label, key)
			continue
		}
		if indexOfString(members, key) >= 0 {
			continue
		}
		members = append(members, key)
	}
	if len(members) == 0 {
		moePrintf(stderr, "pulse: groom: %s resolved to no runs — skipping\n", label)
		return groomedThread{}, false
	}

	anchor, headKey, ok := sw.groomAnchor(label, grp, members, stdout, stderr)
	if !ok {
		return groomedThread{}, false
	}

	// Detach first, all of them: a group may collect runs from several
	// existing threads, and every old unit has to be restitched before
	// the new order is stamped or a member's stale outgoing edge would
	// survive as a fork.
	for _, m := range members {
		sw.graph.detach(m)
	}

	after := ""
	if anchor != "" {
		after = sw.graph.child[anchor]
		sw.graph.setChild(anchor, members[0])
	}
	for j := 0; j+1 < len(members); j++ {
		sw.graph.setChild(members[j], members[j+1])
	}
	last := members[len(members)-1]
	if after != "" && after != last {
		// Splice: the anchor already had a child, so the group goes
		// between them rather than at the end. Mid-ride, this is the
		// queue jump — work placed after an already-merged member runs
		// before the hop that was next.
		sw.graph.setChild(last, after)
	}

	rootKey := headKey
	if rootKey == "" {
		if anchor == "" {
			rootKey = members[0]
		} else {
			// The thread already had a root; the kickable handle is
			// whatever heads the unit this group joined.
			rootKey = sw.graph.threadRoot(anchor)
		}
	}
	return groomedThread{Root: rootKey, Kick: grp.Kick}, true
}

// threadRoot walks back to the head of key's thread — the run a kick
// would name.
func (g *chainGraph) threadRoot(key string) string {
	seen := map[string]bool{key: true}
	root := key
	for {
		ps := g.parents[root]
		if len(ps) == 0 || seen[ps[0]] {
			return root
		}
		root = ps[0]
		seen[root] = true
	}
}

// groomAnchor picks the run a group attaches after, applying the three
// placements in first-match order. Returns the anchor ("" self-roots
// the group), the minted head key if one was minted, and false when the
// group should be skipped entirely.
func (sw *groomSweep) groomAnchor(label string, grp pulseChainGroup, members []string, stdout, stderr io.Writer) (anchor, headKey string, ok bool) {
	switch {
	case grp.Onto != "":
		key, found := sw.resolveAnchor(grp.Onto)
		if !found {
			// Warn-and-skip, matching the spawn path's warn-only ethos:
			// an `onto` that resolves to nothing is a stale opinion, not
			// a reason to drop the sweep.
			moePrintf(stderr, "pulse: groom: %s attaches onto %q, which names no run in %s — skipping\n",
				label, grp.Onto, sw.projectID)
			return "", "", false
		}
		if indexOfString(members, key) >= 0 {
			moePrintf(stderr, "pulse: groom: %s attaches onto %q, which is also one of its own runs — skipping\n",
				label, grp.Onto)
			return "", "", false
		}
		if sw.fenced(key) {
			moePrintf(stderr, "pulse: groom: %s targets %s inside the chain this static ride is walking — self-rooting instead (`!!!!` to extend a ride)\n",
				label, key)
			return "", "", true
		}
		return key, "", true

	case grp.Head != "":
		slug := strings.TrimSpace(grp.Head)
		if slug == "" || run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: groom: %s asks for head %q, which is not a canonical slug — skipping\n", label, grp.Head)
			return "", "", false
		}
		// Provenance rides on the head canvas here and nowhere else: an
		// operator head's purpose note is theirs, and a groomed run's
		// `why` already travels on its own seeded design canvas.
		md, err := mintChainRun(sw.root, sw.projectID, slug, sw.projectID+"/"+sw.pulseSlug, "" /*note*/, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "pulse: groom: %s mint head %q: %v — skipping\n", label, slug, err)
			return "", "", false
		}
		key := sw.projectID + "/" + md.ID
		sw.byKey[key] = md
		moePrintf(stderr, "pulse: opened chain %s\n", key)
		return key, key, true

	default:
		// Opportunistic. Extending the ride in flight needs the fourth
		// bang; without it the group self-roots as a parked thread the
		// operator (or a later pulse) can pick up.
		if sw.mode == rideDynamic && len(sw.spawnerUnit) > 0 {
			return sw.graph.tailFrom(sw.spawnerKey), "", true
		}
		return "", "", true
	}
}

// fenced reports whether a run sits inside the unit a static ride is
// currently walking. `!!!`'s contract is that what the operator saw at
// kick time is what runs, which the sweep honours in both directions:
//
//   - As a placement target (groomAnchor), the group is redirected to a
//     self-rooted thread rather than refused — the work is still worth
//     teeing up, it just doesn't join this ride.
//   - As a group member (placeGroup), the entry is dropped, because
//     grooming a ridden run somewhere else would detach it and shrink
//     the ride out from under the kick.
//
// Since neither direction can touch the unit, the spawnerUnit snapshot
// taken at sweep start stays exact for the whole sweep.
//
// The spawner's unit *is* the ridden unit, so no extra identity has to
// be threaded down here to know which one that is.
func (sw *groomSweep) fenced(key string) bool {
	return sw.mode == rideStatic && sw.spawnerUnit[key]
}

// resolveMember maps a `runs` slug to a run key. This batch's own mints
// win first (the harness may have dated the slug), then any parked —
// in-progress — chainable run in the project, loose or already chained,
// machine- or operator-authored. Members must be parked: a merged run
// has nothing left to execute, so ordering it is meaningless.
func (sw *groomSweep) resolveMember(slug string) (string, bool) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", false
	}
	if id, ok := sw.minted[slug]; ok {
		return sw.projectID + "/" + id, true
	}
	return sw.lookup(slug, func(md *run.Metadata) bool {
		return md.Status == run.StatusInProgress && chainableWorkflow(md.Workflow)
	})
}

// resolveAnchor maps an `onto` slug to a run key. Wider than
// resolveMember on purpose: an anchor may be a merged member of a chain
// mid-ride (that is the queue-jump case), so status is not a filter
// here — only "is this a run in this project that can hold an edge".
func (sw *groomSweep) resolveAnchor(slug string) (string, bool) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", false
	}
	if id, ok := sw.minted[slug]; ok {
		return sw.projectID + "/" + id, true
	}
	return sw.lookup(slug, func(md *run.Metadata) bool {
		return chainableWorkflow(md.Workflow)
	})
}

// lookup finds the run in this project whose id is slug, or — failing
// that — whose id is one of IDBase's dated forms of it, so a survey that
// names `fix-ci` still finds the `fix-ci-2026-07-19` the harness
// actually minted. An exact hit always wins; among dated forms the
// lowest id wins, for determinism.
func (sw *groomSweep) lookup(slug string, admit func(*run.Metadata) bool) (string, bool) {
	exact := sw.projectID + "/" + slug
	if md, ok := sw.byKey[exact]; ok && admit(md) {
		return exact, true
	}
	best := ""
	for key, md := range sw.byKey {
		if md.Project != sw.projectID || !admit(md) {
			continue
		}
		if !slugBaseMatches([]string{md.ID}, slug) {
			continue
		}
		if best == "" || key < best {
			best = key
		}
	}
	return best, best != ""
}
