package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	// git invocation and leaves the index untouched. thread.jsonl is
	// mirrored every turn, so without this guard the staging set is
	// non-empty and the turn would commit a transcript-only snapshot
	// — the failure mode the missing-canvas-doc run was opened against.
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
