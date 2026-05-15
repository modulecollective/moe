// Package cli, clone_canvas.go: the per-turn canvas shuttle between the
// bureaucracy session worktree and the sandbox clone for stages that
// have a clone (NeedsSandbox=true).
//
// The reason this file exists: codex's `apply_patch` enforces a stricter
// "writes must be inside the cwd's project" check than its sandbox-plus-
// add-dir contract suggests. With cwd = sandbox clone and the canvas in
// the bureaucracy worktree, headless turns hit "patch rejected: writing
// outside of the project; rejected by user approval settings" — the
// agent has nothing to write to and the canvas-existence gate in
// commitTurn fails the turn.
//
// The fix routes the canvas through `<clonePath>/.moe-canvas.md`, which
// `apply_patch` sees as in-project. MoE owns the data flow: pre-turn
// copy bureaucracy → clone, post-turn copy clone → bureaucracy. The
// agent's system prompt points at `./.moe-canvas.md` (cwd-relative)
// for code-bearing stages so this is the natural-default path for
// both codex and claude.
//
// `.moe-canvas.md` is hidden from `git status` via the clone's local
// exclude file so it doesn't show up as untracked-noise in the
// sandbox.
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

// CloneCanvasName is the in-clone canvas filename agents read and write
// during code-bearing stages. Lives at <clonePath>/<CloneCanvasName>.
const CloneCanvasName = ".moe-canvas.md"

// syncCanvasIntoClone copies the bureaucracy canvas at
// workRoot/<canvas-rel> into <clonePath>/.moe-canvas.md so the agent's
// in-cwd writes have somewhere to land. No-op when clonePath is empty
// (document-only stages keep using bureaucracy absolute paths directly).
//
// When the bureaucracy canvas does not exist yet — the first
// code-stage turn against a fresh run — any stale `.moe-canvas.md`
// left by a previous aborted turn is removed so the agent reads from
// empty rather than from a prior session's tail.
func syncCanvasIntoClone(workRoot, clonePath string, md *run.Metadata, docID string) error {
	if clonePath == "" {
		return nil
	}
	src := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	dst := filepath.Join(clonePath, CloneCanvasName)
	data, err := os.ReadFile(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if rerr := os.Remove(dst); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("sync canvas: clear stale %s: %w", dst, rerr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("sync canvas: read %s: %w", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("sync canvas: write %s: %w", dst, err)
	}
	return nil
}

// syncCanvasFromClone copies <clonePath>/.moe-canvas.md back to the
// bureaucracy canvas before commitTurn's existence check runs.
// Clone always wins — if the agent also wrote the bureaucracy canvas
// directly (e.g. fell back to an absolute bash write), the clone copy
// overwrites it so the canonical convention is enforced, not advised.
//
// A missing clone canvas is left as-is: commitTurn's bureaucracy-side
// gate will then fire on the absent canvas, which is the loud failure
// we want when an agent never wrote anything.
func syncCanvasFromClone(workRoot, clonePath string, md *run.Metadata, docID string) error {
	if clonePath == "" {
		return nil
	}
	src := filepath.Join(clonePath, CloneCanvasName)
	data, err := os.ReadFile(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("sync canvas: read %s: %w", src, err)
	}
	dst := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("sync canvas: mkdir %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("sync canvas: write %s: %w", dst, err)
	}
	return nil
}

// excludeCloneCanvas adds CloneCanvasName to the clone's local exclude
// file (.git/info/exclude in the common gitdir) so the moe-managed
// shuttle file doesn't appear as untracked noise in `git status`.
//
// Local exclude — not the project's `.gitignore` — because the
// project's .gitignore is tracked and would land in PRs. The common
// gitdir is shared across all worktrees of the same submodule, so the
// entry persists across sandboxes for the same project. Idempotent.
func excludeCloneCanvas(clonePath string) error {
	if clonePath == "" {
		return nil
	}
	out, err := git.Output(clonePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("sync canvas: locate gitdir for %s: %w", clonePath, err)
	}
	gitCommonDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(clonePath, gitCommonDir)
	}
	excludePath := filepath.Join(gitCommonDir, "info", "exclude")

	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sync canvas: read %s: %w", excludePath, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == CloneCanvasName {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("sync canvas: mkdir %s: %w", filepath.Dir(excludePath), err)
	}
	appended := string(existing)
	if appended != "" && !strings.HasSuffix(appended, "\n") {
		appended += "\n"
	}
	appended += CloneCanvasName + "\n"
	if err := os.WriteFile(excludePath, []byte(appended), 0o644); err != nil {
		return fmt.Errorf("sync canvas: write %s: %w", excludePath, err)
	}
	return nil
}
