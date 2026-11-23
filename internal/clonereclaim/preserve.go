// Package clonereclaim contains conservative evidence readers for per-run
// clone cleanup. Evidence is not a deletion permit: mutation must revalidate
// lifetime, generation, resource ownership and path confinement under runowner.
package clonereclaim

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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
	if err := inspectCloneLayout(ctx, clone); err != nil {
		return result, err
	}
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
	// Porcelain honors these flags even when tracked bytes have changed.
	// Refuse them without refreshing or changing the candidate's index.
	entries, err := read(clone, "ls-files", "-v", "-z")
	if err != nil {
		return result, err
	}
	for entry := range strings.SplitSeq(entries, "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 3 || entry[1] != ' ' {
			return result, errors.New("invalid tracked file inventory")
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			result.Reason = "index_inspection_suppressed"
			return result, nil
		}
	}
	// Cached diff reads objects, never worktree content filters. Status can
	// execute those filters and hide unique local bytes (even with no diff).
	state, err := read(clone, "diff", "--cached", "--raw", "-z", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return result, err
	}
	if state != "" {
		result.Reason = "local_modifications"
		return result, nil
	}
	unchanged, err := inspectTrackedBytes(ctx, clone)
	if err != nil {
		return result, err
	}
	if !unchanged {
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

// Compare the actual bytes with the index blobs, independent of Git's stat
// cache and all conversions (clean/process filters, EOL, ident and encoding).
// The index must already match HEAD; the later object scan proves retention.
func inspectTrackedBytes(ctx context.Context, clone string) (bool, error) {
	entries, err := git.Inspect(ctx, clone, scanOutputLimit, "ls-files", "--stage", "-z")
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(clone)
	if err != nil {
		return false, err
	}
	defer root.Close()
	remaining := int64(1 << 30)
	for entry := range strings.SplitSeq(entries, "\x00") {
		if entry == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || !objectID(fields[1]) || !filepath.IsLocal(path) {
			return false, errors.New("invalid tracked blob inventory")
		}
		if fields[2] != "0" {
			return false, nil
		}
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var body io.Reader
		var file *os.File
		size := info.Size()
		if size > remaining {
			return false, errors.New("tracked byte inventory limit exceeded")
		}
		switch fields[0] {
		case "100644", "100755":
			if !info.Mode().IsRegular() || (info.Mode()&0o111 != 0) != (fields[0] == "100755") {
				return false, nil
			}
			file, err = root.Open(path)
			if err != nil {
				return false, err
			}
			body = file
		case "120000":
			if info.Mode()&os.ModeSymlink == 0 {
				return false, nil
			}
			target, err := root.Readlink(path)
			if err != nil {
				return false, err
			}
			size = int64(len(target))
			body = strings.NewReader(target)
		default:
			return false, nil
		}
		var sum hash.Hash = sha1.New()
		if len(fields[1]) == 64 {
			sum = sha256.New()
		}
		fmt.Fprintf(sum, "blob %d\x00", size)
		n, readErr := io.Copy(sum, io.LimitReader(body, remaining+1))
		var closeErr error
		if file != nil {
			closeErr = file.Close()
		}
		if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
			return false, err
		}
		remaining -= n
		if remaining < 0 {
			return false, errors.New("tracked byte inventory limit exceeded")
		}
		if n != size || hex.EncodeToString(sum.Sum(nil)) != fields[1] {
			return false, nil
		}
	}
	return true, nil
}

// An ordinary .git directory can still redirect Git through core.worktree or
// commondir. Bind every inspection to the tree and Git storage we admitted.
func inspectCloneLayout(ctx context.Context, clone string) error {
	clone, err := filepath.Abs(clone)
	if err != nil {
		return err
	}
	out, err := git.Inspect(ctx, clone, scanOutputLimit, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return err
	}
	paths := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(paths) != 3 {
		return errors.New("ambiguous clone Git layout")
	}
	for i, want := range []string{clone, filepath.Join(clone, ".git"), filepath.Join(clone, ".git")} {
		real, err := filepath.EvalSymlinks(paths[i])
		if err != nil {
			return err
		}
		if paths[i] != want || real != want {
			return errors.New("Git worktree or storage redirects outside the admitted clone")
		}
	}
	return nil
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
