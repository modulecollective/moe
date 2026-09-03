package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
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
// run already chained elsewhere and grooming re-stamps it to the group's
// placement, splicing the old unit around the gap. Nearly any parked run
// in the project is groomable. That is the design's sharpest edge, and
// the placement bar in pulse.md ("would the operator kick these, in this
// order, unchanged?") is what carries it.
//
// Appending or moving onto a *parked* chain is curation, not execution:
// nothing moves until that chain's kick. Grooming is fenced out of one
// unit, in both directions — a placement aimed in is redirected to a
// self-rooted thread (see groomAnchor), and a `runs` slug naming a
// member is dropped rather than moved out (see placeGroup): any unit
// under an **operator-minted chain head** (see stagingFenced), because
// under a resident clock the head is the operator's staging fence.
// Machine kicks were already refused there twice over — a hand-minted
// head is stageless so it never clears the settled-design admit, and
// kicking a member under a live parent fails closed with "kick the head"
// — but grooming's move authority could still re-stamp members *out* of
// a unit the operator was composing. Machine-headed and headless threads
// stay fully groomable, so consolidation-by-moving is unchanged where it
// actually runs.
//
// A ride the operator kicked needs no fence of its own: nothing sweeps
// while one is walking, because no verb fires a pulse in-process. What
// the operator saw at kick time is what runs, by construction.

// groomGroup is one thread the sweep is about to stamp: its members in
// execution order, plus where the thread goes. Built from the gate's
// `threads` list by applyPulseGate once every run in it exists — see
// pulseThread for what each field means to the agent writing it.
type groomGroup struct {
	Onto string
	Head string
	Runs []groomMember
	Park string
}

// groomMember is one position in a groomGroup: either a run this sweep
// just minted, which travels as its own concrete id, or a slug naming a
// run that already existed, resolved against disk at apply time.
//
// This pairing is what replaced the alias map. The old gate split
// spawning and ordering into two lists that named each other through one
// shared slug namespace, written last-write-wins: an entry's
// agent-chosen alias could shadow an sdlc entry or a real parked run's
// slug, and the resolver consulted the map before disk — so the wrong
// run got ordered, silently. A minted run now carries its identity from
// the moment it exists, and nothing has to name anything.
type groomMember struct {
	mintedID string
	slug     string
}

// name is what the warn lines call this member — the id when the sweep
// minted it, else whatever the survey wrote.
func (m groomMember) name() string {
	if m.mintedID != "" {
		return m.mintedID
	}
	return m.slug
}

// groomedThread records where a group landed, for the kick step. Root
// is the thread's kickable handle, and it is derived from the *final*
// graph after every group has been placed — see groomChains. Handle is
// the run the root is walked back from: the minted head when the group
// asked for one, else the group's first member.
type groomedThread struct {
	Handle string
	Root   string
	// Park carries the survey's reason to hold this thread, verbatim from
	// the gate. Empty is the common case and means "start it" — under a
	// dynamic sweep, and only there.
	Park string
}

// groomResult is what the groom step hands the kick step. The whole
// point is that the kick reads nothing from disk: every question it asks
// — where does this thread start, is that run kickable — is answered
// from the same final in-memory graph the sweep just stamped. A second
// read would be a second answer, and the window between them is exactly
// where a stale kick root came from.
type groomResult struct {
	threads []groomedThread
	// byKey is the sweep's run metadata, including any chain head this
	// sweep minted mid-walk.
	byKey map[string]*run.Metadata
	// mds, graph and projectID are what the kick's board enumeration
	// needs on top of the groomed threads: the scan to walk, the final
	// graph to walk it back through, and which project is being swept.
	// Carried rather than re-read for the same reason byKey is — the
	// kick's picture of the board has to be the one the sweep just
	// stamped, or an enumerated root names a thread that no longer
	// starts there.
	mds       []*run.Metadata
	graph     *run.ChainGraph
	projectID string
	// idx is the journal the sweep read to build its graph, carried so
	// the kick's readiness check (stage satisfaction, the chore map)
	// reads the same snapshot rather than forking a second index. It
	// predates only the chain heads the groom mints mid-walk, which
	// costs nothing: a run minted seconds ago has no work-turn or
	// advance marker to find. The gate's own spawns and chore
	// nominations land *before* this is built, which is what lets the
	// kick's chore leg see a chore run this same sweep opened.
	idx *run.JournalIndex
}

// groomSweep carries the one-sweep state the group walk threads: the
// graph and the resolver's inputs.
type groomSweep struct {
	root      string
	projectID string
	pulseSlug string
	graph     *run.ChainGraph
	byKey     map[string]*run.Metadata
}

