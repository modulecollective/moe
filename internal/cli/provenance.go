package cli

import (
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
)

// provenanceMaxHops bounds the upward MoE-Spawned-By walk. The data is
// machine-written and a cycle is already impossible by construction
// (a spawner exists before its child), but a page render is no place to
// hang on a hand-edited journal — the seen-set catches loops and this
// catches a pathologically long legitimate chain.
const provenanceMaxHops = 10

// provEdge is one step of the upward walk, held in the direction the
// data is stored: `child` came from `source` by `kind`. Display inverts
// this — see the emit loop in runProvenance — so the walk and the render
// don't have to agree on direction.
type provEdge struct {
	child   string // qualified run this edge explains
	kind    provKind
	source  string // qualified run or idea; empty for provOperator
	agent   bool
	consent string
	why     string
}

type provKind int

const (
	provSpawn provKind = iota
	provReopen
	provPromote
	provOperator
)

// verb is how the edge reads downward, from source to child. The walk
// finds edges child-first ("opened by"); the page reads them root-first,
// so every verb here points the other way.
func (k provKind) verb() string {
	switch k {
	case provReopen:
		return "reopened as"
	case provPromote:
		return "promoted to"
	case provOperator:
		return "opened"
	default:
		return "spawned"
	}
}

// runProvenance answers "how did this run come to be, and did a human
// consent to it?" as a list of display-ready hops, root first: the
// oldest actor at the top, each later hop a step down the chain toward
// this run, and the machine-walk ship — the newest event of all — last.
//
// The walk itself goes the other way: describe how this run opened,
// then — if the machine spawned it — describe how its spawner opened,
// and so on up. Everything it reads is already on disk: run.json's
// spawned_by, the journal index's spawn/consent/promote maps, and the
// spawning pulse's canvas gate for the reason it recorded.
//
// Nothing here fails a page. A run whose spawner has been pruned, a
// pulse whose canvas gate was edited into unparseability, an idea that
// no longer exists — each degrades to a hop that says less, never to an
// error. The one hard rule is the honesty rule: absence is never
// rendered as a claim. A commit written before the MoE-Consent trailer
// landed has unknown consent, so the hop shows no consent word; only an
// absent spawned_by supports the positive "opened by operator" claim,
// and only because no operator verb has ever written that field. A link
// is a claim too — that the page it points at exists — so a run the
// walk could name but not load is named without one.
func runProvenance(root, projectID, slug string) ([]serve.ProvHop, error) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, err
	}
	// Reverse PromotedTo (source idea → destination run) so a hop can ask
	// "which idea was I promoted from?". The forward map is the one the
	// journal carries; the run page reads the edge backwards.
	promotedFrom := make(map[string]string, len(idx.PromotedTo))
	for idea, dest := range idx.PromotedTo {
		if _, dup := promotedFrom[dest]; !dup {
			promotedFrom[dest] = idea
		}
	}

	self := projectID + "/" + slug

	var edges []provEdge
	seen := map[string]bool{}
	// Which named runs are no longer on disk. The walk already asks the
	// question for every hop it visits and throws the answer away; keeping
	// it costs nothing and is what decides whether a name gets a link.
	gone := map[string]bool{}
	cur := self
	for hop := 0; hop < provenanceMaxHops && cur != "" && !seen[cur]; hop++ {
		seen[cur] = true
		curProject, curSlug, ok := strings.Cut(cur, "/")
		if !ok {
			break
		}

		md, _ := run.Load(root, curProject, curSlug)
		gone[cur] = md == nil
		spawner := idx.SpawnedBy[cur]
		if md != nil && md.SpawnedBy != "" {
			spawner = md.SpawnedBy
		}

		switch {
		case spawner != "":
			edges = append(edges, provEdge{
				child: cur, kind: provSpawn, source: spawner,
				// spawned_by is machine-only by construction — no operator
				// verb has ever written it — so the marker is sound for the
				// whole history, even where the consent level isn't.
				agent:   true,
				consent: idx.SpawnConsent[cur],
				why:     spawnWhy(root, spawner, curSlug),
			})
			cur = spawner
			continue
		case md != nil && md.ReopenOf != "":
			edges = append(edges, provEdge{
				child: cur, kind: provReopen, source: curProject + "/" + md.ReopenOf,
			})
		case promotedFrom[cur] != "":
			edges = append(edges, provEdge{
				child: cur, kind: provPromote, source: promotedFrom[cur],
			})
		case md != nil:
			edges = append(edges, provEdge{child: cur, kind: provOperator})
		}
		// No arm matched: the run is gone and the journal knows nothing
		// about its opening, so the chain simply starts below it. Reopen
		// and promote name a source but not a *cause the machine chose*,
		// so the walk stops there rather than re-telling the source run's
		// own story on this page.
		break
	}

	// The root edge can name a source the walk never visited: a reopen or
	// promote source, which it deliberately stops below, or the run a
	// maxHops exit stopped short of. One load answers for it.
	if len(edges) > 0 {
		if src := edges[len(edges)-1].source; src != "" {
			if _, asked := gone[src]; !asked {
				gone[src] = !runLoads(root, src)
			}
		}
	}

	hops := provHops(edges, self, gone)
	if consent, ok := idx.PushConsent[self]; ok {
		// Newest event in the story, so it lands at the bottom — and it
		// hangs off this run, not off the chain above it.
		hops = append(hops, serve.ProvHop{
			Verb:    "shipped by",
			Object:  "a machine walk",
			Agent:   true,
			Consent: consent,
		})
	}
	return hops, nil
}

