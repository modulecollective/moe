package wiki

import (
	"os"
	"path/filepath"
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
