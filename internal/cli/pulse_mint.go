package cli

import (
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// applyPulseGate opens the runs the gate's `loose` list describes, each in
// the project its slug names, and returns the ids it minted keyed by the
// slug the survey wrote, so `next` can name a run this same gate opened.
//
// Warn-only throughout: a spec that can't be minted drops with a stderr
// line, and the report and filings are already durable.
func applyPulseGate(root, pulseSlug string, gate pulseGate, stdout, stderr io.Writer) map[string]string {
	minted := map[string]string{}
	minters := map[string]*pulseMinter{}
	spawnedBy := project.RootID + "/" + pulseSlug
	for _, spec := range gate.Loose {
		target := spec.Slug
		if strings.TrimSpace(spec.Chore) != "" {
			target = spec.Chore
		}
		projectID, bare, err := splitProjectRun(strings.TrimSpace(target))
		if err != nil {
			moePrintf(stderr, "pulse: spawn: entry %q must be <project>/<slug> — skipping\n", target)
			continue
		}
		if projectID == project.RootID {
			moePrintf(stderr, "pulse: spawn: entry %q names the root project, which has no source — skipping\n", target)
			continue
		}
		if err := requireProject(root, projectID); err != nil {
			moePrintf(stderr, "pulse: spawn: entry %q: %v — skipping\n", target, err)
			continue
		}
		m := minters[projectID]
		if m == nil {
			m = &pulseMinter{root: root, projectID: projectID, spawnedBy: spawnedBy}
			minters[projectID] = m
		}
		if strings.TrimSpace(spec.Chore) != "" {
			spec.Chore = bare
		} else {
			spec.Slug = bare
		}
		if id := m.mint(spec, stdout, stderr); id != "" {
			minted[projectID+"/"+bare] = id
		}
	}
	return minted
}

// sayWhyTaggedIdeaStayed names the live run that kept a tagged idea from
// promoting. promoteIfTaggedIdea leaves dup for its caller to speak to,
// and here a silent skip strands the operator's licence sweep after
// sweep. The sibling is usually the idea's own work (a hand-opened
// `moe sdlc new`, or a promotion whose idea mark failed), so the hint
// leads with closing the idea. It re-scans rather than widening the
// shared seam's signature for this one caller; a scan that errors or
// no longer finds a sibling only drops the name, and the next sweep
// asks again.
func sayWhyTaggedIdeaStayed(root, projectID, id string, stderr io.Writer) {
	var siblings []string
	matches, _ := matchingLiveRuns(root, projectID, id)
	for _, md := range matches {
		if md.ID != id {
			siblings = append(siblings, projectID+"/"+md.ID)
		}
	}
	if len(siblings) == 0 {
		moePrintf(stderr, "pulse: promote: tagged idea %s/%s not promoted — its slug is no longer a lone idea\n", projectID, id)
		return
	}
	moePrintf(stderr, "pulse: promote: tagged idea %s/%s not promoted — live run %s already holds its slug; `moe idea close %s/%s` if that run is its work, or close the run to let the idea promote\n",
		projectID, id, strings.Join(siblings, ", "), projectID, id)
}

// pulseMinter opens the runs one sweep's gate asks for. It holds the
// live-slug dedupe set across the whole gate, scanning once on first
// use, and claims each base it mints so a repeat proposal in the same
// gate is a skip rather than a dated sibling.
type pulseMinter struct {
	root      string
	projectID string
	// spawnedBy is the sweep's qualified run, the lineage every mint carries.
	spawnedBy string
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
	projectID, spawnedBy := m.projectID, m.spawnedBy

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
	id, dup := m.promoteIfTaggedIdea(slug, s, stdout, stderr)
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
func (m *pulseMinter) promoteIfTaggedIdea(slug string, s pulseRunSpec, stdout, stderr io.Writer) (id string, dup bool) {
	projectID, spawnedBy := m.projectID, m.spawnedBy

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
	if lookupErr != nil || !operatorCascades(idea.PromoteTo) || len(wf.Stages()) == 0 {
		moePrintf(stderr, "pulse: spawn: idea %s/%s has unusable workflow tag %q — skipping\n", projectID, idea.ID, idea.PromoteTo)
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
