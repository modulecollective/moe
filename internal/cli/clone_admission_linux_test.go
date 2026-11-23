//go:build linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func TestCloneAdmissionAcceptsCreatedRunIDs(t *testing.T) {
	for _, id := range []string{"valid--id", "valid-id-"} {
		for _, withClone := range []bool{false, true} {
			t.Run(id+map[bool]string{false: "/absent", true: "/clone"}[withClone], func(t *testing.T) {
				root := newTestBureaucracy(t)
				markBureaucracy(t, root)
				seedProjectWithSubmodule(t, root, "tele--")
				t.Setenv("MOE_HOME", root)
				var md *run.Metadata
				err := repolock.With(root, repolock.Options{Purpose: "create regression run"}, func() error {
					var err error
					md, err = run.New(root, "tele--", run.Options{ID: id, Workflow: "sdlc"})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				h, err := runowner.Acquire(root, md.Project, md.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := clonereclaim.CheckRunRecovery(h); err != nil {
					h.Release()
					t.Fatal(err)
				}
				var path string
				if withClone {
					path, err = attachRunWorkspace(h, md, "moe/"+md.ID)
					if err == nil {
						_, _, err = setupRunEnvironment(h, path, md, io.Discard, io.Discard)
					}
				}
				h.Release()
				if err != nil {
					t.Fatal(err)
				}
				if p, r, err := splitCloneRef(md.Project + "/" + id); err != nil || p != md.Project || r != id {
					t.Fatalf("exact filter: %q %q %v", p, r, err)
				}
				if code := Run([]string{"sdlc", "close", "--no-edit", md.Project + "/" + id}, io.Discard, io.Discard); code != 0 {
					t.Fatalf("close exit %d", code)
				}
				if got := loadRun(t, root, md.Project, id); got.Status != run.StatusClosed {
					t.Fatalf("close status: %s", got.Status)
				}
				if !withClone {
					if _, err := os.Stat(filepath.Join(root, ".moe", "clones", md.Project, id)); !os.IsNotExist(err) {
						t.Fatalf("unexpected clone: %v", err)
					}
				}
			})
		}
	}
}
