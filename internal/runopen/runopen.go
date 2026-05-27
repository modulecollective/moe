// Package runopen factors the "open a new run" ceremony out of the
// CLI verb so non-CLI callers (notably `moe serve`) can open runs
// in-process instead of spawning `moe sdlc new` and scraping its
// stdout to discover the slug.
//
// Two callers today:
//
//   - `moe sdlc new` (and other workflows' `new` facade) — uses Open
//     directly, and Promote when --from-idea is set. The thin verb
//     wrapper still parses flags, prints `opened run …`, and falls
//     through to promptNextStage.
//
//   - `moe serve` — calls Open / Promote from its HTTP handlers, then
//     spawns `moe sdlc design <p>/<slug>` to host the agent session.
//     Knowing the slug synchronously is what lets serve drop the
//     `:promoting` placeholder + `opened run …` regex it used before.
//
// Open, Promote, CloseIdea, and ReopenIdea take the repolock
// around their mutations so concurrent invocations do not clobber
// each other.s git index.
package runopen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// ideaDocID is the document id for the idea workflow's sole canvas
// stage. Mirrors internal/cli/idea.go's private constant; duplicated
// here so runopen avoids depending on internal/cli (which depends on
// everything).
const ideaDocID = "idea"

// ErrNotIdea is returned when the slug names a run that
// is not an in-progress idea — either a different workflow, or an
// idea that has already been promoted/closed. Defence in depth: serve
// handlers gate on the same check before rendering actions, and this
// catches replayed POSTs landing on a now-terminal idea.
var ErrNotIdea = errors.New("runopen: not an in-progress idea (workflow!=idea or status!=in_progress)")

// ErrNotReopenableIdea is returned by ReopenIdea when the slug is not
// an idea that can be flipped back to in_progress. The wrapped error
// text names the specific status or destination-state problem.
var ErrNotReopenableIdea = errors.New("runopen: not a reopenable idea")

// PromoteOptions configures Promote. The destination run's workflow,
// workspace, and agent are caller-provided; the first-stage doc id is
// where the source idea's canvas lands as a seed.
type PromoteOptions struct {
	Workflow   string // destination workflow (e.g. "sdlc")
	FirstStage string // destination workflow's first-stage doc id (gets the seeded idea canvas)
	Workspace  string // optional workspace binding for the destination
	Agent      string // optional agent override persisted to run.json
}

// Promoted is Promote's result. Run is the destination run's
// metadata. MarkErr is non-nil when the destination opened but the
// source idea's status bump (or its commit) failed; the caller should
// surface this as a warning, not a fatal — the destination is fully
// open and carries a MoE-Idea trailer pointing back, so the
// transition stays greppable.
type Promoted struct {
	Run     *run.Metadata
	MarkErr error
}

// Open opens a fresh run under repolock. opts.Workflow must be set;
// opts.ID names the slug (collisions fail loud) or opts.IDBase
// supplies a base for date-suffixed collision handling. See
// run.New for the full opts contract.
func Open(root, projectID string, opts run.Options) (*run.Metadata, error) {
	runRef := projectID
	if opts.ID != "" {
		runRef = projectID + "/" + opts.ID
	}
	var md *run.Metadata
	err := withRepoLock(root, repolock.Options{
		Purpose: "run-new",
		Run:     runRef,
	}, func() error {
		m, err := run.New(root, projectID, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return md, nil
}

// Promote opens a destination run seeded from an idea, then marks the
// source idea promoted in a separate commit. Two commits total — one
// for the open, one for the status bump — keeps git history honest
// (one event per commit).
//
// Fails loud if ideaSlug doesn't name an in-progress idea run in
// projectID. Once the destination opens, a markIdeaPromoted failure
// is recorded on Promoted.MarkErr rather than rolled back — the
// destination's MoE-Idea trailer still records the source, so the
// promotion stays greppable, and the operator can re-mark the idea
// by hand if needed.
func Promote(root, projectID, ideaSlug string, opts PromoteOptions) (Promoted, error) {
	if opts.Workflow == "" {
		return Promoted{}, errors.New("runopen: PromoteOptions.Workflow is required")
	}
	if opts.FirstStage == "" {
		return Promoted{}, errors.New("runopen: PromoteOptions.FirstStage is required")
	}
	if opts.Workflow == dash.IdeaWorkflow {
		return Promoted{}, errors.New("runopen: cannot promote an idea into another idea run")
	}

	src, seed, err := loadIdeaForPromote(root, projectID, ideaSlug)
	if err != nil {
		return Promoted{}, err
	}

	runOpts := run.Options{
		Workflow:    opts.Workflow,
		Workspace:   opts.Workspace,
		Agent:       opts.Agent,
		IDBase:      ideaSlug,
		SeedDocs:    map[string]string{opts.FirstStage: seed},
		SubjectFrom: "idea " + ideaSlug,
		Trailers:    trailers.Block{Idea: ideaSlug},
	}

	md, err := Open(root, projectID, runOpts)
	if err != nil {
		return Promoted{}, err
	}
	markErr := markIdeaPromoted(root, src, md)
	return Promoted{Run: md, MarkErr: markErr}, nil
}

// loadIdeaForPromote returns the source idea run and its canvas body
// to seed the destination workflow's first-stage doc with. The canvas
// is the full file — H1 included — so the agent that opens the first
// stage starts on a canvas that already names what it's about.
func loadIdeaForPromote(root, projectID, slug string) (*run.Metadata, string, error) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return nil, "", fmt.Errorf("--from-idea: run %s/%s does not exist", projectID, slug)
		}
		return nil, "", fmt.Errorf("--from-idea: %w", err)
	}
	if md.Workflow != dash.IdeaWorkflow {
		return nil, "", fmt.Errorf("--from-idea: run %s/%s is a %s run, not an idea", projectID, slug, md.Workflow)
	}
	if md.Status != run.StatusInProgress {
		return nil, "", fmt.Errorf("--from-idea: idea %s/%s is already %s", projectID, slug, md.Status)
	}
	canvasRel := run.ContentPath(projectID, slug, ideaDocID)
	b, err := os.ReadFile(filepath.Join(root, canvasRel))
	if err != nil {
		return nil, "", fmt.Errorf("--from-idea: read %s: %w", canvasRel, err)
	}
	return md, string(b), nil
}