// runLoads reports whether a qualified run still has metadata on disk —
// the question the walk's own run.Load answers for every hop it visits,
// asked here about a run it never had to visit.
func runLoads(root, qualified string) bool {
	projectID, slug, ok := strings.Cut(qualified, "/")
	if !ok {
		return false
	}
	md, _ := run.Load(root, projectID, slug)
	return md != nil
}

// provHops renders the walk's edges as display lines, root first: one
// line naming the actor or run the story starts from, then one "→ <verb>
// <run>" line per edge, each line's elided subject being the line above.
// A run in `gone` is named but not linked: the page it would point at
// doesn't exist.
func provHops(edges []provEdge, self string, gone map[string]bool) []serve.ProvHop {
	if len(edges) == 0 {
		return nil
	}
	root := edges[len(edges)-1]
	// A plain operator-opened run is a one-line story. Rendering it as
	// "operator / → opened this run" spends two lines and an arrow to say
	// what one line already said.
	if len(edges) == 1 && root.kind == provOperator {
		return []serve.ProvHop{{Verb: "opened by operator"}}
	}

	var hops []serve.ProvHop
	switch {
	case root.source != "":
		// Either the source of a reopen/promote, or — when the walk ran
		// out of story above it — the oldest run it could still name. The
		// latter starts the chain mid-story rather than inventing an
		// origin for a run whose own is unknown.
		start := serve.ProvHop{Subject: root.source}
		if !gone[root.source] {
			start.SubjectURL = "/run/" + root.source
		}
		hops = append(hops, start)
	default:
		hops = append(hops, serve.ProvHop{Subject: "operator"})
	}
	for i := len(edges) - 1; i >= 0; i-- {
		e := edges[i]
		object, objectURL := e.child, ""
		switch {
		case e.child == self:
			// Same rationale as the elided subject: the page naming itself
			// back to its reader is noise.
			object = "this run"
		case !gone[e.child]:
			objectURL = "/run/" + e.child
		}
		hops = append(hops, serve.ProvHop{
			Verb:      e.kind.verb(),
			Object:    object,
			ObjectURL: objectURL,
			Agent:     e.agent,
			Consent:   e.consent,
			Why:       e.why,
		})
	}
	return hops
}

// spawnWhy recovers the reason a spawner recorded for the run it opened.
// Today that means a pulse: its survey canvas's `## Gate` fence carries
// one spec per run it asked for — loose, or inline at a thread position
// — each with the `why` the operator would otherwise have to reconstruct
// from the journal.
//
// Returns "" for every shape that can't answer — a non-pulse spawner, a
// closed pulse whose canvas was edited, a gate that predates the field,
// no matching entry. The hop then renders without a reason.
func spawnWhy(root, spawner, childSlug string) string {
	spawnerProject, spawnerSlug, ok := strings.Cut(spawner, "/")
	if !ok {
		return ""
	}
	gate, ok := readPulseGate(root, spawnerProject, spawnerSlug)
	if !ok {
		return ""
	}
	specs := gate.specs()
	for _, s := range specs {
		if s.Slug == "" {
			continue
		}
		// The harness dates a slug on collision ("foo" → "foo-2026-07-20"),
		// so an exact match is the common case and a dated suffix is the
		// collision case. A `twin` spec asks for a harness-minted
		// `reflect-YYYY-MM-DD`, which matches neither — hence the
		// workflow-keyed fallback below.
		if s.Slug == childSlug || strings.HasPrefix(childSlug, s.Slug+"-") {
			return strings.TrimSpace(s.Why)
		}
	}
	for _, s := range specs {
		if s.Workflow == "twin" && strings.HasPrefix(childSlug, "reflect") {
			return strings.TrimSpace(s.Why)
		}
	}
	return ""
}
