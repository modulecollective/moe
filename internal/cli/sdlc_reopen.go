package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// runSDLCReopen opens a fresh sdlc run seeded by a prior run's design
// canvas. The prior run must be in a terminal status (closed, merged, or
// promoted) — reopening an in-progress run is a usage error, since
// "just keep working" is the right answer there.
//
// What carries over:
//   - The design canvas, byte-for-byte, seeded into the new run. If the
//     source's design canvas is missing or empty (operator opened, bailed,
//     closed without writing), the new run gets a short kickoff seed that
//     names the prior slug and prompts the operator for the goal of the
//     retake — the verb's slug-base + workspace inheritance is the value,
//     not the canvas carry-forward.
//   - Title and workspace inherited verbatim.
//
// What's left behind:
//   - Code-stage canvas (sandbox-specific by the time a run terminates).
//   - Document sessions (a reopen is a fresh conversation).
//
// The new slug is anchored to the prior slug's base — any trailing
// `-YYYY-MM-DD[-N]` from a dated suffix is stripped first — and
// re-suffixed with today's date via run.New's existing dated-collision
// path. Multiple reopens of the same topic on different days get their
// own date; same-day reopens get `-2`, `-3`.
//
// The open commit carries a MoE-Reopen-Of trailer pointing at the prior
// slug, which BuildJournalIndex picks up to populate
// JournalIndex.ReopenedFrom (new slug → prior slug). Dash uses that to
// decide which closed runs are still candidates for reopen.
func runSDLCReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc reopen <project>/<slug>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens a fresh sdlc run seeded with the prior run's design canvas.")
		moePrintln(stderr, "The prior run must be in a terminal status (closed, merged, or promoted);")
		moePrintln(stderr, "reopening an in-progress run is refused. The new slug anchors on the")
		moePrintln(stderr, "prior slug's base (date suffix stripped) and re-dates today.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, priorSlug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "sdlc reopen: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	prior, err := run.Load(root, projectID, priorSlug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "sdlc reopen: run not found: %s %s\n", projectID, priorSlug)
			return 1
		}
		moePrintf(stderr, "sdlc reopen: %v\n", err)
		return 1
	}
	if prior.Workflow != "sdlc" {
		moePrintf(stderr, "sdlc reopen: %s %s is a %s run, not sdlc\n", projectID, priorSlug, prior.Workflow)
		return 1
	}
	switch prior.Status {
	case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		// Terminal — proceed.
	case run.StatusInProgress:
		moePrintf(stderr, "sdlc reopen: %s %s is in_progress; just keep working\n", projectID, priorSlug)
		return 1
	case run.StatusPushed:
		moePrintf(stderr, "sdlc reopen: %s %s is pushed; resolve via GitHub + `moe sync` before reopening\n", projectID, priorSlug)
		return 1
	default:
		moePrintf(stderr, "sdlc reopen: %s %s has unexpected status %q\n", projectID, priorSlug, prior.Status)
		return 1
	}

	canvasRel := run.ContentPath(projectID, priorSlug, "design")
	canvasBody, err := os.ReadFile(filepath.Join(root, canvasRel))
	switch {
	case errors.Is(err, os.ErrNotExist):
		canvasBody = nil
	case err != nil:
		moePrintf(stderr, "sdlc reopen: read %s: %v\n", canvasRel, err)
		return 1
	}
	var designSeed string
	if len(canvasBody) == 0 {
		designSeed = renderEmptyReopenSeed(prior.Title, priorSlug)
	} else {
		designSeed = string(canvasBody)
	}

	opts := run.Options{
		Workflow:    prior.Workflow,
		IDBase:      stripDateSuffix(priorSlug),
		SeedDocs:    map[string]string{"design": designSeed},
		SubjectFrom: "reopen of " + priorSlug,
		Trailers:    trailers.Block{ReopenOf: priorSlug},
		Workspace:   prior.Workspace,
	}

	var md *run.Metadata
	err = withRepoLock(root, repolock.Options{
		Purpose: "run-new",
		Run:     projectID,
	}, func() error {
		m, err := run.New(root, projectID, prior.Title, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened run %s %s (reopen of %s)\n", md.Project, md.ID, priorSlug)
	return promptNextStage(root, md, "", stdout, stderr)
}

// renderEmptyReopenSeed produces the design-canvas kickoff for a reopen
// whose source had no design content. The seed names the prior slug
// (visible in the design agent's first turn via --append-system-prompt)
// and prompts the operator for the goal of the retake — strictly more
// useful than a blank canvas and avoids the empty-canvas invariant
// firing on open.
func renderEmptyReopenSeed(title, priorSlug string) string {
	return fmt.Sprintf(
		"# %s\n\n> Reopened from `%s`, which had no design content.\n> Operator: what's the goal of this retake?\n",
		title, priorSlug,
	)
}

// datedSuffixPattern matches a trailing `-YYYY-MM-DD` segment,
// optionally followed by `-N` from same-day collision suffixing. Strict
// 4-2-2 digit shape so an unrelated `-N` suffix on a non-dated slug
// (e.g., `foo-2`) isn't mistaken for a date.
var datedSuffixPattern = regexp.MustCompile(`-[0-9]{4}-[0-9]{2}-[0-9]{2}(?:-[0-9]+)?$`)

// stripDateSuffix returns slug with any trailing `-YYYY-MM-DD[-N]`
// removed. Used by reopen so a series of reopens against a topic
// re-dates from the same stable base instead of stacking dates
// (`foo-2025-12-01-2026-05-12`). A slug without a dated suffix is
// returned unchanged.
func stripDateSuffix(slug string) string {
	return datedSuffixPattern.ReplaceAllString(slug, "")
}
