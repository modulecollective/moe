// Package cli, clone_canvas.go: the per-turn writable-artifact shuttle
// between the bureaucracy session worktree and the sandbox clone for
// stages that have a clone (NeedsSandbox=true).
//
// The reason this file exists: codex's `apply_patch` enforces a stricter
// "writes must be inside the cwd's project" check than its sandbox-plus-
// add-dir contract suggests. With cwd = sandbox clone and the canvas in
// the bureaucracy worktree, headless turns hit "patch rejected: writing
// outside of the project; rejected by user approval settings" — the
// agent has nothing to write to and the canvas-existence gate in
// commitTurn fails the turn.
//
// Empirical probing (see this run's design doc) narrowed the root cause
// to the moe-submodule ↔ bureaucracy boundary: when cwd is a `git
// worktree` of the moe submodule and the add-dir is a worktree of the
// bureaucracy superproject, codex refuses cross-boundary writes for
// both apply_patch and the bash sandbox. Two layers, same boundary.
//
// Earlier shape: a single-file shuttle for the canvas. That fixed the
// canvas write but left the same wall in front of `followups.md` and
// `feedback/twin.md`, which agents also produce during a code turn.
//
// Current shape: only the agent-authored artifacts are copied into the
// clone as `<clonePath>/.moe-run/`: the current canvas, followups.md,
// and feedback/twin.md. Bureaucracy is read-only from the agent's POV;
// the clone is its read/write workspace. MoE owns the data flow —
// pre-turn copy bureaucracy → clone, post-turn copy clone →
// bureaucracy. The agent's system prompt names paths under
// `./.moe-run/` (cwd-relative) so codex's `apply_patch` sees every
// write as in-project.
//
// `.moe-run/` is hidden from `git status` via the clone's local exclude
// file so it doesn't show up as untracked noise in the sandbox.
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// CloneRunDir is the in-clone directory for the agent-writable run
// artifacts used by code-bearing stages. Lives at
// <clonePath>/<CloneRunDir>/.
const CloneRunDir = ".moe-run"

// syncRunIntoClone copies the allowlisted agent-writable files from
// the bureaucracy run subtree into <clonePath>/.moe-run/ so the
// agent's in-cwd writes — current canvas, followups, twin feedback —
// have somewhere to land that codex's apply_patch accepts as
// in-project. No-op when clonePath is empty (document-only stages keep
// using bureaucracy absolute paths directly).
//
// Atomic: the allowlisted files are staged into a sibling temp dir then
// renamed into place. A failed copy leaves the prior `.moe-run/`
// intact, so the turn either starts on a complete pre-sync mirror or
// aborts cleanly before the agent launches. The previous mirror is
// removed just before the rename to keep the operation idempotent
// across turns.
//
// When the bureaucracy run subtree does not exist yet — a stage
// opening against a fresh run before the first turn commits anything
// — any prior `.moe-run/` is removed and the function returns. The
// agent sees an empty cwd-side run subtree and authors from scratch.
func syncRunIntoClone(workRoot, clonePath string, md *run.Metadata, docID string) error {
	if clonePath == "" {
		return nil
	}
	src := filepath.Join(workRoot, run.Dir(md.Project, md.ID))
	dst := filepath.Join(clonePath, CloneRunDir)

	srcInfo, err := os.Stat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if rerr := os.RemoveAll(dst); rerr != nil {
			return fmt.Errorf("sync run: clear stale %s: %w", dst, rerr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("sync run: stat %s: %w", src, err)
	case !srcInfo.IsDir():
		return fmt.Errorf("sync run: %s is not a directory", src)
	}

	staging, err := os.MkdirTemp(clonePath, CloneRunDir+".tmp-")
	if err != nil {
		return fmt.Errorf("sync run: stage: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	for _, file := range writableRunFiles(md, docID) {
		dstFile := filepath.Join(staging, file.cloneRel)
		if err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {
			return fmt.Errorf("sync run: mkdir %s: %w", filepath.Dir(dstFile), err)
		}
		if err := copyFileIfExists(filepath.Join(workRoot, file.bureaucracyRel), dstFile); err != nil {
			return fmt.Errorf("sync run: copy %s → %s: %w", file.bureaucracyRel, file.cloneRel, err)
		}
	}

	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("sync run: clear %s: %w", dst, err)
	}
	if err := os.Rename(staging, dst); err != nil {
		return fmt.Errorf("sync run: rename %s → %s: %w", staging, dst, err)
	}
	cleanupStaging = false
	return nil
}

// syncRunFromClone copies the allowlisted files from
// <clonePath>/.moe-run/ back into the bureaucracy run subtree before
// commitTurn's existence check runs.
// Overwrite-only: deletes from the agent's side of the boundary are
// not propagated, matching today's `CommitStager` crash-safety
// contract — if the agent crashes mid-turn, whatever it did write
// lands and nothing gets deleted from bureaucracy.
//
// A missing `.moe-run/` is left as-is: commitTurn's bureaucracy-side
// gate will then fire on the absent canvas, which is the loud failure
// we want when an agent never wrote anything.
func syncRunFromClone(workRoot, clonePath string, md *run.Metadata, docID string) error {
	if clonePath == "" {
		return nil
	}
	src := filepath.Join(clonePath, CloneRunDir)
	srcInfo, err := os.Stat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("sync run: stat %s: %w", src, err)
	case !srcInfo.IsDir():
		return fmt.Errorf("sync run: %s is not a directory", src)
	}

	for _, file := range writableRunFiles(md, docID) {
		if err := copyFileIfExists(filepath.Join(src, file.cloneRel), filepath.Join(workRoot, file.bureaucracyRel)); err != nil {
			return fmt.Errorf("sync run: copy %s → %s: %w", file.cloneRel, file.bureaucracyRel, err)
		}
	}
	return nil
}