// markIdeaPromoted bumps the source idea run's status to
// StatusPromoted and commits the transition with a MoE-Promoted-To
// trailer pointing at the destination run. Separate commit from the
// destination's open: two short commits keep git history honest (one
// event per commit).
func markIdeaPromoted(root string, md *run.Metadata, dest *run.Metadata) error {
	md.Status = run.StatusPromoted
	runJSONRel := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf("Promote idea %s/%s → %s/%s\n\n", md.Project, md.ID, dest.Project, dest.ID) +
		trailers.Block{
			Run:        md.ID,
			Project:    md.Project,
			Workflow:   dash.IdeaWorkflow,
			PromotedTo: dest.Project + "/" + dest.ID,
		}.String()
	return withRepoLock(root, repolock.Options{
		Purpose: "idea-promote",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}

// CloseIdea flips an in-progress idea run to closed and commits only
// its run.json with the standard idea close trailer block.
func CloseIdea(root, projectID, slug string) error {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	if md.Workflow != dash.IdeaWorkflow || md.Status != run.StatusInProgress {
		return ErrNotIdea
	}

	runJSONRel := filepath.Join(run.Dir(projectID, slug), "run.json")
	msg := fmt.Sprintf("Close idea %s/%s\n\n", projectID, slug) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: dash.IdeaWorkflow,
		}.String()
	return withRepoLock(root, repolock.Options{
		Purpose: "idea-close",
		Run:     projectID + "/" + slug,
	}, func() error {
		md.Status = run.StatusClosed
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}

// ReopenIdea flips a closed idea back to in_progress. For promoted
// ideas, it preserves the old CLI policy: the promoted destination must
// resolve through MoE-Promoted-To and be closed, otherwise reopening
// would create two live owners of the same intent or resurrect shipped
// work.
func ReopenIdea(root, projectID, slug string) error {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	if md.Workflow != dash.IdeaWorkflow {
		return fmt.Errorf("%w: run %s/%s is a %s run, not an idea", ErrNotReopenableIdea, projectID, slug, md.Workflow)
	}
	switch md.Status {
	case run.StatusClosed:
		// Plain closed idea: direct reopen.
	case run.StatusPromoted:
		if err := verifyPromotedDestinationClosed(root, projectID, slug); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: idea %s/%s is %s, not closed or promoted", ErrNotReopenableIdea, projectID, slug, md.Status)
	}

	runJSONRel := filepath.Join(run.Dir(projectID, slug), "run.json")
	msg := fmt.Sprintf("Reopen idea %s/%s\n\n", projectID, slug) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: dash.IdeaWorkflow,
		}.String()
	return withRepoLock(root, repolock.Options{
		Purpose: "idea-reopen",
		Run:     projectID + "/" + slug,
	}, func() error {
		md.Status = run.StatusInProgress
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}

func verifyPromotedDestinationClosed(root, projectID, slug string) error {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return fmt.Errorf("idea reopen: %w", err)
	}
	destValue := idx.PromotedTo[slug]
	if destValue == "" {
		return fmt.Errorf("%w: idea %s/%s is promoted but has no MoE-Promoted-To trailer on record; cannot resolve destination", ErrNotReopenableIdea, projectID, slug)
	}
	destProject, destSlug, ok := splitPromotedTo(destValue)
	if !ok {
		return fmt.Errorf("%w: idea %s/%s has malformed MoE-Promoted-To trailer %q", ErrNotReopenableIdea, projectID, slug, destValue)
	}
	dest, err := run.Load(root, destProject, destSlug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return fmt.Errorf("%w: idea %s/%s points at %s/%s but that run is gone; cannot verify destination status", ErrNotReopenableIdea, projectID, slug, destProject, destSlug)
		}
		return fmt.Errorf("idea reopen: %w", err)
	}
	switch dest.Status {
	case run.StatusClosed:
		return nil
	case run.StatusInProgress:
		return fmt.Errorf("%w: idea %s/%s destination %s/%s is in_progress; just keep working on it", ErrNotReopenableIdea, projectID, slug, destProject, destSlug)
	case run.StatusPushed:
		return fmt.Errorf("%w: idea %s/%s destination %s/%s is pushed; resolve via GitHub + `moe sync` before reopening", ErrNotReopenableIdea, projectID, slug, destProject, destSlug)
	case run.StatusMerged:
		return fmt.Errorf("%w: idea %s/%s already shipped via %s/%s; run `moe idea new` for a fresh capture instead", ErrNotReopenableIdea, projectID, slug, destProject, destSlug)
	case run.StatusPromoted:
		return fmt.Errorf("%w: idea %s/%s destination %s/%s is itself promoted; resolve that chain before reopening", ErrNotReopenableIdea, projectID, slug, destProject, destSlug)
	default:
		return fmt.Errorf("%w: idea %s/%s destination %s/%s has unexpected status %q", ErrNotReopenableIdea, projectID, slug, destProject, destSlug, dest.Status)
	}
}

func splitPromotedTo(v string) (projectID, slug string, ok bool) {
	projectID, slug, ok = strings.Cut(v, "/")
	if !ok || projectID == "" || slug == "" || strings.Contains(slug, "/") {
		return "", "", false
	}
	return projectID, slug, true
}

// EditIdea overwrites the idea's canvas with body and commits the
// change under the standard idea-edit trailer block. Body is taken
// verbatim — CRLF normalisation is the caller's responsibility.
//
// Returns run.ErrNothingToCommit when body matches the on-disk content
// (caller can treat this as success). Returns run.ErrRunNotFound when
// no such run exists. Returns ErrNotIdea when the run is not an
// in-progress idea (defence in depth — promoted ideas are owned by the
// destination's design stage and must not be rewritten through this
// path).
//
// The CLI's `moe idea edit` does not migrate to EditIdea: its editor /
// chat flow writes the file in place via $EDITOR or runIdeaChat and a
// body-in API doesn't fit cleanly. The trailer block is the same shape
// (work: update idea, MoE-Run / MoE-Project / MoE-Workflow / MoE-Document).
func EditIdea(root, projectID, slug, body string) error {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	if md.Workflow != dash.IdeaWorkflow || md.Status != run.StatusInProgress {
		return ErrNotIdea
	}

	canvasRel := run.ContentPath(projectID, slug, ideaDocID)
	docDir := run.DocDir(projectID, slug, ideaDocID)
	if err := os.MkdirAll(filepath.Join(root, docDir), 0o755); err != nil {
		return fmt.Errorf("runopen: mkdir doc dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(body), 0o644); err != nil {
		return fmt.Errorf("runopen: write canvas: %w", err)
	}

	msg := fmt.Sprintf("work: update %s\n\n", ideaDocID) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: dash.IdeaWorkflow,
			Document: ideaDocID,
		}.String()
	return withRepoLock(root, repolock.Options{
		Purpose: "idea-edit",
		Run:     projectID + "/" + slug,
	}, func() error {
		return run.StageAndCommit(root, msg, docDir)
	})
}

// withRepoLock acquires the bureaucracy-wide lock at <root>/.moe/lock,
// runs fn, releases. Mirrors internal/cli/repolock.go's wrapper;
// duplicated here so runopen has no cli dependency.
func withRepoLock(root string, opts repolock.Options, fn func() error) error {
	l, err := repolock.Acquire(root, opts)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}