// groomChains is the pulse's groom step: walk the gate's `threads` groups
// in order, stamp the edges they imply, and report the threads a kick
// step may root. Every group arrives with its members already resolved
// to a mint or a slug — see applyPulseGate, which opened them.
//
// kickoffEdges is the chain edge set the survey agent saw. If the live
// set has moved since, the whole ordering opinion is stale and grooming
// is skipped — see chainEdgesMoved. nil means "no snapshot" and skips
// the check.
//
// Warn-only throughout, like the spawn step beside it: a group that
// can't be resolved drops with a stderr line and the rest of the sweep
// carries on. Grooming is an ordering opinion — losing one is a
// re-groom next pulse, not a lost sweep.
func groomChains(root, projectID, pulseSlug string, groups []groomGroup, kickoffEdges map[string]string, stdout, stderr io.Writer) groomResult {
	// Every early return below is a groom that produced no threads and no
	// graph, so the kick's board enumeration finds nothing to start.
	var bail groomResult
	if len(groups) == 0 && currentRideMode != rideDynamic {
		// Nothing to place, and no kick downstream to feed: the scan and
		// index below would answer a question nobody is going to ask.
		// Under a dynamic sweep they are exactly the question — the kick
		// enumerates the board off this graph, and a survey with no
		// ordering opinion is the ordinary shape of a board whose stalled
		// thread is already correctly ordered.
		return bail
	}
	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "pulse: groom: scan runs for %s: %v\n", projectID, err)
		return bail
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "pulse: groom: build index for %s: %v\n", projectID, err)
		return bail
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	graph := run.NewChainGraph(idx, byKey)

	// The one contested resource, checked optimistically. Everything else
	// about the sweep is allowed to move underneath it — the triggering
	// verb just wrote the journal, concurrent sweeps are deliberate, and
	// this sweep's own spawns commit mid-apply — so a whole-state pin
	// would be perpetually stale for benign reasons. Chain edges are
	// different: an ordering opinion is a claim *about* them, and
	// restamping a picture the agent never saw is how an operator's
	// `chain edit` mid-survey gets quietly undone.
	//
	// The retry is structural rather than built: the next sweep re-derives
	// the ordering for free, against the state that actually won. And
	// spawns have already landed by now — they carry their own dedupe and
	// need no ordering to be worth keeping.
	if moved := chainEdgesMoved(kickoffEdges, graph.Edges()); moved != "" {
		moePrintf(stderr, "pulse: groom: chain edges moved under this sweep (%s) — skipping groom and kick; the next pulse re-derives the order\n", moved)
		return bail
	}

	sw := &groomSweep{
		root:      root,
		projectID: projectID,
		pulseSlug: pulseSlug,
		graph:     graph,
		byKey:     byKey,
	}

	var threads []groomedThread
	for i, grp := range groups {
		th, ok := sw.placeGroup(i, grp, stdout, stderr)
		if ok {
			threads = append(threads, th)
		}
	}

	// The final graph is the plan, and everything downstream reads *from*
	// it rather than re-deriving its own answer. Two steps, in order:
	// validate it, then derive from it.
	//
	// Validation is one rule — no self-edges. Every branch that could
	// write one already guards against it, so this is the belt: a durable
	// `X → X` edge is not a bad ordering opinion, it is a corrupt graph,
	// and it outlives the sweep that wrote it.
	for _, parent := range sw.graph.SelfEdges() {
		moePrintf(stderr, "pulse: groom: dropping a self-edge on %s before stamping\n", parent)
		sw.graph.ClearChild(parent)
	}

	// Roots last, from the final graph. A group's root is not knowable
	// while the walk is still running: a later group may move the run an
	// earlier group's root was walked back from, and a root captured
	// mid-mutation names a thread that no longer starts there — the kick
	// then silently parks.
	for i := range threads {
		threads[i].Root = sw.graph.Root(threads[i].Handle)
	}

	result := groomResult{
		threads:   threads,
		byKey:     sw.byKey,
		mds:       mds,
		graph:     sw.graph,
		projectID: projectID,
		idx:       idx,
	}

	adds, removes := sw.graph.Diff()
	if len(adds) == 0 && len(removes) == 0 {
		return result
	}
	msg := fmt.Sprintf("chain: groom %s/%s (%d added, %d removed)\n\n", projectID, pulseSlug, len(adds), len(removes)) +
		// Groom is always the pulse acting, ride or no ride — so the
		// consent trailer is unconditional here. It is what lets the
		// index tell a groomed edge from an operator `chain edit` one.
		trailers.Block{ChainedTo: adds, ChainedToRemoved: removes, Consent: currentRideMode.String()}.String()
	err = repolock.With(root, repolock.Options{
		Purpose: "pulse-groom",
		Run:     projectID + "/" + pulseSlug,
	}, func() error {
		// Same shape as `moe chain edit`'s save: the edges are the
		// truth, no file changed, so it is a trailer-only empty commit.
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "pulse: groom: stamp edges for %s: %v — the runs are open but ungroomed\n", projectID, err)
		return bail
	}
	moePrintf(stderr, "pulse: groomed %d chain edge(s) for %s\n", len(adds), projectID)
	return result
}