type writableRunFile struct {
	bureaucracyRel string
	cloneRel       string
}

func writableRunFiles(md *run.Metadata, docID string) []writableRunFile {
	return []writableRunFile{
		{
			bureaucracyRel: run.ContentPath(md.Project, md.ID, docID),
			cloneRel:       filepath.Join("documents", docID, "content.md"),
		},
		{
			bureaucracyRel: run.FollowupsPath(md.Project, md.ID),
			cloneRel:       "followups.md",
		},
		{
			bureaucracyRel: run.FeedbackPath(md.Project, md.ID, "twin"),
			cloneRel:       filepath.Join("feedback", "twin.md"),
		},
	}
}

// copyFileIfExists overwrites dst with src when src exists. Missing src
// is a no-op: first turns and untouched optional artifacts should be
// created by the agent only if it has something to say.
func copyFileIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// excludeCloneRun adds CloneRunDir to the clone's local exclude file
// (.git/info/exclude in the common gitdir) so the moe-managed shuttle
// dir doesn't appear as untracked noise in `git status`.
//
// Local exclude — not the project's `.gitignore` — because the
// project's .gitignore is tracked and would land in PRs. The common
// gitdir is shared across all worktrees of the same submodule, so the
// entry persists across sandboxes for the same project. Idempotent.
func excludeCloneRun(clonePath string) error {
	if clonePath == "" {
		return nil
	}
	out, err := git.Output(clonePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("sync run: locate gitdir for %s: %w", clonePath, err)
	}
	gitCommonDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(clonePath, gitCommonDir)
	}
	excludePath := filepath.Join(gitCommonDir, "info", "exclude")

	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sync run: read %s: %w", excludePath, err)
	}
	entry := CloneRunDir + "/"
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("sync run: mkdir %s: %w", filepath.Dir(excludePath), err)
	}
	appended := string(existing)
	if appended != "" && !strings.HasSuffix(appended, "\n") {
		appended += "\n"
	}
	appended += entry + "\n"
	if err := os.WriteFile(excludePath, []byte(appended), 0o644); err != nil {
		return fmt.Errorf("sync run: write %s: %w", excludePath, err)
	}
	return nil
}
