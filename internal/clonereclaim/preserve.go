// Package clonereclaim contains conservative evidence readers for per-run
// clone cleanup. Evidence is not a deletion permit: mutation must revalidate
// lifetime, generation, resource ownership and path confinement under runowner.
package clonereclaim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

const scanOutputLimit = 16 << 20

// Preservation describes why an unexpired clone may or may not be discarded.
// A nonempty Reason is a policy hold, while unreadable/incomplete evidence
// returns an error. Artifact hashes must come from validated generation-owned
// receipts; a directory prefix such as .moe/ never proves ownership.
type Preservation struct {
	Reason  string `json:"reason,omitempty"`
	Objects int    `json:"objects"`
}

// InspectPreservation establishes independent retention in the canonical
// source for every available Git object, including shared and unreachable
// objects. It does not fetch, prune, repair, run hooks or write the index.
func InspectPreservation(ctx context.Context, clone, source string, artifacts map[string]string) (Preservation, error) {
	var result Preservation
	read := func(dir string, args ...string) (string, error) {
		return git.Inspect(ctx, dir, scanOutputLimit, args...)
	}
	for _, dir := range []string{clone, source} {
		if err := completeHistory(ctx, dir); err != nil {
			return result, err
		}
	}
	if err := independentObjectStorage(ctx, source, clone); err != nil {
		return result, err
	}
	state, err := read(clone, "status", "--porcelain=v1", "-z", "--untracked-files=no", "--ignore-submodules=none")
	if err != nil {
		return result, err
	}
	if state != "" {
		result.Reason = "local_modifications"
		return result, nil
	}
	for _, args := range [][]string{
		{"ls-files", "--others", "--exclude-standard", "-z"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"},
	} {
		files, err := read(clone, args...)
		if err != nil {
			return result, err
		}
		for path := range strings.SplitSeq(files, "\x00") {
			if path == "" {
				continue
			}
			want, known := artifacts[path]
			if !known {
				result.Reason = "local_output"
				return result, nil
			}
			got, err := artifactDigest(ctx, clone, path)
			if err != nil {
				return result, err
			}
			if got != want {
				result.Reason = "artifact_mismatch"
				return result, nil
			}
		}
	}
	stash, err := read(clone, "for-each-ref", "--format=%(objectname)", "refs/stash")
	if err != nil {
		return result, err
	}
	if stash != "" {
		result.Reason = "stash"
		return result, nil
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-apply", "rebase-merge", "BISECT_LOG", "sequencer", "index.lock"} {
		path, err := read(clone, "rev-parse", "--path-format=absolute", "--git-path", name)
		if err != nil {
			return result, err
		}
		if _, err := os.Lstat(strings.TrimSpace(path)); err == nil {
			result.Reason = "unfinished_git_operation"
			return result, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	// fsck accounts for HEAD, refs and reflogs as well as loose/packed objects.
	// Missing or corrupt objects must fail even if the set comparison could
	// otherwise appear to be a subset.
	for _, dir := range []string{clone, source} {
		if _, err := read(dir, "fsck", "--full", "--no-dangling"); err != nil {
			return result, err
		}
	}
	retained, err := read(source, "rev-list", "--objects", "--all", "--no-object-names")
	if err != nil {
		return result, err
	}
	objects, err := read(clone, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if err != nil {
		return result, err
	}
	kept := make(map[string]bool)
	for object := range strings.FieldsSeq(retained) {
		if !objectID(object) {
			return result, errors.New("invalid retained object inventory")
		}
		kept[object] = true
	}
	for object := range strings.FieldsSeq(objects) {
		if !objectID(object) {
			return result, errors.New("invalid clone object inventory")
		}
		result.Objects++
		if !kept[object] {
			result.Reason = "unique_git_objects"
		}
	}
	if result.Objects == 0 {
		return result, errors.New("empty Git object inventory")
	}
	return result, nil
}

func objectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func completeHistory(ctx context.Context, dir string) error {
	read := func(args ...string) (string, error) { return git.Inspect(ctx, dir, scanOutputLimit, args...) }
	shallow, err := read("rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(shallow) != "false" {
		return errors.New("incomplete shallow history")
	}
	replace, err := read("for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil {
		return err
	}
	if replace != "" {
		return errors.New("replacement history")
	}
	graft, err := read("rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(strings.TrimSpace(graft)); err == nil {
		return errors.New("grafted history")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	config, err := read("config", "--list")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.ToLower(config), "\n") {
		key, _, _ := strings.Cut(line, "=")
		if key == "extensions.partialclone" || strings.HasSuffix(key, ".promisor") {
			return errors.New("incomplete promisor history")
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

// Canonical alternates are a graph, not necessarily a single path. Every
// reachable object directory must survive removal of the clone. Reject
// symlinked object storage rather than assuming its final destination.
func independentObjectStorage(ctx context.Context, source, clone string) error {
	gitdir, err := git.Inspect(ctx, source, scanOutputLimit, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	gitdir, err = filepath.EvalSymlinks(strings.TrimSpace(gitdir))
	if err != nil {
		return err
	}
	clone, err = filepath.Abs(clone)
	if err != nil {
		return err
	}
	if within(clone, gitdir) {
		return errors.New("canonical Git directory depends on clone")
	}
	pending := []string{filepath.Join(gitdir, "objects")}
	seen := make(map[string]bool)
	nodes := 0
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		if len(seen) >= 64 {
			return errors.New("object storage scan limit exceeded")
		}
		seen[path] = true
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if real != path || within(clone, real) {
			return errors.New("canonical object storage depends on clone or symlinks")
		}
		if err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			nodes++
			if nodes > 200000 {
				return errors.New("object storage scan limit exceeded")
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlinked canonical object storage")
			}
			return nil
		}); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(path, "info", "alternates"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 1<<20+1))
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return errors.New("alternates scan limit exceeded")
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "\"") {
				return errors.New("quoted object alternate requires recovery")
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(path, line)
			}
			pending = append(pending, line)
		}
	}
	return nil
}

func artifactDigest(ctx context.Context, root, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", errors.New("invalid owned artifact path")
	}
	path := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlinked owned artifact")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("owned artifact %s is not regular", rel)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 1<<20+1))
	if err != nil {
		return "", err
	}
	if n > 1<<20 {
		return "", errors.New("owned artifact scan limit exceeded")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
