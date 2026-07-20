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
// Open, Promote, CloseCapture, Reopen, and ReopenIdea take the repolock
// around their mutations so concurrent invocations do not clobber
// each other.s git index.
package runopen

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// twinWorkflow is the twin (reflect) workflow's name in run.json.
// Spelled here rather than imported because runopen never mints one —
// it names the workflow only to refuse it (see Promote).
const twinWorkflow = "twin"

// ErrNotCapture is returned when the slug names a run that is not an
// in-progress capture (idea or intent) — either a staged workflow, or
// a capture that has already been promoted/closed. Defence in depth:
// serve handlers gate on the same check before rendering actions, and
// this catches replayed POSTs landing on a now-terminal capture.
var ErrNotCapture = errors.New("runopen: not an in-progress capture (workflow not idea/intent, or status!=in_progress)")

// ErrNotReopenableIdea is returned by ReopenIdea when the slug is not
// an idea that can be flipped back to in_progress. The wrapped error
// text names the specific status or destination-state problem.
var ErrNotReopenableIdea = errors.New("runopen: not a reopenable idea")

// NotClosableError reports a close refused because of the run's current
// state — a non-idea run that is pushed, already terminal, or of a
// different workflow — rather than an internal failure (the
// canvas-empty gate, a dirty tree, a commit error). The sdlc-side peer
// of ErrNotCapture.
//
// The close pipeline itself stays in the cli package (it leans on
// cli-resident workspace teardown and harvest helpers), but serve must
// classify its result without importing cli. So the type lives here, in
// the package cli and serve already share: cli's close core returns it
// through serve's CloseRun callback, and serve maps it to HTTP 409 while
// internal failures fall through to 500. Reason is the operator-facing
// message (carried verbatim into the HTTP body and CLI stderr).
type NotClosableError struct{ Reason string }

func (e *NotClosableError) Error() string { return e.Reason }

