package cli

import (
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// applyPulseGate opens every run the gate's specs describe and hands
// back the groom groups its threads imply. Two walks over one minter, in
// document order — `loose` first, then each thread's `runs` — so a slug
// proposed twice in one gate hits the dedupe rather than minting a dated
// sibling.
//
// Both spellings of a thread entry reach the minter: an object spec
// mints, and a string names a parked run — which may be a tagged idea,
// and then promotion is what "opens" it. Only the string that resolves
// to nothing promotable travels on as a slug for the groom.
//
// A minted run travels onward as its own run id rather than as a name in
// a shared namespace. That is the whole of the alias map's replacement:
// the gate's two lists used to name each other by slug, so the mapping
// had to exist, and every write into it was last-write-wins.
//
// Warn-only throughout: a spec that can't be minted drops with a stderr
// line and the rest of the sweep carries on. The report and filings are
// already durable, so a failed mint is a spawn-by-hand-later, not a lost
// sweep.
func applyPulseGate(root, projectID, pulseSlug string, gate pulseGate, stdout, stderr io.Writer) []groomGroup {
	m := &pulseMinter{root: root, projectID: projectID, pulseSlug: pulseSlug}
	for _, spec := range gate.Loose {
		m.mint(spec, stdout, stderr)
	}
	groups := make([]groomGroup, 0, len(gate.Threads))
	for _, th := range gate.Threads {
		grp := groomGroup{Onto: th.Onto, Head: th.Head, Park: th.Park}
		repair := false
		for _, entry := range th.Runs {
			repair = repair || entry.Repair != ""
		}
		if repair {
			if len(th.Runs) != 1 || th.Onto != "" || th.Head != "" {
				moePrintf(stderr, "pulse: CI repair must be a singleton group without onto or head — skipping\n")
				continue
			}
			grp.Repair = th.Runs[0].Existing
			groups = append(groups, grp)
			continue
		}
		for _, entry := range th.Runs {
			if entry.Spec == nil {
				// A string entry names "any parked run in the project", and a
				// tagged idea is one — the survey that nominated all three
				// parked ideas on 2026-08-13 spelled every entry this way and
				// the groom dropped all three, because resolveMember admits
				// only chainable runs. So promotion gets its turn here too:
				// the grammar offers two spellings and both have to work.
				// Anything that isn't a promotable idea falls through to the
				// slug it always was — an ordinary parked run is the common
				// case and stays the groom's to resolve.
				if id, _ := m.promoteIfTaggedIdea(entry.Existing, pulseRunSpec{Why: "named at a thread position"}, true /*atThread*/, stdout, stderr); id != "" {
					grp.Runs = append(grp.Runs, groomMember{mintedID: id})
					// A ping lands only on a run that already existed when the
					// survey wrote the gate. A promotion mints a different
					// run than the one the question was written against, so
					// the ask drops with a line rather than landing somewhere
					// the survey never looked.
					if entry.Ask != "" {
						moePrintf(stderr, "pulse: input: %s promoted to %s — question dropped\n", entry.Existing, id)
					}
					continue
				}
				grp.Runs = append(grp.Runs, groomMember{slug: entry.Existing})
				if entry.Ask != "" {
					askThreadEntry(root, projectID, pulseSlug, entry.Existing, entry.Ask, stderr)
				}
				continue
			}
			// Design-only is loose-only. Such a root is unsettled by
			// definition until the operator advances it, so heading a
			// thread with one would strand every member behind an
			// unwritten design — the exact shape the held-head groom
			// exists to undo, proposed on purpose. Skipped here rather
			// than in mint because only the caller knows the position.
			if entry.Spec.DesignOnly {
				moePrintf(stderr, "pulse: spawn: entry %q asks for design_only at a thread position — a design-only root strands the runs behind it; put it in loose. Skipping\n", entry.Spec.Slug)
				continue
			}
			// Mint in place. A spec that fails leaves a hole in the order
			// rather than shifting the rest — the warn line names it, and
			// the runs around it keep the positions the survey gave them.
			if id := m.mint(*entry.Spec, stdout, stderr); id != "" {
				grp.Runs = append(grp.Runs, groomMember{mintedID: id})
			}
		}
		groups = append(groups, grp)
	}
	return groups
}

// askThreadEntry opens the survey's ping on the run sitting at this
// thread position. Warn-only, like everything else in applyPulseGate: a
// refused ask costs the operator a stderr line and the next sweep's
// attention, nothing more.
//
// It holds nothing, deliberately. A survey that needs the answer before
// the work writes the thread's `park` too — the two are separate acts,
// so a question the work need not wait for is still askable.
//
// The refusals are the design's scope fence made mechanical. A chain
// placeholder has no agent turn to deliver the answer to, so it is not
// somewhere a question can usefully live; input.Ask covers the rest —
// a terminal run, an empty question, a run that already has one open.
func askThreadEntry(root, projectID, pulseSlug, slug, question string, stderr io.Writer) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "pulse: input: %s/%s: %v\n", projectID, slug, err)
		return
	}
	if md.Workflow == chainWorkflow {
		moePrintf(stderr, "pulse: input: %s/%s is a chain head — no stage to deliver an answer to; question dropped\n", projectID, slug)
		return
	}
	e, err := input.Ask(root, projectID, slug, projectID+"/"+pulseSlug, question, walkConsent())
	if err != nil {
		moePrintf(stderr, "pulse: input: %s/%s: %v\n", projectID, slug, err)
		return
	}
	moePrintf(stderr, "pulse: input: asked %s/%s#%d — %s\n", projectID, slug, e.ID, e.Question)
}

