package runopen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// failCommits makes every commit in root fail at commit-msg.
func failCommits(t *testing.T, root string) {
	t.Helper()
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	t.Cleanup(func() { gittest.Run(t, root, "config", "--unset", "core.hooksPath") })
}

func canvas(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, run.ContentPath("alpha", "my-idea", "idea")))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A checked save lands only over the body the editor opened; anything
// else is refused with the canvas, journal and tree untouched.
func TestEditCaptureCheckedRefusesIntervening(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
	if err := EditCaptureChecked(root, "alpha", "my-idea", "# one\n", "# two\n"); err != nil {
		t.Fatal(err)
	}
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	// A second editor opened at "# one" is now stale.
	err := EditCaptureChecked(root, "alpha", "my-idea", "# one\n", "# three\n")
	if !errors.Is(err, ErrCaptureChanged) {
		t.Fatalf("stale save = %v", err)
	}
	if canvas(t, root) != "# two\n" || gittest.Output(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("stale save wrote")
	}
	if err := EditCaptureChecked(root, "alpha", "my-idea", "# two\n", "# two\n"); !errors.Is(err, run.ErrNothingToCommit) {
		t.Fatalf("unchanged save = %v", err)
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("unchanged save committed")
	}

	// Closed in between: refused as no longer a capture.
	if err := CloseCapture(root, "alpha", "my-idea"); err != nil {
		t.Fatal(err)
	}
	if err := EditCaptureChecked(root, "alpha", "my-idea", "# two\n", "# four\n"); !errors.Is(err, ErrNotCapture) {
		t.Fatalf("closed save = %v", err)
	}
}

// A session reservation (an open edit --chat) refuses a checked save.
func TestEditCaptureCheckedRefusesReservedSession(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
	gittest.Run(t, root, "branch", "session/alpha/my-idea/idea")
	var conflict *session.UnresolvedError
	if err := EditCaptureChecked(root, "alpha", "my-idea", "# one\n", "# two\n"); !errors.As(err, &conflict) {
		t.Fatalf("reserved save = %v", err)
	}
	if canvas(t, root) != "# one\n" {
		t.Fatal("reserved save wrote")
	}
}

// A failed commit puts the canvas and its index entry back, and leaves
// the operator's unrelated dirty work alone.
func TestEditCaptureCommitFailureRestores(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
	stray := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(stray, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	failCommits(t, root)
	err := EditCaptureChecked(root, "alpha", "my-idea", "# one\n", "# two\n")
	if err == nil || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("failed commit = %v", err)
	}
	if canvas(t, root) != "# one\n" || gittest.Output(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("failed commit left the edit behind")
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); st != "?? notes.txt" {
		t.Fatalf("tree after rollback = %q", st)
	}
}

func TestCloseCaptureCommitFailureRestores(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
	failCommits(t, root)
	if err := CloseCapture(root, "alpha", "my-idea"); err == nil {
		t.Fatal("close with a failing commit succeeded")
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil || md.Status != run.StatusInProgress {
		t.Fatalf("after failed close: %+v, %v", md, err)
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("tree after rollback = %q", st)
	}
}

func TestTagIdeaCommitFailureRestoresAndRefusesPromoted(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
	failCommits(t, root)
	if err := TagIdea(root, "alpha", "my-idea", "sdlc", true); err == nil {
		t.Fatal("tag with a failing commit succeeded")
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil || md.PromoteTo != "" || md.DesignOnly {
		t.Fatalf("after failed tag: %+v, %v", md, err)
	}
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	md.Status = run.StatusPromoted
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "promote")
	if err := TagIdea(root, "alpha", "my-idea", "sdlc", false); !errors.Is(err, ErrNotTaggableIdea) {
		t.Fatalf("tag of promoted idea = %v", err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", t.TempDir()) // cleanup's --unset needs a value
}

// The operator may have a staged version of the very file a writer
// touches, and a different unstaged one on top. A failed commit must
// hand both back, not reset the index entry to HEAD.
func TestCaptureCommitFailureRestoresStagedAndUnstaged(t *testing.T) {
	cases := []struct {
		name  string
		want  string // the refusal; close checks a clean tree before writing
		rel   string
		write func(root string) error
	}{
		{"edit", "nothing recorded", run.ContentPath("alpha", "my-idea", "idea"), func(root string) error {
			return EditCapture(root, "alpha", "my-idea", "# mine\n")
		}},
		{"tag", "nothing recorded", filepath.Join(run.Dir("alpha", "my-idea"), "run.json"), func(root string) error {
			return TagIdea(root, "alpha", "my-idea", "sdlc", false)
		}},
		{"close", "uncommitted changes", filepath.Join(run.Dir("alpha", "my-idea"), "run.json"), func(root string) error {
			return CloseCapture(root, "alpha", "my-idea")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := newIdeaBureaucracy(t, "alpha", "my-idea", "# one\n")
			abs := filepath.Join(root, c.rel)
			orig, err := os.ReadFile(abs)
			if err != nil {
				t.Fatal(err)
			}
			// Staged and working versions both differ from HEAD and from
			// each other; each stays parseable for the writers that read it.
			staged, working := string(orig)+"\n", string(orig)+"\n\n"
			if err := os.WriteFile(abs, []byte(staged), 0o644); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "add", "--", c.rel)
			stagedBlob := gittest.Output(t, root, "rev-parse", ":"+c.rel)
			if err := os.WriteFile(abs, []byte(working), 0o644); err != nil {
				t.Fatal(err)
			}
			head := gittest.Output(t, root, "rev-parse", "HEAD")
			failCommits(t, root)
			if err := c.write(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("failed commit = %v", err)
			}
			if got := gittest.Output(t, root, "rev-parse", ":"+c.rel); got != stagedBlob {
				t.Fatalf("index after rollback = %s, want the staged blob %s", got, stagedBlob)
			}
			if b, _ := os.ReadFile(abs); string(b) != working {
				t.Fatalf("working file after rollback = %q, want %q", b, working)
			}
			if gittest.Output(t, root, "rev-parse", "HEAD") != head {
				t.Fatal("failed commit moved HEAD")
			}
		})
	}
}
