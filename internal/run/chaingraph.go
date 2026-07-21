package run

import (
	"sort"
	"time"
)

// ChainGraph is the chain edge set as a graph — the one read-side view
// of "what runs after what", shared by every surface that asks.
//
// Before this type there were four independent walkers over
// idx.ChainedChild: the dash and `chain edit`'s unit grouping, the head
// page's parent lookup, the settled-parent annotation, and the pulse's
// groom sweep. Each re-derived the same liveness rule, and each carried
// its own cycle belt against a hand-edited journal describing a loop.
// Four answers to one question is how they drift.
//
// Two rules define the edge set, applied once at build time:
//
//   - the child must be live (ChainChildLive — terminal children are
//     filtered, so an edge to one wouldn't fire on the ride), and
//   - the parent must be a run on disk.
//
// A consequence worth naming: a terminal run has no incoming edges here,
// so it always reads as its own root. That is what makes the pulse's
// "the unit my spawner sits in" question answerable at a tail, where the
// spawner has just merged.
//
// The mutating half (SetChild / ClearChild / Detach / Diff) has exactly
// one user — the pulse's groom sweep, which places, splices and moves
// against a consistent picture and emits one commit at the end. It lives
// here rather than in a wrapper because a graph you can only read is not
// enough to express a move, and two types sharing one edge map is the
// duplication this type exists to remove.
type ChainGraph struct {
	child   map[string]string
	parents map[string][]string
	byKey   map[string]*Metadata
	orig    map[string]string
	touched map[string]bool
}

// NewChainGraph builds the live edge graph from a journal index and the
// runs on disk.
func NewChainGraph(idx *JournalIndex, byKey map[string]*Metadata) *ChainGraph {
	g := &ChainGraph{
		child:   map[string]string{},
		parents: map[string][]string{},
		byKey:   byKey,
		orig:    map[string]string{},
		touched: map[string]bool{},
	}
	for parent, child := range idx.ChainedChild {
		if !ChainChildLive(child, byKey) {
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

// Child returns the run key chains to, or "" for none.
func (g *ChainGraph) Child(key string) string { return g.child[key] }

// Parents returns every run that chains to key, sorted. A child can have
// several parents — cross-parent fan-in is allowed — so this is a list,
// and the lowest key is the deterministic choice every walk-back makes.
func (g *ChainGraph) Parents(key string) []string { return g.parents[key] }

// walkChain follows next from `from` until it repeats or runs out, and
// returns the last key reached. The seen set is this package's one cycle
// belt: chain edges are operator- and agent-writable, a hand-edited
// journal can describe a loop, and neither a page render nor a groom
// sweep is any place to hang.
func walkChain(from string, next func(string) string) string {
	seen := map[string]bool{from: true}
	cur := from
	for {
		n := next(cur)
		if n == "" || seen[n] {
			return cur
		}
		seen[n] = true
		cur = n
	}
}

func (g *ChainGraph) lowestParent(key string) string {
	ps := g.parents[key]
	if len(ps) == 0 {
		return ""
	}
	return ps[0]
}

// Tail walks forward from a run to the end of its thread.
func (g *ChainGraph) Tail(key string) string { return walkChain(key, g.Child) }

// Root walks back to the head of key's thread — the run a kick names.
func (g *ChainGraph) Root(key string) string { return walkChain(key, g.lowestParent) }

// Thread returns key followed by every live chained child, head→tail —
// the walk `moe chain kick` rides, stopping exactly where it does.
func (g *ChainGraph) Thread(key string) []string {
	if key == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for cur := key; cur != "" && !seen[cur]; cur = g.child[cur] {
		seen[cur] = true
		out = append(out, cur)
	}
	return out
}

// Unit returns every member of the thread key belongs to: back to the
// root, then forward.
func (g *ChainGraph) Unit(key string) map[string]bool {
	if key == "" {
		return nil
	}
	members := map[string]bool{}
	for _, k := range g.Thread(g.Root(key)) {
		members[k] = true
	}
	return members
}

// LiveParentOf returns the in-progress run that chains to key, or "" for
// none. The guard behind `moe chain kick`: riding a chain from the
// middle is what it refuses, and the caller names the head instead.
func (g *ChainGraph) LiveParentOf(key string) string {
	for _, p := range g.parents[key] {
		if md, ok := g.byKey[p]; ok && md.Status == StatusInProgress {
			return p
		}
	}
	return ""
}

// TerminalParentOf returns the settled run that chains to key, or "" for
// none. The mirror of LiveParentOf: same fan-in rule, opposite liveness
// test.
//
// The edge outlives its parent — a chain's second-to-last item shipping
// doesn't remove the trailer — but every grouping view drops it, because
// a unit needs both endpoints active. That leaves the queued tail looking
// like an orphan on the dash and in the pulse's chain block the moment
// the run ahead of it merges. This is the lookup those two surfaces use
// to annotate the tail with the thread it belongs to.
//
// One hop only: for A→B→C with A and B both settled, C's answer is B. The
// nearest predecessor is the context worth naming; replaying the whole
// settled ancestry is noise.
func (g *ChainGraph) TerminalParentOf(key string) string {
	for _, p := range g.parents[key] {
		if p == key {
			continue
		}
		if md, ok := g.byKey[p]; ok && terminalChainStatus(md.Status) {
			return p
		}
	}
	return ""
}

// ChainOrderItem is one active run handed to Units: its qualified
// "<project>/<slug>" key and the recency time used to place it. Callers
// pass items in their own recency order (newest first); Units is stable
// over that order, so equal-time ties resolve however the caller already
// sorted them.
type ChainOrderItem struct {
	Key  string
	When time.Time
}

// Units groups the given active runs into chain units and returns them
// most-recent-first, each unit ordered head→tail. It is the shared
// ordering behind the dash's ACTIVE section and the `chain edit` editor
// file, so both read the same way.
//
// A unit is either a single run (an orphan, or any run with no live
// active child) or a head run followed transitively by its live chained
// children — a contiguous head→tail block. The graph's edges are
// narrowed further here: both endpoints must appear in items, because a
// unit is a thing the operator can see and reorder. Each unit floats by
// its most-recent member; units sort by that time, descending, stably
// over the caller's item order.
//
// A parentless cycle (no head) is caught by the safety net: any run left
// unplaced after the head walks is emitted as its own one-key unit in
// its recency slot, so no run is ever dropped.
//
// items must be in recency order (newest first); see ChainOrderItem.
func (g *ChainGraph) Units(items []ChainOrderItem) [][]string {
	inActive := make(map[string]bool, len(items))
	whenOf := make(map[string]time.Time, len(items))
	for _, it := range items {
		inActive[it.Key] = true
		whenOf[it.Key] = it.When
	}

	// childOf is ≤1 per parent; parentOf records the active incoming
	// edge — only its presence is read, so fan-in's last-writer-wins is
	// harmless.
	childOf := make(map[string]string)
	parentOf := make(map[string]string)
	for parent, child := range g.child {
		if inActive[parent] && inActive[child] {
			childOf[parent] = child
			parentOf[child] = parent
		}
	}

	type unit struct {
		keys []string
		rep  time.Time // representative time = most-recent member
	}
	consumed := make(map[string]bool, len(items))
	var units []unit
	for _, it := range items { // recency order
		k := it.Key
		if consumed[k] {
			continue
		}
		if _, hasParent := parentOf[k]; hasParent {
			continue // a member; emitted within its head's unit
		}
		if _, hasChild := childOf[k]; !hasChild {
			consumed[k] = true
			units = append(units, unit{keys: []string{k}, rep: it.When})
			continue
		}
		// Head: walk childOf transitively, cycle-guarded by consumed.
		var u unit
		for cur := k; cur != "" && !consumed[cur]; cur = childOf[cur] {
			consumed[cur] = true
			u.keys = append(u.keys, cur)
			if w := whenOf[cur]; w.After(u.rep) {
				u.rep = w
			}
		}
		units = append(units, u)
	}
	// Safety net for a parentless cycle (no head): keep any unplaced run
	// in its recency slot rather than dropping it.
	for _, it := range items {
		if !consumed[it.Key] {
			consumed[it.Key] = true
			units = append(units, unit{keys: []string{it.Key}, rep: it.When})
		}
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].rep.After(units[j].rep) })
	out := make([][]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.keys)
	}
	return out
}

