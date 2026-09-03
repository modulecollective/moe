package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/twin"
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
			Consent:  walkConsent(),
		}.String()
	err := run.StageAndCommit(root, msg, runJSON)
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
}

// commitAdvance records that docID's stage is done without producing a
// work-turn for the next stage. The operator hit the chain prompt's
// "decline, advance" key: they don't want to run the next stage now, but
// they don't want the run to re-open and re-run docID's agent the next
// time it's picked up either.
//
// The commit itself is runopen.AdvanceMarker — shared with serve's
// advance-mark route, which writes the same marker from the web. What
// stays here is the ride's consent: a chain prompt answered mid-cascade
// stamps the level it is riding at, which a click has nothing to say
// about. Lock policy lives at the callsite, like every other main
// writer: the chain prompt's `a` branch wraps this in repolock.With.
func commitAdvance(root string, md *run.Metadata, docID string) error {
	return runopen.AdvanceMarker(root, md, docID, walkConsent())
}

// commitTurn stages the document dir and run.json, then commits with
// a trailer block keyed to the document/session. See docs/concepts.md
// §"Runs, Stages, And Canvases" for the trailer convention.
//
// extraPaths lists additional path specs (relative to root) to stage
// alongside the document dir — the projectCommitDirs trees an sdlc
// stage may write — so the operator always sees the agent's edits
// there and the canvas snapshot moving together in git history.
//
// timedOut is the headless cap that killed this turn, or zero when the
// turn ended any other way. Non-zero adds MoE-Timed-Out to the block —
// the turn's own commit is where a kill becomes durable, since the
// transcript it interrupted gets overwritten by the next drive. Zero
// leaves the message byte-identical to what every ordinary turn writes.
func commitTurn(root string, md *run.Metadata, docID string, timedOut time.Duration, extraPaths ...string) error {
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

	block := trailers.Block{
		Run:      md.ID,
		Project:  md.Project,
		Workflow: md.Workflow,
		Document: docID,
		Session:  md.Documents[docID].Session,
		Consent:  walkConsent(),
	}
	if timedOut > 0 {
		block.TimedOut = timedOut.String()
	}
	msg := fmt.Sprintf("work: update %s\n\n", docID) + block.String()
	allPaths := append([]string{docDir, runJSON}, extraPaths...)
	// followups.md is sibling of run.json — stages append to it as
	// they spot adjacent work to capture. Stage it conditionally so
	// turns that touched neither the doc nor the followups file still
	// trip ErrNothingToCommit cleanly inside StageAndCommit.
	if followupsRel, ok := stageableFollowups(root, md); ok {
		allPaths = append(allPaths, followupsRel)
	}
	// feedback/*.md is the sibling directory for notes a run leaves for
	// something downstream to harvest — twin.md and lore.md today,
	// another feedback/*.md here for free. Same conditional-stage
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

// projectCommitDirs names the directories under projects/<p>/ whose
// edits ride a workflow's per-turn stage commit alongside the canvas.
//
// sdlc gets all four. A stage that authors a hook, a chore, a
// knowledge topic, or a twin edit as a side deliverable of its main
// change would otherwise leave the file untracked, and it dies with the
// pruned session worktree while the canvas claims it landed. Each of
// the four once had its own workflow; the whitelist dissolved the
// reason for them, and now sdlc is the only route.
//
// Deliberately not a sweep of projects/<p>/: src/ is the submodule
// pointer. Extending this whitelist is a one-line change if a future
// artifact class needs it.
//
// Also the single source of truth for the prompt sentence that tells
// the agent these dirs are writable — see operationalCore — and for
// whether a turn owes the project-doc hygiene gate.
func projectCommitDirs(workflow string) []string {
	if workflow == sdlcWorkflow {
		return []string{"hooks", "chores", "knowledge", twin.DirRel}
	}
	return nil
}

// stageProjectDirs is the ExtraStagePaths callback the sdlc workflow
// hands runStageSession. It resolves projectCommitDirs against the
// run's project and drops the ones that don't exist in the session
// worktree — `git add --` fails on a pathspec matching nothing, and
// most projects lack some of the dirs. Same conditional-stage shape as
// stageableFollowups.
func stageProjectDirs(workRoot string, md *run.Metadata) ([]string, error) {
	var out []string
	for _, name := range projectCommitDirs(md.Workflow) {
		rel := filepath.Join(project.Dir(md.Project), name)
		if _, err := os.Stat(filepath.Join(workRoot, rel)); err != nil {
			continue
		}
		out = append(out, rel)
	}
	return out, nil
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
