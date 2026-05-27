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
// Both Open and Promote take the repolock around their mutations so
// concurrent invocations don't clobber each other's git index.
package runopen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