// ClearChild drops key's outgoing edge.
func (g *ChainGraph) ClearChild(parent string) {
	old, ok := g.child[parent]
	if !ok || old == "" {
		return
	}
	delete(g.child, parent)
	g.parents[old] = dropChainParent(g.parents[old], parent)
	g.touched[parent] = true
}

// SetChild points parent at child, replacing any edge parent already had.
func (g *ChainGraph) SetChild(parent, child string) {
	g.ClearChild(parent)
	g.child[parent] = child
	g.parents[child] = append(g.parents[child], parent)
	sort.Strings(g.parents[child])
	g.touched[parent] = true
}

// Detach lifts r out of its current position and restitches the hole:
// every run that chained to r now chains to whatever r chained to. This
// is the unchain-and-splice authority `moe chain edit` already has,
// expressed as one call so a move never leaves the old unit broken in
// half.
func (g *ChainGraph) Detach(r string) {
	after := g.child[r]
	before := append([]string(nil), g.parents[r]...)
	g.ClearChild(r)
	for _, p := range before {
		if after != "" {
			g.SetChild(p, after)
		} else {
			g.ClearChild(p)
		}
	}
}

// SelfEdges lists every run currently chained to itself, sorted. Nothing
// should be able to write one, but the caller stamping a mutated graph
// wants a belt: a self-edge is durable, outlives the sweep that wrote it,
// and reads as a one-run cycle to every walker afterwards.
func (g *ChainGraph) SelfEdges() []string {
	var out []string
	for parent, child := range g.child {
		if parent == child {
			out = append(out, parent)
		}
	}
	sort.Strings(out)
	return out
}

// Diff renders the mutated parents as chain trailer values ("<parent>
// <child>"). Only parents the caller actually wrote are considered, so
// an edge the sweep never touched can never be cleared as a side effect
// — the difference between a groom step and a `chain clear`. Sorted by
// parent so commit bodies and tests are deterministic.
func (g *ChainGraph) Diff() (adds, removes []string) {
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

func dropChainParent(xs []string, want string) []string {
	var out []string
	for _, x := range xs {
		if x != want {
			out = append(out, x)
		}
	}
	return out
}

// terminalChainStatus reports whether a status is one the chain read-side
// treats as settled — nothing left to execute, so an edge into it never
// fires.
func terminalChainStatus(status string) bool {
	switch status {
	case StatusClosed, StatusMerged, StatusPromoted, StatusPushed:
		return true
	}
	return false
}

// ChainChildLive reports whether childKey names a run that's on disk
// and not terminal — the read-side of the chain-edge rule. Empty or
// missing-from-byKey counts as not live. The dash render, the chain-edit
// annotations and the graph above share this one definition.
func ChainChildLive(childKey string, byKey map[string]*Metadata) bool {
	if childKey == "" {
		return false
	}
	child, ok := byKey[childKey]
	if !ok {
		return false
	}
	return !terminalChainStatus(child.Status)
}
