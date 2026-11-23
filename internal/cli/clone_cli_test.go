package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func TestCloneListSortedWithStatus(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "alpha", "in-flight", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "alpha", "merged-one", "sdlc", run.StatusMerged)
	trailerstest.SeedRun(t, root, "beta", "closed-one", "sdlc", run.StatusClosed)

	for _, c := range [][2]string{
		{"alpha", "in-flight"},
		{"alpha", "merged-one"},
		{"alpha", "phantom"},
		{"beta", "closed-one"},
	} {
		if err := os.MkdirAll(sandbox.Path(root, c[0], c[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"clone", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), got)
	}
	wantPrefix := []string{
		"alpha/in-flight\t" + run.StatusInProgress + "\t",
		"alpha/merged-one\t" + run.StatusMerged + "\t",
		"alpha/phantom\t(missing)\t",
		"beta/closed-one\t" + run.StatusClosed + "\t",
	}
	for i, want := range wantPrefix {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d: got %q, want prefix %q", i, lines[i], want)
		}
	}
}

// TestCloneListNoClones: an empty bureaucracy must surface "no clones"
// rather than a stat error or an empty body.
func TestCloneListNoClones(t *testing.T) {
	newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"clone", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "no clones") {
		t.Fatalf("expected 'no clones', got %q", out.String())
	}
}

// TestDockerRmRecipeFormat pins the manual-recipe shape: the operator
// copy-pastes this when the in-process + docker-fallback path both
// fail, so the command has to be a complete, executable line.
