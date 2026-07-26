package cli

import (
	"io"

	"github.com/modulecollective/moe/internal/run"
)

// The interactive stage verbs (`moe sdlc design/code/review/test
// <p>/<slug>`) are the door an operator walks through to pick a run back
// up, and until this guard they walked onto terminal runs too. Nothing
// flipped the status, so the dash kept the run in COMPLETED while agents
// committed turns onto it, and the work dead-ended at `moe sdlc push`'s
// "already merged" short-circuit. The cascade-flag legs have refused
// terminal runs since resolveAndGuardForCascade landed
// (stage_verb.go); this is the same fence on the no-flag leg, one
// gesture friendlier — on a tty it offers the forward instead of only
// naming it.
//
// `pushed` stays allowed. Iterating against PR feedback before the merge
// lands is a real flow: push on a pushed run re-pushes the same PR, and
// the dash renders pushed as ACTIVE. Only merged/closed/promoted are the
// zombie class.

// resolveSDLCReentry resolves a typed <project>/<run> for an interactive
// sdlc stage verb and guards the resolved run's status. It layers onto
// resolveSDLCRunSlug (whose lineage walk only fires when the typed slug
// misses on disk) the case that walk can't see: the slug exists, loads
// fine, and is terminal.
//
// On a terminal run:
//
//   - exactly one live descendant (a reopen/promotion chain link that
//     loads and isn't itself terminal) — prompt `did you mean
//     <descendant>? [Y/n]` on a tty and continue the typed stage there;
//     without a tty, refuse with a `hint:` line naming it. A second
//     reopen would be the wrong gesture when the chain is already
//     extended.
//   - more than one live descendant — refuse and list them, no default.
//     Same shape resolveSDLCRunSlug uses for an ambiguous lineage: the
//     operator picks.
//   - no live descendant — prompt `reopen it as a fresh run? [Y/n]` on a
//     tty; Y mints the reopen (the same mint `moe sdlc reopen` runs, at
//     its inherit-workspace-and-agent defaults) and returns the fresh
//     slug so the typed stage opens there. Without a tty, refuse with
//     the `moe sdlc reopen` hint.
//
// Returns the run id the caller should open, and a process exit code (0
// to proceed; non-zero with stderr already written).
//
// The guard is idempotent: what it returns is always a non-terminal run,
// so re-running it on the resolved id is a pass-through. That's what
// lets runStageVerb call it ahead of the --agent persist without the
// downstream opener's own resolution re-prompting.
func resolveSDLCReentry(verb, projectID, runID string, stdout, stderr io.Writer) (string, int) {
	return resolveSDLCReentryWithMode(verb, projectID, runID, stdinIsTerminal(), stdout, stderr)
}

// resolveSDLCReentryWithMode is the testable seam under
// resolveSDLCReentry — tty is what stdinIsTerminal() returned, threaded
// through so tests drive the prompt path and the refusal path without
// faking os.Stdin's mode bits. Mirrors
// resolveSDLCRunSlugWithMode's split for the same reason.
func resolveSDLCReentryWithMode(verb, projectID, runID string, tty bool, stdout, stderr io.Writer) (string, int) {
	resolved, code := resolveSDLCRunSlugWithMode(verb, projectID, runID, tty, stdout, stderr)
	if code != 0 {
		return "", code
	}
	runID = resolved

	root, err := findRoot(stderr)
	if err != nil {
		return "", 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		// Terminal — guard below.
	default:
		return runID, 0
	}

	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", 1
	}
	live := liveSDLCDescendants(root, idx, projectID, runID)

	refuseForward := func(slug string) {
		moePrintf(stderr, "%s: %s/%s is %s; %s carries it forward\n", verb, projectID, runID, md.Status, slug)
		moePrintf(stderr, "hint: moe %s %s/%s\n", verb, projectID, slug)
	}
	refuseReopen := func() {
		moePrintf(stderr, "%s: %s/%s is %s; reopen it to keep iterating\n", verb, projectID, runID, md.Status)
		moePrintf(stderr, "hint: moe sdlc reopen %s/%s\n", projectID, runID)
	}

	switch len(live) {
	case 0:
		if !tty {
			refuseReopen()
			return "", 1
		}
		moePrintf(stdout, "%s: %s/%s is %s — reopen it as a fresh run? [Y/n] ", verb, projectID, runID, md.Status)
		accepted, code := readChainAccept(stderr)
		if code != 0 {
			return "", code
		}
		if !accepted {
			refuseReopen()
			return "", 1
		}
		fresh, code := mintSDLCReopen(verb, root, md, md.Workspace, md.Agent, stdout, stderr)
		if code != 0 {
			return "", code
		}
		return fresh.ID, 0
	case 1:
		slug := live[0]
		if !tty {
			refuseForward(slug)
			return "", 1
		}
		moePrintf(stdout, "%s: %s/%s is %s — did you mean %s? [Y/n] ", verb, projectID, runID, md.Status, slug)
		accepted, code := readChainAccept(stderr)
		if code != 0 {
			return "", code
		}
		if !accepted {
			refuseForward(slug)
			return "", 1
		}
		// Recurse so a descendant that has itself gone terminal since the
		// journal was indexed lands on the next link rather than being
		// opened blind.
		return resolveSDLCReentryWithMode(verb, projectID, slug, tty, stdout, stderr)
	default:
		moePrintf(stderr, "%s: %s/%s is %s; its chain has more than one live run\n", verb, projectID, runID, md.Status)
		moePrintf(stderr, "did you mean one of:\n")
		for _, slug := range live {
			moePrintf(stderr, "  moe %s %s/%s\n", verb, projectID, slug)
		}
		return "", 1
	}
}

// liveSDLCDescendants narrows findChainedDescendants to the links an
// operator could actually open: the run dir loads, the run is sdlc, and
// its status isn't terminal. Order is inherited from the walk —
// most-recent journal activity first.
//
// A terminal descendant is skipped rather than offered: the walk is
// transitive, so if that link was itself reopened its live successor is
// already in the list, and if it wasn't, offering it would just hit this
// same guard one hop along.
func liveSDLCDescendants(root string, idx *run.JournalIndex, projectID, slug string) []string {
	var out []string
	for _, d := range findChainedDescendants(idx, projectID, slug) {
		md, err := run.Load(root, projectID, d.slug)
		if err != nil || md.Workflow != "sdlc" {
			continue
		}
		switch md.Status {
		case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
			continue
		}
		out = append(out, d.slug)
	}
	return out
}
