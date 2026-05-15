// Package cli, clone_canvas.go: the per-turn run-subtree shuttle
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
// Current shape: the whole run subtree is mirrored into the clone as
// `<clonePath>/.moe-run/`. Bureaucracy is read-only from the agent's
// POV; the clone is its read/write workspace. MoE owns the data flow
// — pre-turn copy bureaucracy → clone, post-turn copy clone →
// bureaucracy. The agent's system prompt names paths under
// `./.moe-run/` (cwd-relative) so codex's `apply_patch` sees every
// write as in-project.
//
// Post-turn the shuttle refuses to propagate changes to run.json or to
// any prior-stage `documents/<other>/content.md` — those are MoE-owned
// or earlier-stage artifacts the agent should not be rewriting from a
// code turn. The refusal fails the turn loudly so an agent that strays
// outside its writable surface is named, not silently honored.
//
// `.moe-run/` is hidden from `git status` via the clone's local exclude
// file so it doesn't show up as untracked noise in the sandbox.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// CloneRunDir is the in-clone directory the bureaucracy run subtree is
// mirrored into for code-bearing stages. Lives at
// <clonePath>/<CloneRunDir>/.
const CloneRunDir = ".moe-run"

// threadFilePrefix names per-agent transcript files written by the
// executor directly into the bureaucracy worktree (thread-<agent>.jsonl).
// The shuttle deliberately skips them: the agent does not read its own
// transcripts during a turn, and copying them twice (bureaucracy →
// clone → bureaucracy) would double the I/O on what is already the
// largest file in the run subtree.
const threadFilePrefix = "thread-"

// syncRunIntoClone mirrors the bureaucracy run subtree at
// projects/<p>/runs/<r>/ into <clonePath>/.moe-run/ so the agent's
// in-cwd writes — canvas, followups, twin feedback — have somewhere
// to land that codex's apply_patch accepts as in-project. No-op when
// clonePath is empty (document-only stages keep using bureaucracy
// absolute paths directly).
//
// Atomic: the mirror is staged into a sibling temp dir then renamed
// into place. A failed copy leaves the prior `.moe-run/` intact, so
// the turn either starts on a complete pre-sync mirror or aborts
// cleanly before the agent launches. The previous mirror is removed
// just before the rename to keep the operation idempotent across
// turns.
//
// thread-<agent>.jsonl files are skipped — they're operator forensic
// history written by the executor straight into bureaucracy, not
// agent state, and copying them per-turn would add significant I/O
// to no purpose.
//
// When the bureaucracy run subtree does not exist yet — a stage
// opening against a fresh run before the first turn commits anything
// — any prior `.moe-run/` is removed and the function returns. The
// agent sees an empty cwd-side run subtree and authors from scratch.
func syncRunIntoClone(workRoot, clonePath string, md *run.Metadata) error {
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

	if err := copyTree(src, staging, skipTranscriptFiles); err != nil {
		return fmt.Errorf("sync run: copy %s → %s: %w", src, staging, err)
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

// syncRunFromClone copies <clonePath>/.moe-run/ back into the
// bureaucracy run subtree before commitTurn's existence check runs.
// Overwrite-only: deletes from the agent's side of the boundary are
// not propagated, matching today's `CommitStager` crash-safety
// contract — if the agent crashes mid-turn, whatever it did write
// lands and nothing gets deleted from bureaucracy.
//
// docID names the current stage's canvas. Anything else under
// `documents/<other>/content.md` is a prior-stage artifact: if the
// agent rewrote it, the shuttle refuses the post-sync and fails the
// turn loudly. run.json is similarly guarded — MoE owns it and an
// agent edit there is always a mistake.
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

	dst := filepath.Join(workRoot, run.Dir(md.Project, md.ID))
	if err := guardProtectedPaths(src, dst, md, docID); err != nil {
		return err
	}
	if err := copyTree(src, dst, skipTranscriptFiles); err != nil {
		return fmt.Errorf("sync run: copy %s → %s: %w", src, dst, err)
	}
	return nil
}

// guardProtectedPaths walks the staged clone subtree and refuses to
// mirror back any change to:
//   - run.json — MoE owns it; an agent edit is always wrong.
//   - documents/<other>/content.md for any other != docID — those are
//     prior- (or future-) stage canvases the agent should not be
//     rewriting from a code turn.
//
// Differing bytes against the bureaucracy original triggers a loud
// turn-level error naming the offending path. A missing bureaucracy
// counterpart (e.g. agent created an `documents/<other>/content.md`
// from scratch) is also refused. The clone state is left intact so
// the operator can inspect.
func guardProtectedPaths(src, dst string, md *run.Metadata, docID string) error {
	for _, rel := range []string{"run.json"} {
		if err := refuseIfChanged(src, dst, rel); err != nil {
			return err
		}
	}
	docsDir := filepath.Join(src, "documents")
	entries, err := os.ReadDir(docsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync run: read %s: %w", docsDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == docID {
			continue
		}
		rel := filepath.Join("documents", e.Name(), "content.md")
		if err := refuseIfChanged(src, dst, rel); err != nil {
			return err
		}
	}
	_ = md
	return nil
}

// refuseIfChanged compares src/rel against dst/rel byte-for-byte and
// returns a turn-level error if they differ. Missing src/rel is a
// no-op (the agent didn't touch the file); missing dst/rel with a
// present src/rel is also an offense (the agent fabricated a
// protected file).
func refuseIfChanged(src, dst, rel string) error {
	srcBytes, err := os.ReadFile(filepath.Join(src, rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("sync run: read %s: %w", filepath.Join(src, rel), err)
	}
	dstBytes, err := os.ReadFile(filepath.Join(dst, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sync run: agent created protected file %s; refusing to mirror back", rel)
	}
	if err != nil {
		return fmt.Errorf("sync run: read %s: %w", filepath.Join(dst, rel), err)
	}
	if !bytes.Equal(srcBytes, dstBytes) {
		return fmt.Errorf("sync run: agent modified protected file %s; refusing to mirror back", rel)
	}
	return nil
}

// copyTree walks src and copies every file it visits into dst,
// preserving directory structure. skip, when non-nil, is consulted
// with the path relative to src; returning true skips the entry
// (and, when the entry is a directory, the whole subtree).
//
// Existing files at dst are overwritten. Existing directories at dst
// are kept. The walk does not delete anything at dst — that's the
// overwrite-only contract syncRunFromClone relies on.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// skipTranscriptFiles drops thread-<agent>.jsonl entries from the
// mirror. They're written directly into the bureaucracy worktree by
// the executor and aren't agent-readable state for the turn.
func skipTranscriptFiles(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasPrefix(base, threadFilePrefix) && strings.HasSuffix(base, ".jsonl")
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
