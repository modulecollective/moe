package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestOpenReconcilesDiagnosticOnlyMain(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstaged", true: "staged"}[staged], func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			commitInWorktree(t, s.WorktreePath, canvasRel, "original", "session checkpoint")
			if err := os.WriteFile(filepath.Join(s.WorktreePath, canvasRel), []byte("pending edits"), 0o644); err != nil {
				t.Fatal(err)
			}
			if staged {
				gittest.Run(t, s.WorktreePath, "add", canvasRel)
			}
			gittest.WriteAndCommit(t, root, "projects/moe/runs/r1/diagnostics/design/attempt/failure.json", "{}", "diagnostic: missing canvas")
			_, err = Open(root, "moe", "r1", "design")
			if staged {
				var refusal *RebaseFailureError
				if !errors.As(err, &refusal) || !refusal.Dirty {
					t.Fatalf("staged intent must still refuse: %v", err)
				}
				if !git.Probe(s.WorktreePath, "diff", "--quiet") || git.Probe(s.WorktreePath, "diff", "--cached", "--quiet") {
					t.Fatal("staged intent was changed")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !git.Probe(root, "merge-base", "--is-ancestor", "main", s.Branch) {
					t.Fatal("session remains stale after diagnostics")
				}
			}
			b, err := os.ReadFile(filepath.Join(s.WorktreePath, canvasRel))
			if err != nil || string(b) != "pending edits" {
				t.Fatalf("pending edits lost: %q %v", b, err)
			}
		})
	}
}
