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

// TestFindOrphanClones is the unit test on the classifier: clone dirs
// for terminal-status runs (merged / closed / promoted) and clones
// without a matching run.json count as orphans; in-progress and pushed
// runs are skipped. Result is sorted (project, run) so the verb's
// output order is stable.
func TestFindOrphanClones(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	// One project at each status the classifier should accept, plus a
	// pushed run we expect to skip.
	trailerstest.SeedRun(t, root, "alpha", "in-flight", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "alpha", "merged-one", "sdlc", run.StatusMerged)
	trailerstest.SeedRun(t, root, "beta", "closed-one", "sdlc", run.StatusClosed)
	trailerstest.SeedRun(t, root, "beta", "pushed-one", "sdlc", run.StatusPushed)
	trailerstest.SeedRun(t, root, "gamma", "promoted-one", "idea", run.StatusPromoted)

	// A clone dir for every run above plus a "phantom" clone whose
	// run.json was never seeded — that one must classify as orphan too.
	for _, c := range [][2]string{
		{"alpha", "in-flight"},
		{"alpha", "merged-one"},
		{"beta", "closed-one"},
		{"beta", "pushed-one"},
		{"gamma", "promoted-one"},
		{"alpha", "phantom"},
	} {
		if err := os.MkdirAll(sandbox.Path(root, c[0], c[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	orphans, err := findOrphanClones(root)
	if err != nil {
		t.Fatalf("findOrphanClones: %v", err)
	}
	gotKeys := make([]string, 0, len(orphans))
	for _, o := range orphans {
		gotKeys = append(gotKeys, o.project+"/"+o.run)
	}
	want := []string{
		"alpha/merged-one",
		"alpha/phantom",
		"beta/closed-one",
		"gamma/promoted-one",
	}
	if len(gotKeys) != len(want) {
		t.Fatalf("orphans: got %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("orphan[%d]: got %q, want %q (full: %v)", i, gotKeys[i], want[i], gotKeys)
		}
	}
}

// TestFindOrphanClonesNoClonesDir covers the freshly-initialised
// bureaucracy where `.moe/clones/` doesn't exist yet — the classifier
// must treat that as "nothing to do" rather than a stat error.
func TestFindOrphanClonesNoClonesDir(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	orphans, err := findOrphanClones(root)
	if err != nil {
		t.Fatalf("findOrphanClones: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("expected no orphans, got %v", orphans)
	}
}

// TestGCClonesRemovesOrphans is the end-to-end happy-path: the verb
// removes terminal-status clones, leaves in-progress and pushed ones
// alone, prints one line per removal, and exits 0.
func TestGCClonesRemovesOrphans(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "alpha", "in-flight", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "alpha", "merged-one", "sdlc", run.StatusMerged)
	trailerstest.SeedRun(t, root, "beta", "closed-one", "sdlc", run.StatusClosed)

	for _, c := range [][2]string{
		{"alpha", "in-flight"},
		{"alpha", "merged-one"},
		{"beta", "closed-one"},
	} {
		if err := os.MkdirAll(sandbox.Path(root, c[0], c[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"gc", "clones"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"removed alpha/merged-one",
		"removed beta/closed-one",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "in-flight") {
		t.Fatalf("output mentions in-flight (should be skipped):\n%s", got)
	}
	if _, err := os.Stat(sandbox.Path(root, "alpha", "merged-one")); !os.IsNotExist(err) {
		t.Fatalf("expected merged-one clone gone, stat err=%v", err)
	}
	if _, err := os.Stat(sandbox.Path(root, "beta", "closed-one")); !os.IsNotExist(err) {
		t.Fatalf("expected closed-one clone gone, stat err=%v", err)
	}
	if _, err := os.Stat(sandbox.Path(root, "alpha", "in-flight")); err != nil {
		t.Fatalf("in-flight clone removed unexpectedly: %v", err)
	}
}

// TestGCClonesNoOrphans confirms the "nothing to do" path prints a
// status line and exits 0 — sync runs this in a loop, so a clean
// bureaucracy must not surface as a failure.
func TestGCClonesNoOrphans(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"gc", "clones"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "no orphan clones") {
		t.Fatalf("expected 'no orphan clones', got %q", out.String())
	}
}

// TestDockerRmRecipeFormat pins the manual-recipe shape: the operator
// copy-pastes this when the in-process + docker-fallback path both
// fail, so the command has to be a complete, executable line.
func TestDockerRmRecipeFormat(t *testing.T) {
	got := dockerRmRecipe("/some/where/clone")
	want := "docker run --rm -v /some/where/clone:/x alpine rm -rf /x"
	if got != want {
		t.Fatalf("recipe: got %q want %q", got, want)
	}
}

