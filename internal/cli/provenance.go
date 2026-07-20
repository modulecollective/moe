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

// runProvenance answers "how did this run come to be, and did a human
// consent to it?" as a list of display-ready hops, newest cause first.
//
// The walk is: describe how this run opened, then — if the machine
// spawned it — describe how its spawner opened, and so on up. Everything
// it reads is already on disk: run.json's spawned_by, the journal
// index's spawn/consent/promote maps, and the spawning pulse's canvas
// gate for the reason it recorded.
//
// Nothing here fails a page. A run whose spawner has been pruned, a
// pulse whose canvas gate was edited into unparseability, an idea that
// no longer exists — each degrades to a hop that says less, never to an
// error. The one hard rule is the honesty rule: absence is never
// rendered as a claim. A commit written before the MoE-Consent trailer
// landed has unknown consent, so the hop shows no consent word; only an
// absent spawned_by supports the positive "opened by operator" claim,
// and only because no operator verb has ever written that field.
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

	var hops []serve.ProvHop
	self := projectID + "/" + slug
	if consent, ok := idx.PushConsent[self]; ok {
		hops = append(hops, serve.ProvHop{
			Verb:    "shipped by",
			Object:  "a machine walk",
			Agent:   true,
			Consent: consent,
		})
	}

	seen := map[string]bool{}
	cur := self
	for hop := 0; hop < provenanceMaxHops && cur != "" && !seen[cur]; hop++ {
		seen[cur] = true
		curProject, curSlug, ok := strings.Cut(cur, "/")
		if !ok {
			break
		}
		// Subject is elided on the first hop: it describes the page's own
		// run, and naming it back to the reader is noise.
		subject, subjectURL := cur, "/run/"+cur
		if hop == 0 {
			subject, subjectURL = "", ""
		}

		md, _ := run.Load(root, curProject, curSlug)
		spawner := idx.SpawnedBy[cur]
		if md != nil && md.SpawnedBy != "" {
			spawner = md.SpawnedBy
		}

		switch {
		case spawner != "":
			hops = append(hops, serve.ProvHop{
				Subject:    subject,
				SubjectURL: subjectURL,
				Verb:       "opened by",
				Object:     spawner,
				ObjectURL:  "/run/" + spawner,
				// spawned_by is machine-only by construction — no operator
				// verb has ever written it — so the marker is sound for the
				// whole history, even where the consent level isn't.
				Agent:   true,
				Consent: idx.SpawnConsent[cur],
				Why:     spawnWhy(root, spawner, curSlug),
			})
			cur = spawner
			continue
		case md != nil && md.ReopenOf != "":
			hops = append(hops, serve.ProvHop{
				Subject:    subject,
				SubjectURL: subjectURL,
				Verb:       "reopened from",
				Object:     curProject + "/" + md.ReopenOf,
				ObjectURL:  "/run/" + curProject + "/" + md.ReopenOf,
			})
		case promotedFrom[cur] != "":
			hops = append(hops, serve.ProvHop{
				Subject:    subject,
				SubjectURL: subjectURL,
				Verb:       "promoted from idea",
				Object:     promotedFrom[cur],
				ObjectURL:  "/run/" + promotedFrom[cur],
			})
		default:
			hops = append(hops, serve.ProvHop{
				Subject:    subject,
				SubjectURL: subjectURL,
				Verb:       "opened by operator",
			})
		}
		// Reopen and promote name a source but not a *cause the machine
		// chose*, so the walk stops there rather than re-telling the
		// source run's own story on this page.
		break
	}
	return hops, nil
}

// spawnWhy recovers the reason a spawner recorded for the run it opened.
// Today that means a pulse: its survey canvas's `## Gate` fence carries
// one entry per spawn, each with the `why` the operator would otherwise
// have to reconstruct from the journal.
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
	for _, s := range gate.Spawn {
		if s.Slug == "" {
			continue
		}
		// The harness dates a slug on collision ("foo" → "foo-2026-07-20"),
		// so an exact match is the common case and a dated suffix is the
		// collision case. A `twin` entry's slug is a batch-local alias for
		// a harness-minted `reflect-YYYY-MM-DD`, which matches neither —
		// hence the workflow-keyed fallback below.
		if s.Slug == childSlug || strings.HasPrefix(childSlug, s.Slug+"-") {
			return strings.TrimSpace(s.Why)
		}
	}
	for _, s := range gate.Spawn {
		if s.Workflow == "twin" && strings.HasPrefix(childSlug, "reflect") {
			return strings.TrimSpace(s.Why)
		}
	}
	return ""
}