// pulseMinter opens the runs one sweep's gate asks for. It holds the
// live-slug dedupe set across the whole gate, scanning once on first
// use, and claims each base it mints so a repeat proposal in the same
// gate is a skip rather than a dated sibling.
type pulseMinter struct {
	root      string
	projectID string
	pulseSlug string
	live      []string
	scanned   bool
	broken    bool
}

// mint opens one run for one spec and returns its id, or "" when the
// spec was skipped.
//
// A `chore` spec takes its own path (nominateChore) — it opens no fresh
// run of the survey's design, only the one the operator already
// registered. Everything below is the fresh-run path.
//
// Dispatch is per-workflow, and sdlc (the default) is the only one that
// spawns. Anything else warns and skips.
//
// No numeric cap. The harness has no basis for judging which proposals
// to trim, and parked is itself the review gate: spawned runs are
// visible on the dash and prunable with `moe chain edit`. Over-proposal
// is visible junk, which the pulse already prefers to invisible absence.
// The bar — mechanical, bounded, verifiable — is taught in the stage
// fragment, where judgment belongs.
//
// The one mechanical exception to dedupe is a live idea carrying a
// harvested follow-up's workflow tag: the pulse promotes that capture
// through the same seam as `--from-idea`. Untagged ideas remain behind a
// structural human-triage fence. Every other live match still skips.
func (m *pulseMinter) mint(s pulseRunSpec, stdout, stderr io.Writer) string {
	projectID, pulseSlug := m.projectID, m.pulseSlug
	spawnedBy := projectID + "/" + pulseSlug

	// A chore nomination names a registration, not a slug to mint, so it
	// dispatches ahead of the slug validation the sdlc path applies.
	if choreName := strings.TrimSpace(s.Chore); choreName != "" {
		return m.nominateChore(choreName, s, stderr)
	}
	slug := strings.TrimSpace(s.Slug)
	// Dispatch on workflow before the slug check, the same way a chore
	// entry dispatches ahead of it.
	switch workflow := strings.TrimSpace(s.Workflow); workflow {
	case "", "sdlc":
		if slug == "" || run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: spawn: skipping entry with unusable slug %q\n", s.Slug)
			return ""
		}
	default:
		moePrintf(stderr, "pulse: spawn: entry %q asks for workflow %q — only sdlc is spawnable; skipping\n", slug, workflow)
		return ""
	}
	if !m.ensureLive(stderr) {
		return ""
	}
	if slugBaseMatches(m.live, slug) {
		// A live match is either an ordinary run already in flight, which
		// promoteOrSkip calls a duplicate, or a tagged idea, which it
		// promotes. What `design_only` means there depends on the idea's
		// own licence, so the refusal lives in promoteIfTaggedIdea, where
		// the idea has been identified.
		return m.promoteOrSkip(slug, s, stdout, stderr)
	}
	// A design-only spec buys a design turn with the brief it carries.
	// Without one the seed is a title and a why — the one-line idea
	// this rung exists to replace — and the turn would spend itself
	// re-deriving what the survey already knew. Checked below the
	// live-slug dispatch because only a fresh mint makes the spec the
	// seed: a tagged idea supplies its own brief on its canvas, and
	// every other live match is a duplicate skip.
	if s.DesignOnly && strings.TrimSpace(s.Design) == "" {
		moePrintf(stderr, "pulse: spawn: entry %q asks for design_only with no design body — the brief is the point; skipping\n", slug)
		return ""
	}
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = slug
	}
	md, err := runopen.Open(m.root, projectID, run.Options{
		IDBase:     slug,
		Workflow:   "sdlc",
		SeedDocs:   map[string]string{"design": spawnDesignSeed(title, s)},
		SpawnedBy:  spawnedBy,
		DesignOnly: s.DesignOnly,
		Trailers: trailers.Block{
			SpawnedBy: spawnedBy,
			Consent:   spawnConsent(spawnedBy),
		},
	})
	if err != nil {
		moePrintf(stderr, "pulse: spawn %q for %s: %v\n", slug, projectID, err)
		return ""
	}
	m.live = append(m.live, md.ID)
	kind := "fix run"
	if s.DesignOnly {
		kind = "design-only run"
	}
	moePrintf(stderr, "pulse: spawned %s %s/%s (%s)\n", kind, projectID, md.ID, title)
	return md.ID
}

