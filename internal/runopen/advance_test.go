package runopen

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// stagedRunFixture seeds an in-progress sdlc run whose design stage has
// a committed work turn — the "worked, not advanced" shape the mark
// exists for.
func stagedRunFixture(t *testing.T, workTurn bool) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	projectDir := filepath.Join(root, "projects", "alpha")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project.json"),
		[]byte(`{"id":"alpha","remote":"git@example.test:p.git","default_branch":"main","submodule":"m/p"}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	seedRunMetadata(t, root, "alpha", "fix-it", "sdlc", run.StatusInProgress)
	gittest.Commit(t, root, "open: alpha/fix-it")
	if workTurn {
		trailerstest.CommitWorkTurnAt(t, root, "alpha", "fix-it", "sdlc", "design",
			time.Now().Add(-time.Hour))
	}
	return root
}

// TestMarkAdvancedWritesTheMarker is the shape stageSatisfied reads: an
// empty commit whose `advance: <doc>` subject and MoE-* trailers scope it
// to one stage of one run.
func TestMarkAdvancedWritesTheMarker(t *testing.T) {
	root := stagedRunFixture(t, true)

	if err := MarkAdvanced(root, "alpha", "fix-it", "design", io.Discard, io.Discard); err != nil {
		t.Fatalf("MarkAdvanced: %v", err)
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"advance: design",
		"MoE-Run: fix-it",
		"MoE-Project: alpha",
		"MoE-Workflow: sdlc",
		"MoE-Document: design",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
	// A click is the operator's own act, not a ride at some level, so
	// the marker carries no consent trailer.
	if contains(msg, "MoE-Consent") {
		t.Errorf("web advance mark must stamp no consent\n%s", msg)
	}
	// Empty: under this marker there is no file to change, so the marker
	// *is* the message.
	if diff := gittest.Output(t, root, "show", "--name-only", "--format=", "HEAD"); diff != "" {
		t.Errorf("marker commit touched files: %q", diff)
	}
	// stageSatisfied wants the marker at least as recent as the stage's
	// own work turn, which a marker written now always is.
	_, advWhen, err := run.LatestAdvanceSHA(root, "alpha", "fix-it", "design")
	if err != nil {
		t.Fatal(err)
	}
	_, turnWhen, err := run.LatestWorkTurnSHA(root, "alpha", "fix-it", "design")
	if err != nil {
		t.Fatal(err)
	}
	if advWhen.Before(turnWhen) {
		t.Errorf("marker at %s predates the work turn at %s — it would satisfy nothing", advWhen, turnWhen)
	}
}

// TestMarkAdvancedRefusesAStageWithNoWorkTurn: stageSatisfied wants a
// marker *and* a turn, so a mark on a never-worked stage is one the
// ladder ignores. Refuse rather than leave a marker that means nothing.
func TestMarkAdvancedRefusesAStageWithNoWorkTurn(t *testing.T) {
	root := stagedRunFixture(t, false)

	err := MarkAdvanced(root, "alpha", "fix-it", "design", io.Discard, io.Discard)
	if !errors.Is(err, ErrNotAdvanceable) {
		t.Fatalf("want ErrNotAdvanceable, got %v", err)
	}
	if sha, _, _ := run.LatestAdvanceSHA(root, "alpha", "fix-it", "design"); sha != "" {
		t.Error("refused mark still wrote a marker commit")
	}
}

// TestMarkAdvancedRefusesATerminalRun: consent to move something that
// has already stopped isn't consent to anything.
func TestMarkAdvancedRefusesATerminalRun(t *testing.T) {
	root := stagedRunFixture(t, true)
	seedRunMetadata(t, root, "alpha", "fix-it", "sdlc", run.StatusMerged)

	err := MarkAdvanced(root, "alpha", "fix-it", "design", io.Discard, io.Discard)
	if !errors.Is(err, ErrNotAdvanceable) {
		t.Fatalf("want ErrNotAdvanceable, got %v", err)
	}
}

// TestMarkAdvancedReportsAMissingRun: a replayed POST against a run that
// is gone gets run.ErrRunNotFound, which the route maps to 404 — the
// same classification every other runopen verb gives it.
func TestMarkAdvancedReportsAMissingRun(t *testing.T) {
	root := stagedRunFixture(t, true)

	err := MarkAdvanced(root, "alpha", "ghost", "design", io.Discard, io.Discard)
	if !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

// TestMarkAdvancedRefusesAnEmptyStage: the route re-derives the stage
// server-side and "" is what it gets for a run with nothing left to
// mark. Refusing here keeps a marker with an empty document trailer —
// which no stage would ever match — out of the journal.
func TestMarkAdvancedRefusesAnEmptyStage(t *testing.T) {
	root := stagedRunFixture(t, true)

	err := MarkAdvanced(root, "alpha", "fix-it", "", io.Discard, io.Discard)
	if !errors.Is(err, ErrNotAdvanceable) {
		t.Fatalf("want ErrNotAdvanceable, got %v", err)
	}
}
