package run

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

// ArtifactReader reads the current checkout first, then indexed blobs for
// skip-worktree paths. Keep one per operation: omitted entries are collected
// once, lazily, with no persistent cache or filesystem hydration.
// It is not safe for concurrent use.
type ArtifactReader struct {
	root    string
	loaded  bool
	omitted map[string]string // repo-relative path to indexed blob OID
	err     error
}

func NewArtifactReader(root string) *ArtifactReader { return &ArtifactReader{root: root} }

func (a *ArtifactReader) load() error {
	if a.loaded {
		return a.err
	}
	a.loaded = true
	a.omitted = map[string]string{}
	// Non-repository roots are useful for readers of unpacked fixtures too.
	if _, err := os.Stat(filepath.Join(a.root, ".git")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		a.err = err
		return err
	}
	out, err := git.Output(a.root, "ls-files", "--full-name", "-t", "--stage", "-z")
	if err != nil {
		a.err = err
		return err
	}
	for entry := range strings.SplitSeq(out, "\x00") {
		if !strings.HasPrefix(entry, "S ") {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 4 || fields[3] != "0" {
			a.err = fmt.Errorf("artifact index: unexpected entry %q", entry)
			return a.err
		}
		a.omitted[path] = fields[2]
	}
	return nil
}

func (a *ArtifactReader) path(rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("artifact: path must be relative to root: %q", rel)
	}
	return filepath.Join(a.root, rel), nil
}

// Exists shares discovery with Open, so omitted backends still participate
// in transcript disambiguation. Permission and index errors are returned.
func (a *ArtifactReader) Exists(rel string) (bool, error) {
	path, err := a.path(rel)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err == nil {
		return !info.IsDir(), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := a.load(); err != nil {
		return false, err
	}
	_, ok := a.omitted[filepath.ToSlash(filepath.Clean(rel))]
	return ok, nil
}

func (a *ArtifactReader) Open(rel string) (io.ReadCloser, error) {
	path, err := a.path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if loadErr := a.load(); loadErr != nil {
		return nil, loadErr
	}
	oid, ok := a.omitted[filepath.ToSlash(filepath.Clean(rel))]
	if !ok {
		return nil, err
	}
	blob, err := git.Output(a.root, "cat-file", "blob", oid)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(blob)), nil
}

// DocumentIDs includes stages whose only files were omitted artifacts and
// therefore have no directory on disk. Metadata's documents map is a session
// ledger, not an inventory of all stage artifacts.
func (a *ArtifactReader) DocumentIDs(projectID, runID string) ([]string, error) {
	rel := filepath.Join(Dir(projectID, runID), "documents")
	entries, err := os.ReadDir(filepath.Join(a.root, rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ids := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			ids[e.Name()] = true
		}
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	prefix := filepath.ToSlash(rel) + "/"
	for path := range a.omitted {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			if id, _, ok := strings.Cut(rest, "/"); ok {
				ids[id] = true
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