// PromoteOptions configures Promote. The destination run's workflow,
// workspace, and agent are caller-provided; the first-stage doc id is
// where the source idea's canvas lands as a seed.
type PromoteOptions struct {
	Workflow   string // destination workflow (e.g. "sdlc")
	FirstStage string // destination workflow's first-stage doc id (gets the seeded idea canvas)
	Workspace  string // optional workspace binding for the destination
	Agent      string // optional agent override persisted to run.json
	SpawnedBy  string // optional qualified machine spawner persisted to metadata + trailer
	// Consent is the MoE-Consent value for the destination's open
	// commit. Set by machine callers alongside SpawnedBy (the two travel
	// together — a promote the machine made); empty for the operator's
	// `moe idea promote`, which stamps no consent trailer at all.
	Consent string
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

// Open opens a fresh run under repolock and races the open commit to
// origin. opts.Workflow must be set; opts.ID names the slug (collisions
// fail loud) or opts.IDBase supplies a base for date-suffixed collision
// handling. See run.New for the full opts contract.
func Open(root, projectID string, opts run.Options, stdout, stderr io.Writer) (*run.Metadata, error) {
	runRef := projectID
	if opts.ID != "" {
		runRef = projectID + "/" + opts.ID
	}
	var md *run.Metadata
	err := sync.WithJournalPush(root, repolock.Options{
		Purpose: "run-new",
		Run:     runRef,
	}, stdout, stderr, func() error {
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
func Promote(root, projectID, ideaSlug string, opts PromoteOptions, stdout, stderr io.Writer) (Promoted, error) {
	if opts.Workflow == "" {
		return Promoted{}, errors.New("runopen: PromoteOptions.Workflow is required")
	}
	if opts.FirstStage == "" {
		return Promoted{}, errors.New("runopen: PromoteOptions.FirstStage is required")
	}
	if opts.Workflow == dash.IdeaWorkflow {
		return Promoted{}, errors.New("runopen: cannot promote an idea into another idea run")
	}
	// A twin run is never minted here. Its slug is harness-dated
	// (reflect-YYYY-MM-DD), it takes no seed doc, and it is subject to a
	// one-pass-in-flight rule — all of which live in the reflect mint
	// core, which resolves a twin destination by minting *or* mapping onto
	// the open pass. Promote can only mint, so routing a twin destination
	// through it would open a second pass under a wrong slug with a seed
	// nothing reads. Structural, rather than call-site discipline: the
	// serve promote form and the CLI facades already can't reach twin.
	if opts.Workflow == twinWorkflow {
		return Promoted{}, errors.New("runopen: cannot promote an idea into a twin run; resolve the reflect through the reflect core")
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
		SpawnedBy:   opts.SpawnedBy,
		Trailers:    trailers.Block{Idea: ideaSlug, SpawnedBy: opts.SpawnedBy, Consent: opts.Consent},
	}

	md, err := Open(root, projectID, runOpts, stdout, stderr)
	if err != nil {
		return Promoted{}, err
	}
	markErr := markIdeaPromoted(root, src, md.Project, md.ID, stdout, stderr)
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
	canvasRel := run.ContentPath(projectID, slug, dash.IdeaDocID)
	b, err := os.ReadFile(filepath.Join(root, canvasRel))
	if err != nil {
		return nil, "", fmt.Errorf("--from-idea: read %s: %w", canvasRel, err)
	}
	return md, string(b), nil
}

// MarkPromoted marks an in-progress idea promoted onto a destination
// run the caller already has in hand, writing the same status bump and
// MoE-Promoted-To trailer Promote writes for a run it opened itself.
//
// The seam exists for destinations Promote can't mint: pulse resolving a
// `(twin)`-tagged idea onto the project's reflect, which the reflect
// core either minted or mapped onto. The promotion edge is the same one
// either way, so the journal, the dash, and `moe idea reopen` read a
// twin-tagged idea exactly like any other promoted idea.
func MarkPromoted(root, projectID, ideaSlug, destProjectID, destSlug string, stdout, stderr io.Writer) error {
	if destProjectID == "" || destSlug == "" {
		return errors.New("runopen: MarkPromoted requires a destination run")
	}
	md, err := run.Load(root, projectID, ideaSlug)
	if err != nil {
		return err
	}
	if md.Workflow != dash.IdeaWorkflow {
		return fmt.Errorf("runopen: run %s/%s is a %s run, not an idea", projectID, ideaSlug, md.Workflow)
	}
	if md.Status != run.StatusInProgress {
		return fmt.Errorf("runopen: idea %s/%s is already %s", projectID, ideaSlug, md.Status)
	}
	return markIdeaPromoted(root, md, destProjectID, destSlug, stdout, stderr)
}

// markIdeaPromoted bumps the source idea run's status to
// StatusPromoted and commits the transition with a MoE-Promoted-To
// trailer pointing at the destination run. Separate commit from the
// destination's open: two short commits keep git history honest (one
// event per commit).
func markIdeaPromoted(root string, md *run.Metadata, destProjectID, destSlug string, stdout, stderr io.Writer) error {
	md.Status = run.StatusPromoted
	runJSONRel := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf("Promote idea %s/%s → %s/%s\n\n", md.Project, md.ID, destProjectID, destSlug) +
		trailers.Block{
			Run:        md.ID,
			Project:    md.Project,
			Workflow:   dash.IdeaWorkflow,
			PromotedTo: destProjectID + "/" + destSlug,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: "idea-promote",
		Run:     md.Project + "/" + md.ID,
	}, stdout, stderr, func() error {
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}

// CloseCapture flips an in-progress capture run (idea or intent) to
// closed and commits only its run.json with that workflow's close
// trailer block. Subject and lock purpose derive from the run's own
// workflow, so the resulting commit is byte-identical to the one the
// matching CLI verb (`moe idea close` / `moe intent close`) writes.
func CloseCapture(root, projectID, slug string, stdout, stderr io.Writer) error {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	if !dash.IsCapture(md.Workflow) || md.Status != run.StatusInProgress {
		return ErrNotCapture
	}

	runJSONRel := filepath.Join(run.Dir(projectID, slug), "run.json")
	msg := fmt.Sprintf("Close %s %s/%s\n\n", md.Workflow, projectID, slug) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: md.Workflow,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: md.Workflow + "-close",
		Run:     projectID + "/" + slug,
	}, stdout, stderr, func() error {
		md.Status = run.StatusClosed
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}

// Reopen is the bare flip behind every reopen path: it sets a terminal
// run's status back to in_progress and commits run.json under repolock
// with a reopen trailer derived from the run's own workflow. The caller
// owns all the policy — ReopenIdea's workflow/promoted-destination
// guards, the chat verb's closed-only check — and hands a loaded md
// here only once the run is known reopenable. md.Status is set in place.
//
// Workflow-derived subject ("Reopen idea …", "Reopen chat …"), trailer
// block, and lock purpose keep each workflow's history greppable while
// the flip itself lives in exactly one place.
func Reopen(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	runJSONRel := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf("Reopen %s %s/%s\n\n", md.Workflow, md.Project, md.ID) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: md.Workflow + "-reopen",
		Run:     md.Project + "/" + md.ID,
	}, stdout, stderr, func() error {
		md.Status = run.StatusInProgress
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
// work. The flip itself routes through Reopen.
func ReopenIdea(root, projectID, slug string, stdout, stderr io.Writer) error {
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
	return Reopen(root, md, stdout, stderr)
}

func verifyPromotedDestinationClosed(root, projectID, slug string) error {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return fmt.Errorf("idea reopen: %w", err)
	}
	destValue := idx.PromotedTo[projectID+"/"+slug]
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

// EditCapture overwrites a capture run's canvas (idea or intent) with
// body and commits the change under that workflow's edit trailer
// block. Body is taken verbatim — CRLF normalisation is the caller's
// responsibility.
//
// Returns run.ErrNothingToCommit when body matches the on-disk content
// (caller can treat this as success). Returns run.ErrRunNotFound when
// no such run exists. Returns ErrNotCapture when the run is not an
// in-progress capture (defence in depth — promoted ideas are owned by
// the destination's design stage and must not be rewritten through
// this path).
//
// The CLI's `moe idea edit` / `moe intent edit` do not migrate here:
// their editor flow writes the file in place via $EDITOR and a body-in
// API doesn't fit cleanly. The trailer block is the same shape
// (work: update <doc>, MoE-Run / MoE-Project / MoE-Workflow / MoE-Document).
func EditCapture(root, projectID, slug, body string, stdout, stderr io.Writer) error {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	docID, ok := dash.CaptureDocID(md.Workflow)
	if !ok || md.Status != run.StatusInProgress {
		return ErrNotCapture
	}

	canvasRel := run.ContentPath(projectID, slug, docID)
	docDir := run.DocDir(projectID, slug, docID)
	if err := os.MkdirAll(filepath.Join(root, docDir), 0o755); err != nil {
		return fmt.Errorf("runopen: mkdir doc dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(body), 0o644); err != nil {
		return fmt.Errorf("runopen: write canvas: %w", err)
	}

	msg := fmt.Sprintf("work: update %s\n\n", docID) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: md.Workflow,
			Document: docID,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: md.Workflow + "-edit",
		Run:     projectID + "/" + slug,
	}, stdout, stderr, func() error {
		return run.StageAndCommit(root, msg, docDir)
	})
}
