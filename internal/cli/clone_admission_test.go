package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func TestSourceStagePreparationWithoutHooks(t *testing.T) {
	root := seedCloseFixture(t, "tele", "fresh-stage", "sdlc", run.StatusInProgress)
	seedProjectWithSubmodule(t, root, "tele")
	md := loadRun(t, root, "tele", "fresh-stage")
	h, err := runowner.Acquire(root, md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	path, err := attachRunWorkspace(h, md, "moe/"+md.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := setupRunEnvironment(h, path, md, io.Discard, io.Discard)
	if err != nil || len(env) != 0 {
		t.Fatalf("no-hook source preparation: %v %v", env, err)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatal(err)
	}
}
