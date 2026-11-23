package cli

import (
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"os"
	"path/filepath"
	"testing"
)

func attachTestRunWorkspace(t *testing.T, root string, md *run.Metadata, branch string) (string, error) {
	t.Helper()
	h, err := runowner.Acquire(root, md.Project, md.ID)
	if err != nil {
		return "", err
	}
	defer h.Release()
	return attachRunWorkspace(h, md, branch)
}

func cleanTestClone(t *testing.T, root, p, id string) string {
	t.Helper()
	source := filepath.Join(root, "projects", p, "src")
	if _, err := os.Stat(filepath.Join(source, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		gittest.InitAt(t, source)
		gittest.Commit(t, source, "source seed")
		gittest.Run(t, root, "add", source)
		gittest.Run(t, root, "commit", "-m", "record fixture source", "--", source)
	}
	h, err := runowner.Acquire(root, p, id)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	path, err := ensureRunClone(h)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func releaseTestRunWorkspace(t *testing.T, root string, md *run.Metadata) error {
	t.Helper()
	h, err := runowner.Acquire(root, md.Project, md.ID)
	if err != nil {
		return err
	}
	defer h.Release()
	return releaseRunWorkspace(h, md)
}
