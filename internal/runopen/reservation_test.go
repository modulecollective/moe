package runopen

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func TestMutationsRefuseUnresolvedReservationsWithoutEffects(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "advance", true: "capture close"}[capture], func(t *testing.T) {
			root := stagedRunFixture(t, true)
			if capture {
				md, err := run.Load(root, "alpha", "fix-it")
				if err != nil {
					t.Fatal(err)
				}
				md.Workflow = "idea"
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				gittest.Commit(t, root, "capture")
			}
			branch := "session/alpha/fix-it/retired-doc"
			gittest.Run(t, root, "branch", branch)
			before := gittest.Output(t, root, "rev-parse", "HEAD")
			mdBefore := gittest.Output(t, root, "show", "HEAD:projects/alpha/runs/fix-it/run.json")
			var err error
			if capture {
				err = CloseCapture(root, "alpha", "fix-it")
			} else {
				err = MarkAdvanced(root, "alpha", "fix-it", "design", AdvanceOptions{Validate: func(*run.Metadata) (string, error) { t.Fatal("validator ran past occupancy refusal"); return "", nil }})
			}
			var conflict *session.UnresolvedError
			if !errors.As(err, &conflict) {
				t.Fatalf("error %v", err)
			}
			if before != gittest.Output(t, root, "rev-parse", "HEAD") {
				t.Fatal("refusal committed")
			}
			if mdBefore != gittest.Output(t, root, "show", "HEAD:projects/alpha/runs/fix-it/run.json") {
				t.Fatal("metadata changed")
			}
			if status := gittest.Output(t, root, "status", "--porcelain"); status != "" {
				t.Fatal("refusal dirtied tree:", status)
			}
			gittest.Run(t, root, "branch", "-D", branch)
			if capture {
				err = CloseCapture(root, "alpha", "fix-it")
			} else {
				err = MarkAdvanced(root, "alpha", "fix-it", "design")
			}
			if err != nil {
				t.Fatal("mutation refused after resolution:", err)
			}
		})
	}
}

func TestAdvanceValidationRunsUnderLock(t *testing.T) {
	root := stagedRunFixture(t, true)
	before := gittest.HeadSHA(t, root)
	sentinel := errors.New("gate changed")
	err := MarkAdvanced(root, "alpha", "fix-it", "design", AdvanceOptions{Validate: func(*run.Metadata) (string, error) {
		lockErr := repolock.With(root, repolock.Options{Purpose: "probe"}, func() error { return nil })
		var nested *repolock.NestedError
		if !errors.As(lockErr, &nested) {
			t.Fatalf("validation escaped writer lock: %v", lockErr)
		}
		return "", sentinel
	}})
	if !errors.Is(err, sentinel) || gittest.HeadSHA(t, root) != before {
		t.Fatalf("validation result %v", err)
	}
}

func TestMutationRefReadFailuresFailWithoutEffects(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "advance", true: "capture close"}[capture], func(t *testing.T) {
			root := stagedRunFixture(t, true)
			if capture {
				md, _ := run.Load(root, "alpha", "fix-it")
				md.Workflow = "idea"
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				gittest.Commit(t, root, "capture")
			}
			before := gittest.HeadSHA(t, root)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script := "#!/bin/sh\nif [ \"$1\" = for-each-ref ]; then echo forced-ref-read-failure >&2; exit 1; fi\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if capture {
				err = CloseCapture(root, "alpha", "fix-it")
			} else {
				err = MarkAdvanced(root, "alpha", "fix-it", "design")
			}
			var conflict *session.UnresolvedError
			if err == nil || errors.As(err, &conflict) || !strings.Contains(err.Error(), "forced-ref-read-failure") {
				t.Fatalf("error %v", err)
			}
			if before != gittest.HeadSHA(t, root) || gittest.Output(t, root, "status", "--porcelain") != "" {
				t.Fatal("ref-read failure mutated state")
			}
		})
	}
}
