package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/ui"
)

func installCreateFailureHook(t *testing.T, root, body string) {
	t.Helper()
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
}

// The harvester commits checked-line progress after the second writer
// fails. That commit must not accidentally record the rejected idea.
func TestHarvestFailedCreationDoesNotContaminateProgress(t *testing.T) {
	root, md := controlFixture(t)
	writeFollowups(t, root, "moe", md.ID, "- [ ] `first-fix` — First fix\n  First body.\n- [ ] `second-fix` — Second fix\n  Second body.\n")
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	installCreateFailureHook(t, root, `if [ "$(head -n 1 "$1")" = "Open run moe/second-fix" ]; then
  echo reject-second >&2
  exit 1
fi
`)
	harvest := func() error {
		return repolock.With(root, repolock.Options{Purpose: "test-harvest"}, func() error {
			return harvestFollowups(root, "moe", md.ID, md.Workflow, true)
		})
	}
	if err := harvest(); err == nil || !strings.Contains(err.Error(), "reject-second") {
		t.Fatalf("harvest = %v", err)
	}
	if got := readFollowups(t, root, "moe", md.ID); !strings.Contains(got, "- [x] `first-fix`") || !strings.Contains(got, "- [ ] `second-fix`") {
		t.Fatalf("partial progress = %q", got)
	}
	if _, err := run.Load(root, "moe", "first-fix"); err != nil {
		t.Fatalf("first idea lost: %v", err)
	}
	rel := run.Dir("moe", "second-fix")
	for _, args := range [][]string{{"ls-tree", "-r", "--name-only", "HEAD", "--", rel}, {"ls-files", "--stage", "--", rel}} {
		if got := gittest.Output(t, root, args...); got != "" {
			t.Fatalf("failed idea in index/progress commit: %s", got)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, rel)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed idea survived: %v", err)
	}
	if got := gittest.Output(t, root, "log", "-1", "--format=%s"); !strings.HasPrefix(got, "harvest: capture follow-ups") {
		t.Fatalf("progress commit missing: %s", got)
	}
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	if err := harvest(); err != nil {
		t.Fatalf("retry harvest: %v", err)
	}
	if got := readFollowups(t, root, "moe", md.ID); !strings.Contains(got, "- [x] `first-fix`") || !strings.Contains(got, "- [x] `second-fix`") {
		t.Fatalf("retry progress = %q", got)
	}
	for _, id := range []string{"first-fix", "second-fix"} {
		if _, err := run.Load(root, "moe", id); err != nil {
			t.Fatal(err)
		}
		log := gittest.Output(t, root, "log", before+"..HEAD", "--format=%s")
		if strings.Count(log, "Open run moe/"+id) != 1 {
			t.Fatalf("idea not opened exactly once: %s", log)
		}
	}
}

func TestUIDoCapturesCreationFailure(t *testing.T) {
	for _, workflow := range []string{"idea", "intent"} {
		for _, locked := range []bool{false, true} {
			name := workflow + "/rollback"
			if locked {
				name = workflow + "/cleanup-failed"
			}
			t.Run(name, func(t *testing.T) {
				root, _ := controlFixture(t)
				body := "echo reject-capture >&2\nexit 1\n"
				if locked {
					// Partial commits hold index.lock while this hook runs.
					body = "if [ -f .git/index.lock ]; then mv .git/index.lock .git/fixture-index.lock; fi\nmkdir .git/index.lock\n" + body
				}
				installCreateFailureHook(t, root, body)
				a := ui.Action{Kind: ui.ActCreate, Project: "moe", Run: "retry-me", Workflow: workflow, Text: "# Mine\n"}
				r := uiDo(context.Background(), root, a)
				if r.OK || r.Uncertain != locked || !strings.Contains(r.Message, "reject-capture") {
					t.Fatalf("create result = %+v", r)
				}
				if _, err := os.Lstat(filepath.Join(root, run.Dir("moe", a.Run))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed capture survived: %v", err)
				}
				if locked {
					if len(r.Lines) == 0 || !strings.Contains(r.Lines[0], "readback:") || strings.Contains(r.Message, "nothing recorded") {
						t.Fatalf("uncertain result lost readback: %+v", r)
					}
					if err := os.Remove(filepath.Join(root, ".git", "index.lock")); err != nil {
						t.Fatal(err)
					}
					return
				}
				if got := gittest.Output(t, root, "ls-files", "--stage", "--", run.Dir("moe", a.Run)); got != "" {
					t.Fatalf("capture index survived: %s", got)
				}
				gittest.Run(t, root, "config", "--unset", "core.hooksPath")
				if r := uiDo(context.Background(), root, a); !r.OK {
					t.Fatalf("retry = %+v", r)
				}
				if b, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", a.Run, workflow))); err != nil || string(b) != a.Text {
					t.Fatalf("retried capture = %q, %v", b, err)
				}
			})
		}
	}
}
