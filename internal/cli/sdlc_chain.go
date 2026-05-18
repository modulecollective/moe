package cli

import (
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// `moe sdlc <verb> <p> <slug>` accepts a literal slug — same contract
// every other run-taking verb honours. When `<slug>` misses on disk
// and the journal records a promotion or reopen chain rooted at it,
// resolveSDLCRunSlug offers the live descendant(s) instead of bouncing
// the operator back to the dash for a slug lookup. The literal-slug
// contract still wins: descendants only surface on miss, and an
// interactive prompt asks consent before the verb re-runs against the
// dated slug. Non-interactive callers see the standard not-found error
// with an inline hint.

// chainedDescendant carries one resolved descendant of a typed slug —
// the slug that exists today, plus its last-activity timestamp from
// the journal index. lastActivity is the sort key for the multi-
// descendant case (most-recent first); zero time sorts last.
type chainedDescendant struct {
	slug         string
	lastActivity time.Time
}

// findChainedDescendants walks the journal index from typedSlug to
// every reachable descendant via promotion (idea → sdlc) and reopen
// (sdlc → sdlc) trailers. Returns descendants sorted by last journal
// activity, most-recent first.
//
// Walk is breadth-first over two index maps:
//
//   - PromotedTo[X] = "<dest-project>/<dest-slug>" — set on an idea's
//     promote commit. Idea promotion stays in the same project, so
//     the dest-project field is informational; we still parse it and
//     drop entries whose project doesn't match projectID, in case a
//     future promote variant crosses projects.
//   - ReopenedFrom[K] = priorSlug — set on a reopen's open commit.
//     Reopens stay in the same project; the index doesn't carry K's
//     project, so cross-project slug collisions could in principle
//     leak through. resolveSDLCRunSlug verifies each candidate via
//     run.Load(projectID, slug) before offering it, so a phantom
//     descendant gets dropped by the existence check downstream.
//
// Cycles in PromotedTo+ReopenedFrom would be a journal-shape bug
// (promote and reopen are linear, descendant-only operations), but
// the seen-set guards the walk anyway so a corrupt index can't spin.
func findChainedDescendants(idx *run.JournalIndex, projectID, typedSlug string) []chainedDescendant {
	if idx == nil {
		return nil
	}
	seen := map[string]bool{typedSlug: true}
	queue := []string{typedSlug}
	var out []chainedDescendant
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if pt, ok := idx.PromotedTo[cur]; ok {
			if proj, slug, parsed := splitPromotedTo(pt); parsed && proj == projectID && !seen[slug] {
				seen[slug] = true
				out = append(out, chainedDescendant{slug: slug, lastActivity: idx.LastActivity[slug]})
				queue = append(queue, slug)
			}
		}
		for child, prior := range idx.ReopenedFrom {
			if prior != cur || seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, chainedDescendant{slug: child, lastActivity: idx.LastActivity[child]})
			queue = append(queue, child)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].lastActivity.After(out[j].lastActivity)
	})
	return out
}