// placeGroup resolves one group's members and anchor and rewrites the
// graph. Returns the thread it landed on, and false when the group was
// skipped.
func (sw *groomSweep) placeGroup(i int, grp groomGroup, stdout, stderr io.Writer) (groomedThread, bool) {
	label := fmt.Sprintf("chain group %d", i+1)
	if grp.Onto != "" && grp.Head != "" {
		moePrintf(stderr, "pulse: groom: %s sets both `onto` and `head` — skipping\n", label)
		return groomedThread{}, false
	}

	var members []string
	for _, m := range grp.Runs {
		key, ok := sw.resolveMember(m)
		if !ok {
			moePrintf(stderr, "pulse: groom: %s names %q, which is not a parked run in %s — skipping that entry\n",
				label, m.name(), sw.projectID)
			continue
		}
		if head := sw.stagingFenced(key); head != "" {
			moePrintf(stderr, "pulse: groom: %s names %s, which the operator is staging under %s — dropping that entry (the head is the fence)\n",
				label, key, head)
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
		sw.graph.Detach(m)
	}

	after := ""
	if anchor != "" {
		after = sw.graph.Child(anchor)
		sw.graph.SetChild(anchor, members[0])
	}
	for j := 0; j+1 < len(members); j++ {
		sw.graph.SetChild(members[j], members[j+1])
	}
	last := members[len(members)-1]
	if after != "" && after != last {
		// Splice: the anchor already had a child, so the group goes
		// between them rather than at the end. Mid-ride, this is the
		// queue jump — work placed after an already-merged member runs
		// before the hop that was next.
		sw.graph.SetChild(last, after)
	}

	// The handle, not the root: a minted head if the group asked for one,
	// else the group's own first member. Both are runs this group put
	// somewhere; whatever heads the thread they end up in is derived once
	// the whole walk is done. Deriving it here would read the graph
	// mid-mutation.
	handle := headKey
	if handle == "" {
		handle = members[0]
	}
	return groomedThread{Handle: handle, Park: grp.Park}, true
}

// groomAnchor picks the run a group attaches after, applying the three
// placements in first-match order. Returns the anchor ("" self-roots
// the group), the minted head key if one was minted, and false when the
// group should be skipped entirely.
func (sw *groomSweep) groomAnchor(label string, grp groomGroup, members []string, stdout, stderr io.Writer) (anchor, headKey string, ok bool) {
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
		if head := sw.stagingFenced(key); head != "" {
			moePrintf(stderr, "pulse: groom: %s targets %s, which the operator is staging under %s — self-rooting instead (the head is the fence)\n",
				label, key, head)
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
		// Neither `onto` nor `head`: the group self-roots as its own
		// thread. A dynamic sweep's kick loop starts it; anything else
		// parks it for the operator (or a later pulse) to pick up.
		return "", "", true
	}
}

// stagingFenced reports the operator-minted chain head a run is staged
// under, or "" when the run is machine territory. This is the durable
// hold, and it is the head the operator already names with `moe chain
// new` — no new mark was invented for it.
//
// "Operator-minted" is read off the head's own SpawnedBy: a pulse-minted
// head is stamped (see mintChainRun's caller in groomAnchor), and
// absence means *unknown, never operator's-to-take* — the same mark
// semantics every other machine/human split in the system uses. The
// fence is scoped to `chain`-workflow heads on purpose: a headless
// thread whose root is a settled operator run stays machine territory,
// which is the line the design draws and the one verb (`moe chain new`)
// the operator has to reach for to hold something.
func (sw *groomSweep) stagingFenced(key string) string {
	root := sw.graph.Root(key)
	md := sw.byKey[root]
	if md == nil || md.Workflow != chainWorkflow || md.SpawnedBy != "" {
		return ""
	}
	return root
}

// resolveMember maps one thread position to a run key. A run this sweep
// minted needs no lookup at all — it carries its own id. Anything else
// is a slug naming a parked — in-progress — chainable run in the
// project, loose or already chained, machine- or operator-authored.
// Members must be parked: a merged run has nothing left to execute, so
// ordering it is meaningless.
func (sw *groomSweep) resolveMember(m groomMember) (string, bool) {
	if m.mintedID != "" {
		return sw.projectID + "/" + m.mintedID, true
	}
	slug := strings.TrimSpace(m.slug)
	if slug == "" {
		return "", false
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

// chainEdgesMoved reports what changed between the edge set the survey
// agent saw and the one at apply time, or "" when nothing did. A nil
// `before` is "no snapshot" — the kickoff couldn't read the graph — and
// reads as no drift, because refusing to groom on a read we never made
// would turn one bad read into a lost sweep.
//
// The answer names the parents whose outgoing edge differs, sorted and
// capped, so the warn line says what moved rather than just that
// something did.
func chainEdgesMoved(before, now map[string]string) string {
	if before == nil {
		return ""
	}
	changed := map[string]bool{}
	for parent, child := range before {
		if now[parent] != child {
			changed[parent] = true
		}
	}
	for parent, child := range now {
		if before[parent] != child {
			changed[parent] = true
		}
	}
	if len(changed) == 0 {
		return ""
	}
	parents := make([]string, 0, len(changed))
	for p := range changed {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	const cap = 3
	if len(parents) > cap {
		return strings.Join(parents[:cap], ", ") + fmt.Sprintf(" and %d more", len(parents)-cap)
	}
	return strings.Join(parents, ", ")
}
