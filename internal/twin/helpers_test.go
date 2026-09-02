package twin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// newGitRepo is the shared fixture root: scoped git config, throwaway
// tempdir, one initial commit.
func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubMissing writes a harmless placeholder for every managed doc the
// fixture didn't seed, so a test that cares about one finding isn't
// also reading MissingManagedDocs for the docs it never mentioned. The
// stubs carry no links, citations, or glossary terms, so they add no
// findings of their own. Names in skip are left absent — a test about
// a missing doc needs it to stay missing.
func stubMissing(t *testing.T, dir string, skip ...string) {
	t.Helper()
	for _, name := range managedDocs {
		if slices.Contains(skip, name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			continue
		}
		writeFile(t, filepath.Join(dir, name), "# Stub\n\nPlaceholder prose.\n")
	}
}