// nominateChore opens a judged chore's run for a gate entry that says
// the chore's condition holds. It is a nomination, not a create: with
// the chore's run already open, openChoreInProcess refuses with the
// open run's id and the nomination maps onto it, so a `chore` spec
// written at a thread position places the existing run instead of
// dropping.
//
// The opened run is an ordinary chore run — the chore's own workflow,
// its `prompt.md` seed, the MoE-Chore trailer — so completion, cooldown
// and the open-run guard all keep working exactly as they do for a
// mechanical chore. What the survey replaced is only the due check;
// choreOpenNominated keeps the cooldown and refuses a mechanical chore
// outright.
//
// Warn-only throughout, like the rest of the gate: a refusal or a
// failure drops this entry and the sweep carries on.
func (m *pulseMinter) nominateChore(name string, s pulseRunSpec, stderr io.Writer) string {
	for _, extra := range []struct{ field, value string }{
		{"slug", s.Slug}, {"workflow", s.Workflow}, {"title", s.Title}, {"design", s.Design},
	} {
		if strings.TrimSpace(extra.value) != "" {
			moePrintf(stderr, "pulse: chore: ignoring %s on chore entry %q; a chore run's shape comes from its own definition\n", extra.field, name)
		}
	}
	if s.DesignOnly {
		moePrintf(stderr, "pulse: chore: ignoring design_only on chore entry %q; a chore run's shape comes from its own definition\n", name)
	}
	res, err := openChoreInProcess(m.root, m.projectID, name, choreOpenNominated)
	if err != nil {
		var notOpenable *choreNotOpenableError
		if errors.As(err, &notOpenable) && notOpenable.OpenRun != "" {
			// Logged rather than silent so the sweep output stays honest
			// about which run the nomination — and the thread position
			// holding it — landed on.
			moePrintf(stderr, "pulse: chore %s/%s already open as %s/%s — mapped (%s)\n",
				m.projectID, name, m.projectID, notOpenable.OpenRun, s.Why)
			return notOpenable.OpenRun
		}
		moePrintf(stderr, "pulse: chore %s/%s not opened — %v\n", m.projectID, name, err)
		return ""
	}
	moePrintf(stderr, "pulse: judged chore met — opened %s/%s (%s)\n", m.projectID, res.Metadata.ID, s.Why)
	return res.Metadata.ID
}

// promoteOrSkip handles the one live-slug match that isn't a duplicate:
// a tagged idea, which the pulse promotes through the same seam
// `--from-idea` uses. Everything else already has this work in flight
// and skips.
func (m *pulseMinter) promoteOrSkip(slug string, s pulseRunSpec, stdout, stderr io.Writer) string {
	id, dup := m.promoteIfTaggedIdea(slug, s, false /*atThread*/, stdout, stderr)
	if dup {
		moePrintf(stderr, "pulse: spawn: %s already has a live run for %q — skipping\n", m.projectID, slug)
	}
	return id
}