// splitPromotedTo parses a MoE-Promoted-To trailer value (the wire
// shape is `<project>/<run-id>`). Returns parsed=false for any value
// that does not contain exactly one `/`, since today's writer
// (markIdeaPromoted) emits that shape and a malformed value isn't
// worth guessing at.
func splitPromotedTo(v string) (project, slug string, parsed bool) {
	i := strings.IndexByte(v, '/')
	if i <= 0 || i == len(v)-1 {
		return "", "", false
	}
	if strings.IndexByte(v[i+1:], '/') >= 0 {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

// resolveSDLCRunSlug is the seam every sdlc verb passes <project>
// <run> through. On exact match it returns the runID unchanged.
// On not-found it looks for a promotion/reopen chain rooted at the
// typed slug and:
//
//   - 0 descendants — prints the standard not-found error and returns
//     a non-zero exit code.
//   - 1 descendant, interactive stdin — prompts `did you mean
//     <descendant>? [Y/n]`. Y / Enter returns the descendant slug; N
//     declines and returns the standard not-found error.
//   - 1 descendant, non-interactive stdin — prints the standard
//     not-found error with a `hint:` line carrying the suggested
//     invocation. Returns non-zero.
//   - many descendants — prints the standard not-found error with
//     a `did you mean one of:` list, no prompt and no default (operator
//     picks and re-types). Returns non-zero.
//
// The hint and the multi-descendant list both render the suggested
// invocation as `moe <verb> <project> <descendant>`, where verb is
// the sub-command label the caller passes in (e.g. "sdlc code"). Verb
// flags like `--pr` or `--no-edit` don't carry through — the operator
// re-types if they need the flag, the hint stays minimal and
// unambiguous.
//
// verb appears in the error preamble too (`<verb>: run not found: ...`)
// so a misaimed slug surfaces inside the command the operator just
// typed instead of behind a bare "run not found".
func resolveSDLCRunSlug(verb, projectID, runID string, stdout, stderr io.Writer) (string, int) {
	return resolveSDLCRunSlugWithMode(verb, projectID, runID, stdinIsTerminal(), stdout, stderr)
}

// resolveSDLCRunSlugWithMode is the testable seam under
// resolveSDLCRunSlug — tty is what stdinIsTerminal() returned. Split
// so tests can drive both the prompt path (tty=true) and the
// no-tty error+hint path without faking os.Stdin's mode bits.
func resolveSDLCRunSlugWithMode(verb, projectID, runID string, tty bool, stdout, stderr io.Writer) (string, int) {
	root, err := findRoot(stderr)
	if err != nil {
		return "", 1
	}
	md, err := run.Load(root, projectID, runID)
	switch {
	case err == nil && md.Workflow == "sdlc":
		// Live sdlc handle — downstream verbs do their own status /
		// canvas guards from here. Pass through verbatim.
		return runID, 0
	case err == nil && md.Workflow != "sdlc":
		// Idea-rooted lineage is the primary worked example: typed
		// slug is the promoted idea (workflow=idea), the natural
		// reach is its sdlc descendant. Fall through to the descendant
		// walk; if nothing's there, the wrong-workflow case surfaces
		// as a clean not-found below rather than the verb-specific
		// "is a <wf> run, not sdlc" message — same standard error a
		// truly-missing slug gets, with the lineage hint when one
		// exists.
	case errors.Is(err, run.ErrRunNotFound):
		// Fall through to descendant walk.
	default:
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", 1
	}

	idx, ierr := run.BuildJournalIndex(root)
	if ierr != nil {
		moePrintf(stderr, "%s: %v\n", verb, ierr)
		return "", 1
	}
	candidates := findChainedDescendants(idx, projectID, runID)
	// Drop phantoms whose run dir is gone (cross-project slug collision
	// in ReopenedFrom, or a run dir that was rm'd without a status flip
	// — the journal still carries the trailer but the run isn't
	// addressable). run.Load is the same probe every downstream verb
	// performs, so passing this filter is what makes the descendant
	// safe to re-invoke against.
	descendants := candidates[:0]
	for _, d := range candidates {
		if _, err := run.Load(root, projectID, d.slug); err == nil {
			descendants = append(descendants, d)
		}
	}

	notFound := func() {
		if md != nil && md.Workflow != "" && md.Workflow != "sdlc" {
			// Typed slug exists but in another workflow (today: an idea
			// that was never promoted). "run not found" would mislead;
			// say what's actually wrong.
			moePrintf(stderr, "%s: %s %s is a %s run, not sdlc\n", verb, projectID, runID, md.Workflow)
			return
		}
		moePrintf(stderr, "%s: run not found: %s %s\n", verb, projectID, runID)
	}

	switch len(descendants) {
	case 0:
		notFound()
		return "", 1
	case 1:
		d := descendants[0]
		if !tty {
			notFound()
			moePrintf(stderr, "hint: moe %s %s %s\n", verb, projectID, d.slug)
			return "", 1
		}
		moePrintf(stdout, "%s: %s %s not found — did you mean %s? [Y/n] ", verb, projectID, runID, d.slug)
		accepted, code := readChainAccept(stderr)
		if code != 0 {
			return "", code
		}
		if !accepted {
			notFound()
			return "", 1
		}
		// Recurse: if the operator-accepted descendant is itself a
		// terminal that has a live successor, re-prompt against the
		// next link. Today the chain is shallow (idea → dated sdlc),
		// but recursion keeps the resolver honest if a future trailer
		// extends the lineage.
		return resolveSDLCRunSlugWithMode(verb, projectID, d.slug, tty, stdout, stderr)
	default:
		notFound()
		moePrintf(stderr, "did you mean one of:\n")
		for _, d := range descendants {
			moePrintf(stderr, "  moe %s %s %s\n", verb, projectID, d.slug)
		}
		return "", 1
	}
}

// readChainAccept reads a Y/n answer from the shared stdin buffer.
// Default is Y (blank line accepts) per the design's
// `[Y/n]` legend; anything starting with `n` declines; SIGINT and
// EOF decline as well — the safe default at a "did you mean" prompt
// is to bail with the original not-found error, not to redirect
// silently. Returns (accepted, exitCode); a non-zero exitCode means
// the read itself failed and the caller should surface the failure.
func readChainAccept(stderr io.Writer) (bool, int) {
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		return false, 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return false, 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" || strings.HasPrefix(answer, "y") {
		return true, 0
	}
	return false, 0
}
