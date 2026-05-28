package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// enterTerminal advances md to newStatus (merged or closed), running
// follow-up harvest first when the workflow owns one. Returns the
// repo-relative paths the caller must include in its status-flip
// commit (run.json, plus followups.md when it ends up on disk).
//
// skipEdit controls the harvest pre-flight: false opens followups.md
// in $EDITOR (close and push — both are operator-initiated termination
// decisions, so the operator gets to review what's about to fan out
// into ideas); true harvests as-is (sync reconciling an upstream merge,
// and `close --no-edit`).
//
// The whole rule "any non-idea run reaching a terminal status
// harvests its follow-ups" lives here, once. Every site that flips a
// run to merged or closed must flow through this helper — the lint
// test in terminal_lint_test.go pins that invariant.
//
// Idempotent on harvest: lines already marked `[x]` are skipped, and
// partial progress is committed by harvestFollowups itself, so a
// retry after partial failure picks up where it left off.
func enterTerminal(root string, md *run.Metadata, newStatus string, skipEdit bool) ([]string, error) {
	if newStatus != run.StatusMerged && newStatus != run.StatusClosed {
		return nil, fmt.Errorf("enterTerminal: not a terminal status: %q", newStatus)
	}
	paths := []string{filepath.Join(run.Dir(md.Project, md.ID), "run.json")}
	if md.Workflow != dash.IdeaWorkflow {
		if err := harvestFollowups(root, md.Project, md.ID, md.Workflow, skipEdit); err != nil {
			return nil, err
		}
		rel := run.FollowupsPath(md.Project, md.ID)
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			paths = append(paths, rel)
		}
		// Lore harvest is the parallel pop on feedback/lore.md: same
		// shape, same idempotency, different destination
		// (lore/<slug>.md at the bureaucracy root instead of an idea
		// run). The promoted files plus the rewritten feedback file
		// ride along on the close commit; stage lore/ as a dir so
		// every newly-written slug gets picked up without enumerating
		// them here.
		if err := harvestLore(root, md.Project, md.ID, md.Workflow, skipEdit); err != nil {
			return nil, err
		}
		loreRel := run.FeedbackPath(md.Project, md.ID, "lore")
		if _, err := os.Stat(filepath.Join(root, loreRel)); err == nil {
			paths = append(paths, loreRel)
		}
		if _, err := os.Stat(filepath.Join(root, wiki.LoreDirRel)); err == nil {
			paths = append(paths, wiki.LoreDirRel)
		}
	}
	md.Status = newStatus
	if err := run.Save(root, md); err != nil {
		return nil, err
	}
	return paths, nil
}

// revertTerminal undoes the status flip a prior enterTerminal call
// wrote to disk: it restores md.Status to priorStatus and re-saves
// run.json. mergePath uses this when ffPushToDefault fails after
// harvest succeeded — the harvest commits and any followups.md
// rewrites are kept (idempotent on retry; harvest will skip the
// already-checked entries), but the run goes back to its pre-merge
// status so the next push attempt sees the correct gate state and
// the operator's reloadRun reflects "merge didn't happen yet."
//
// Lives in terminal.go so the lint invariant ("only this file flips
// terminal status") still holds — the priorStatus rewrite here is
// the inverse of the enterTerminal write it pairs with.
func revertTerminal(root string, md *run.Metadata, priorStatus string) error {
	md.Status = priorStatus
	return run.Save(root, md)
}
