package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// commitSessionStart commits run.json immediately after EnsureDocument
// mints a fresh Claude session UUID, so the long Claude run that follows
// doesn't leave the bureaucracy tree dirty for hours. Only run.json is
// staged — any unrelated edits the operator had in the tree stay put.
//
// ErrNothingToCommit is tolerated silently: the caller only reaches this
// path when mutated=true, so run.json is expected to differ from HEAD,
// but if some concurrent action already committed the identical state
// there's no work to do and no reason to fail the turn.
func commitSessionStart(root string, md *run.Metadata, docID string) error {
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf("work: start session for %s\n\n", docID) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
			Document: docID,
			Session:  md.Documents[docID].Session,
		}.String()
	err := run.StageAndCommit(root, msg, runJSON)
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
}

// commitWikiTurn stages the wiki content dir alongside the per-run
// canvas in a single `work: <docID> pass <runSlug>` commit. Shared by
// the three wiki-attached out-of-band sessions (claim, reflect, lint)
// — every wiki-touching workflow lands its turn through this helper.
//
// The canvas branch is conditional: callers (claim, reflect) instruct
// the agent to drop a per-pass record at `run.ContentPath(...)`, and
// the helper stages it alongside the wiki edits so both land in the
// same commit and the session-close empty-canvas gate sees a non-empty
// canvas at the branch tip. Lint doesn't write a canvas, so `os.Stat`
// skips the branch and the commit stays wiki-only — if lint ever starts
// writing a canvas the helper picks it up with no change. The close-
// time gate is the strict check; this helper tolerates a missing
// canvas so the wiki edits still land if the agent forgot.
func commitWikiTurn(workRoot, workflow, projectID, runSlug, docID, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	paths := []string{wikiRel}
	canvasRel := run.ContentPath(projectID, runSlug, docID)
	if _, err := os.Stat(filepath.Join(workRoot, canvasRel)); err == nil {
		paths = append(paths, canvasRel)
	}
	if err := run.Stage(workRoot, paths...); err != nil {
		return err
	}
	if !run.HasStagedChanges(workRoot) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf("work: %s pass %s\n\n", docID, runSlug) +
		trailers.Block{
			Run:      runSlug,
			Project:  projectID,
			Workflow: workflow,
			Document: docID,
		}.String()
	return run.StageAndCommit(workRoot, msg, paths...)
}

// commitTurn stages the document dir and run.json, then commits with
// a trailer block keyed to the document/session. See README §"one run
// branch per run" for the trailer convention.
//
// extraPaths lists additional path specs (relative to root) to stage
// alongside the document dir. Used by ingest stages to ride the wiki
// dir into the same per-turn commit as the canvas, so the operator
// always sees the agent's wiki edits and the canvas snapshot moving
// together in git history.
func commitTurn(root string, md *run.Metadata, docID string, extraPaths ...string) error {
	docDir := run.DocDir(md.Project, md.ID, docID)
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")

	// Cheap os.Stat first so a missing-canvas turn fails before any
	// git invocation and leaves the index untouched. The per-agent
	// thread file is mirrored every turn, so without this guard the
	// staging set is non-empty and the turn would commit a
	// transcript-only snapshot — the failure mode the missing-canvas-doc
	// run was opened against.
	canvas := filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))
	switch info, err := os.Stat(canvas); {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("commit: canvas %s does not exist — agent did not write to its canvas this turn", canvas)
	case err != nil:
		return fmt.Errorf("commit: stat canvas %s: %w", canvas, err)
	case info.Size() == 0:
		return fmt.Errorf("commit: canvas %s is empty", canvas)
	}

	if err := run.Save(root, md); err != nil {
		return err
	}

	msg := fmt.Sprintf("work: update %s\n\n", docID) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
			Document: docID,
			Session:  md.Documents[docID].Session,
		}.String()
	allPaths := append([]string{docDir, runJSON}, extraPaths...)
	// followups.md is sibling of run.json — stages append to it as
	// they spot adjacent work to capture. Stage it conditionally so
	// turns that touched neither the doc nor the followups file still
	// trip ErrNothingToCommit cleanly inside StageAndCommit.
	if followupsRel, ok := stageableFollowups(root, md); ok {
		allPaths = append(allPaths, followupsRel)
	}
	// feedback/*.md is the sibling directory for notes addressed to
	// downstream recipients (twin reflect today). v1 picks up twin.md;
	// another feedback/*.md lands here for free. Same conditional-stage
	// pattern as followups so a turn that touched neither still trips
	// ErrNothingToCommit cleanly.
	allPaths = append(allPaths, stageableFeedback(root, md)...)
	return run.StageAndCommit(root, msg, allPaths...)
}

// stageableFollowups returns the run's followups.md path (relative to
// root) if the file exists, along with true. A missing file means no
// agent or operator has captured anything yet — leave it out of the
// staging set rather than passing a non-existent pathspec to git add.
func stageableFollowups(root string, md *run.Metadata) (string, bool) {
	rel := run.FollowupsPath(md.Project, md.ID)
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		return "", false
	}
	return rel, true
}

// stageableFeedback returns every feedback/<recipient>.md path
// (relative to root) the run has on disk. v1 writers only produce
// twin.md, but the helper globs the directory so a future moe.md (and
// any other recipient added later) rides the same stage commit
// without a code change here. Returns nil when the dir is absent or
// empty — a run with no feedback never touches the index.
func stageableFeedback(root string, md *run.Metadata) []string {
	dir := filepath.Join(root, run.FeedbackDir(md.Project, md.ID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		out = append(out, filepath.Join(run.FeedbackDir(md.Project, md.ID), name))
	}
	return out
}
