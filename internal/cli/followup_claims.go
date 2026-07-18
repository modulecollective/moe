package cli

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// A canvas that says it filed a followup is making a claim about a file
// on disk. When the claim is false the item silently evaporates — and
// because the *canvas* still reports it, a later reader (a pulse
// sweeping the settled record, an operator skimming a closed run)
// believes the work is tracked. The 2026-07-18 chains lost an item
// exactly this way: a design canvas said it "filed as followup
// `pulse-tail-stale-binary`", the entry never landed, and the next
// pulse refiled the whole observation out of fidelity to that claim.
//
// So at close we scan the run's canvases for followup claims and warn
// about any that name nothing on disk. Warn, never block: the rules are
// prose regexes, and a false positive that wedges a cascade would cost
// far more than the miss it prevents.

// followupClaimVerbRE gates the generic rule. The word "followup" alone
// is everywhere — skill prose, stage guidance, this run's own design.
// A *claim* is past-tense: something was filed. Imperative and
// infinitive phrasing ("leave a followup via the `moe-bureaucracy`
// skill") is instruction, not a claim, and stays out.
var followupClaimVerbRE = regexp.MustCompile(`(?i)\b(filed|logged|captured|recorded|raised|left|opened)\b`)

// followupWordRE matches either spelling the codebase uses.
var followupWordRE = regexp.MustCompile(`(?i)follow-?ups?\b`)

// canvasSlugRE plucks backticked slugs from a line. Shape matches
// followupOpenRE's slug group — bare or `<project>/`-prefixed — so a
// slug this finds is one the harvest grammar would have accepted, with
// one narrowing: the bare-slug half must contain a hyphen. Single-word
// backticks on a claim-shaped line are overwhelmingly commands and tool
// names (`vet`, `new`, `pgrep`), and against the bureaucracy's whole
// history that one condition removed five of the six false positives at
// the cost of a hypothetical single-word followup slug, of which the
// corpus contains none.
var canvasSlugRE = regexp.MustCompile("`([a-z0-9][a-z0-9-]*/)?([a-z0-9][a-z0-9-]*-[a-z0-9][a-z0-9-]*)`")

// pulseFilingItemRE matches a `## New filings` list item: a leading
// backticked slug, which is the grammar pulse.md teaches for that
// section. The generic rule misses these entirely — the lines don't
// contain the word "followup" — and a pulse report that lists a filing
// it never made feeds the same refile loop on auto-close, where no
// editor pop reviews anything.
var pulseFilingItemRE = regexp.MustCompile("^\\s*-\\s+`([a-z0-9][a-z0-9-]*(?:/[a-z0-9][a-z0-9-]*)?)`")

// newFilingsHeadingRE opens the pulse-specific section; any later `##`
// heading closes it.
var newFilingsHeadingRE = regexp.MustCompile(`(?i)^##\s+New filings\s*$`)

// followupFiledSlugRE plucks the slug from a followups.md entry in
// either box state. parseFollowups deliberately returns only unchecked
// entries (checked ones are the audit trail of past harvests), but a
// claim is satisfied by either: `- [x]` means the item was filed *and*
// already promoted to an idea. This scan is also lenient where
// parseFollowups is total — a malformed followups.md must not turn an
// advisory warning into a close-time error.
var followupFiledSlugRE = regexp.MustCompile("^\\s*-\\s+\\[[ xX]\\]\\s+`([a-z0-9][a-z0-9-]*(?:/[a-z0-9][a-z0-9-]*)?)`")

// followupClaimSlugs returns the followup slugs a canvas claims, in
// first-appearance order, deduped. Two rules, both cheap:
//
//   - generic: a line that names a followup in the past tense, with a
//     backticked slug on it;
//   - pulse: a leading backticked slug under `## New filings`.
func followupClaimSlugs(canvas []byte) []string {
	var out []string
	seen := map[string]bool{}
	add := func(slug string) {
		if !seen[slug] {
			seen[slug] = true
			out = append(out, slug)
		}
	}

	inNewFilings := false
	for _, line := range strings.Split(string(canvas), "\n") {
		if strings.HasPrefix(line, "##") {
			inNewFilings = newFilingsHeadingRE.MatchString(line)
		}
		if inNewFilings {
			if m := pulseFilingItemRE.FindStringSubmatch(line); m != nil {
				add(m[1])
				continue
			}
		}
		if followupWordRE.MatchString(line) && followupClaimVerbRE.MatchString(line) {
			for _, m := range canvasSlugRE.FindAllStringSubmatch(line, -1) {
				add(m[1] + m[2])
			}
		}
	}
	return out
}

// filedFollowupSlugs reads the run's followups.md and returns the slugs
// it records, checked or unchecked. A missing file is not an error —
// most runs file nothing.
func filedFollowupSlugs(root, projectID, runID string) map[string]bool {
	filed := map[string]bool{}
	body, err := os.ReadFile(filepath.Join(root, run.FollowupsPath(projectID, runID)))
	if err != nil {
		return filed
	}
	for _, line := range strings.Split(string(body), "\n") {
		if m := followupFiledSlugRE.FindStringSubmatch(line); m != nil {
			filed[m[1]] = true
		}
	}
	return filed
}

// allRunSlugs lists every run slug on record, any project, any status —
// the second half of the claim check's double condition. Prose
// *discussing* a real followup names a slug that became a run; only a
// lie names a slug that exists nowhere.
func allRunSlugs(root string) []string {
	mds, err := run.Scan(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(mds))
	for _, md := range mds {
		out = append(out, md.ID)
	}
	return out
}

// unverifiedFollowupClaims returns, per document, the claimed slugs
// that name neither a followups.md entry nor an existing run. Docs are
// walked in sorted order so the warning stream is deterministic.
func unverifiedFollowupClaims(root string, md *run.Metadata) map[string][]string {
	filed := filedFollowupSlugs(root, md.Project, md.ID)
	runSlugs := allRunSlugs(root)

	out := map[string][]string{}
	for _, docID := range sortedDocIDs(md) {
		canvas, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, docID)))
		if err != nil {
			continue
		}
		for _, slug := range followupClaimSlugs(canvas) {
			// A cross-project claim (`claudia/foo`) is matched on its
			// bare slug: run IDs are not project-qualified on disk.
			base := slug
			if i := strings.IndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			if filed[slug] || slugBaseMatches(runSlugs, base) {
				continue
			}
			out[docID] = append(out[docID], slug)
		}
	}
	return out
}

// sortedDocIDs is the deterministic document walk closeRunInProcess's
// canvas seal already does, hoisted so both callers share it.
func sortedDocIDs(md *run.Metadata) []string {
	ids := make([]string, 0, len(md.Documents))
	for docID := range md.Documents {
		ids = append(ids, docID)
	}
	sort.Strings(ids)
	return ids
}

// warnUnverifiedFollowupClaims writes the advisory to stderr. Called
// after the close commit is durable — this never changes the close's
// outcome, it just puts the discrepancy where the operator is already
// watching the cascade tail.
func warnUnverifiedFollowupClaims(root string, md *run.Metadata, stderr io.Writer) {
	claims := unverifiedFollowupClaims(root, md)
	for _, docID := range sortedDocIDs(md) {
		for _, slug := range claims[docID] {
			moePrintf(stderr,
				"close: canvas %s claims followup `%s` that was never filed — file it or fix the canvas\n",
				docID, slug)
		}
	}
}