// promoteIfTaggedIdea promotes the tagged idea named by slug and returns
// the destination run's id, or "" when slug doesn't name one. It is the
// promotion seam both entries into the gate share: a spec whose slug
// collided with a live run (promoteOrSkip, above) and a thread's string
// entry naming a parked run (applyPulseGate) are the same question asked
// from two grammars, and a tagged idea is a promotable answer to both.
//
// dup reports the one case a caller has to speak to itself: slug names
// live work that isn't a lone idea. What that means depends on the
// grammar — a duplicate proposal to the spec path, an ordinary parked
// run to the thread path — so it's returned rather than warned about
// here. Every other refusal (an unreadable scan, an untagged idea, an
// unusable tag, a design-only idea at a thread position, a failed
// promotion) has already warned by the time this returns "".
//
// atThread says the caller is applyPulseGate's string-entry branch,
// which is a position a design-only root can't hold.
func (m *pulseMinter) promoteIfTaggedIdea(slug string, s pulseRunSpec, atThread bool, stdout, stderr io.Writer) (id string, dup bool) {
	projectID, pulseSlug := m.projectID, m.pulseSlug
	spawnedBy := projectID + "/" + pulseSlug

	matches, err := matchingLiveRuns(m.root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "pulse: spawn: scan live match for %s/%s: %v\n", projectID, slug, err)
		return "", false
	}
	if len(matches) != 1 || matches[0].Workflow != dash.IdeaWorkflow {
		return "", true
	}
	idea := matches[0]
	if idea.PromoteTo == "" {
		moePrintf(stderr, "pulse: spawn: idea %s/%s is untagged and requires operator triage — skipping\n", projectID, idea.ID)
		return "", false
	}
	wf, lookupErr := LookupWorkflow(idea.PromoteTo)
	if lookupErr != nil || !chainableWorkflow(idea.PromoteTo) || len(wf.Stages()) == 0 {
		moePrintf(stderr, "pulse: spawn: idea %s/%s has unusable workflow tag %q — skipping\n", projectID, idea.ID, idea.PromoteTo)
		return "", false
	}
	// The same rule a `design_only` spec gets at a thread position, on
	// the tag that carries the bit instead: the promoted root is
	// unsettled by definition until the operator advances it, so every
	// member behind it would strand. The next sweep may propose the idea
	// loose, which is where design-only work belongs.
	if idea.DesignOnly && atThread {
		moePrintf(stderr, "pulse: spawn: idea %s/%s is tagged design-only and was named at a thread position — a design-only root strands the runs behind it; put it in loose. Skipping\n", projectID, idea.ID)
		return "", false
	}
	// A spec that asks for design_only on a plain-tagged idea is trying
	// to narrow the operator's ship licence to a design turn; refuse it,
	// same as design_only on any other live slug. On an idea that
	// already carries the bit the two agree, and refusing would make the
	// operator's own licence unusable the moment a survey quotes the
	// board it reads "design only" from.
	if s.DesignOnly {
		if !idea.DesignOnly {
			moePrintf(stderr, "pulse: spawn: entry %q asks for design_only but the slug names tagged idea %s/%s — the tag is the operator's licence to spend; skipping\n", slug, projectID, idea.ID)
			return "", false
		}
		moePrintf(stderr, "pulse: spawn: ignoring design_only for tagged idea %s/%s; the tag carries it\n", projectID, idea.ID)
	}
	if strings.TrimSpace(s.Design) != "" {
		moePrintf(stderr, "pulse: spawn: ignoring design body for tagged idea %s/%s; the idea canvas is the seed\n", projectID, idea.ID)
	}
	promoted, promoteErr := runopen.Promote(m.root, projectID, idea.ID, runopen.PromoteOptions{
		Workflow:   idea.PromoteTo,
		FirstStage: wf.Stages()[0],
		SpawnedBy:  spawnedBy,
		Consent:    spawnConsent(spawnedBy),
		DesignOnly: idea.DesignOnly,
	}, stdout, stderr)
	if promoteErr != nil {
		moePrintf(stderr, "pulse: promote tagged idea %s/%s: %v\n", projectID, idea.ID, promoteErr)
		return "", false
	}
	if promoted.MarkErr != nil {
		moePrintf(stderr, "pulse: warning: promoted %s/%s but could not mark the idea: %v\n", projectID, promoted.Run.ID, promoted.MarkErr)
	}
	m.live = append(m.live, promoted.Run.ID)
	kind := idea.PromoteTo
	if idea.DesignOnly {
		kind += ", design only"
	}
	moePrintf(stderr, "pulse: promoted tagged idea %s/%s to %s run %s/%s\n", projectID, idea.ID, kind, projectID, promoted.Run.ID)
	return promoted.Run.ID, false
}

