package git

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestUnbornHEAD(t *testing.T) {
	for _, state := range []string{"fresh", "orphan", "committed", "detached", "non-branch"} {
		t.Run(state, func(t *testing.T) {
			dir := gittest.Init(t)
			if state != "fresh" {
				gittest.Commit(t, dir, "seed")
			}
			switch state {
			case "orphan":
				gittest.Run(t, dir, "checkout", "--orphan", "empty")
			case "detached":
				gittest.Run(t, dir, "checkout", "--detach")
			case "non-branch":
				gittest.Run(t, dir, "symbolic-ref", "HEAD", "refs/tags/absent")
			}
			got, err := UnbornHEAD(dir)
			want := state == "fresh" || state == "orphan"
			if err != nil || got != want {
				t.Fatalf("UnbornHEAD = %v, %v; want %v, nil", got, err, want)
			}
		})
	}
}

func TestUnbornHEADFailures(t *testing.T) {
	for _, state := range []string{"non-repo", "malformed-head", "malformed-ref", "missing-object", "missing-git"} {
		t.Run(state, func(t *testing.T) {
			dir := gittest.Init(t)
			switch state {
			case "non-repo":
				dir = t.TempDir()
				// GOTMPDIR may be inside another repository in a stage.
				t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
			case "malformed-head":
				writeFile(t, filepath.Join(dir, ".git", "HEAD"), "broken\n")
			case "malformed-ref", "missing-object":
				ref := gittest.Output(t, dir, "symbolic-ref", "HEAD")
				data := "broken\n"
				if state == "missing-object" {
					data = strings.Repeat("1", 40) + "\n"
				}
				writeFile(t, filepath.Join(dir, ".git", filepath.FromSlash(ref)), data)
			case "missing-git":
				t.Setenv("PATH", t.TempDir())
			}
			got, err := UnbornHEAD(dir)
			if got || err == nil {
				t.Fatalf("UnbornHEAD = %v, %v; want false and an error", got, err)
			}
			if state == "missing-git" {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("missing executable error lost: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "fatal:") {
					t.Fatalf("Git exit error or diagnostic lost: %v", err)
				}
			}
		})
	}
}