// ensureLive loads the dedupe set on first use, so a gate whose specs
// are all chore nominations or non-sdlc skips (or which has none at
// all) pays for no scan.
// A failed scan is remembered: without the dedupe set every sdlc mint
// would be unguarded, so the whole gate's sdlc half is refused rather
// than re-scanning per spec.
func (m *pulseMinter) ensureLive(stderr io.Writer) bool {
	if m.broken {
		return false
	}
	if m.scanned {
		return true
	}
	live, err := liveSlugs(m.root, m.projectID)
	if err != nil {
		moePrintf(stderr, "pulse: spawn: scan runs for %s: %v\n", m.projectID, err)
		m.broken = true
		return false
	}
	m.live, m.scanned = live, true
	return true
}

// matchingLiveRuns returns every live run derived from base. The pulse
// only promotes when the tagged idea is the sole match: a dated live
// destination beside it means a prior promotion already queued the work,
// even if marking the source idea failed.
func matchingLiveRuns(root, projectID, base string) ([]*run.Metadata, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var matches []*run.Metadata
	for _, md := range mds {
		if md.Project != projectID || (md.Status != run.StatusInProgress && md.Status != run.StatusPushed) {
			continue
		}
		if slugBaseMatches([]string{md.ID}, base) {
			matches = append(matches, md)
		}
	}
	return matches, nil
}

// spawnDesignSeed builds the design canvas body a spawned run opens
// with. The survey's own markdown is the body when it wrote one;
// otherwise the title and why are all there is, which is still a
// better starting point than an empty canvas.
func spawnDesignSeed(title string, s pulseRunSpec) string {
	body := strings.TrimSpace(s.Design)
	if body != "" {
		return body + "\n"
	}
	seed := "# " + title + "\n"
	if why := strings.TrimSpace(s.Why); why != "" {
		seed += "\n" + why + "\n"
	}
	return seed
}

// liveSlugs lists the project's live run slugs — the dedupe set the
// spawn guard checks against. Live means in progress or pushed and
// waiting on a human to merge: in both cases the fix is already in
// flight, and whatever it addresses stays broken until it lands.
// Merged, closed and promoted runs are out — a finding that survives a
// merge is a new finding, not a duplicate.
func liveSlugs(root, projectID string) ([]string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, md := range mds {
		if md.Project == projectID && (md.Status == run.StatusInProgress || md.Status == run.StatusPushed) {
			out = append(out, md.ID)
		}
	}
	return out, nil
}

// datedSlugSuffix matches what run.Options.IDBase appends to a slug base
// on collision: `-YYYY-MM-DD`, optionally `-N` for a same-day repeat.
var datedSlugSuffix = regexp.MustCompile(`^-\d{4}-\d{2}-\d{2}(-\d+)?$`)

// slugBaseMatches reports whether any slug in the set was derived from
// base — the bare base, or one of IDBase's dated forms. The spawn guard
// passes the live set; the close-time followup-claim check passes every
// run on record.
//
// Deliberately not a bare prefix match: `fix-ci` and `fix-ci-red-main`
// are different proposals, and a greedy prefix would silently skip the
// second whenever the first is live. Only a date-shaped remainder counts
// as "the harness already dated this base".
func slugBaseMatches(slugs []string, base string) bool {
	for _, slug := range slugs {
		if slug == base {
			return true
		}
		if rest, ok := strings.CutPrefix(slug, base); ok && datedSlugSuffix.MatchString(rest) {
			return true
		}
	}
	return false
}
