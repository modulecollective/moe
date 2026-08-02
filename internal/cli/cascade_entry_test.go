package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// seedCascadeEntryRun stands up an isolated bureaucracy holding one
// sdlc run at status, and returns the root plus the *stale* in-memory
// metadata a chain prompt would still be carrying: in_progress, loaded
// before whatever terminated the run on disk. That gap is the whole
// subject of these tests.
func seedCascadeEntryRun(t *testing.T, status string) (string, *run.Metadata) {
	t.Helper()
	root := isolateCascadeMoeHome(t)
	if err := run.Save(root, &run.Metadata{
		ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: status,
	}); err != nil {
		t.Fatal(err)
	}
	return root, &run.Metadata{
		ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress,
	}
}

// TestDispatchCascadeRefusesTerminalRun: a bang answer typed at a
// chain prompt that has been sitting open while the run was scuttled,
// merged, or promoted from somewhere else must not drive a single
// stage. The prompt's md still says in_progress; dispatchCascade
// re-reads run.json and refuses.
func TestDispatchCascadeRefusesTerminalRun(t *testing.T) {
	for _, status := range []string{run.StatusMerged, run.StatusClosed, run.StatusPromoted} {
		t.Run(status, func(t *testing.T) {
			root, stale := seedCascadeEntryRun(t, status)
			stages := stubOpenSdlcStage(t, nil)
			pushes := stubPushFromCascade(t, 0, nil)

			var stdout, stderr bytes.Buffer
			code := dispatchCascade("!!!", "code", root, stale, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr.String())
			}
			if want := "cascade: tele/fix-it is " + status + "; nothing to cascade"; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
			if len(*stages) != 0 {
				t.Errorf("dispatched %+v against a %s run, want none", *stages, status)
			}
			if len(*pushes) != 0 {
				t.Errorf("shipped %+v a %s run, want no push", *pushes, status)
			}
		})
	}
}

// TestDispatchCascadeRefusesPushedRun: pushed is its own refusal —
// the run isn't sealed, but a cascade can't drive it (push is the last
// stage, and its work is already out for review).
func TestDispatchCascadeRefusesPushedRun(t *testing.T) {
	root, stale := seedCascadeEntryRun(t, run.StatusPushed)
	stages := stubOpenSdlcStage(t, nil)

	var stdout, stderr bytes.Buffer
	code := dispatchCascade("!", "code", root, stale, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr.String())
	}
	if want := "cascade: tele/fix-it already pushed; cascade cannot drive a pushed run"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if len(*stages) != 0 {
		t.Errorf("dispatched %+v against a pushed run, want none", *stages)
	}
}

// TestDispatchCascadeRefusesVanishedRun: the run directory going away
// between prompt and answer is the same stale-load class, and the same
// answer — refuse loudly rather than cascade against metadata that no
// longer describes anything on disk.
func TestDispatchCascadeRefusesVanishedRun(t *testing.T) {
	root, stale := seedCascadeEntryRun(t, run.StatusInProgress)
	if err := os.RemoveAll(filepath.Join(root, run.Dir("tele", "fix-it"))); err != nil {
		t.Fatal(err)
	}
	stages := stubOpenSdlcStage(t, nil)

	var stdout, stderr bytes.Buffer
	code := dispatchCascade("!!", "code", root, stale, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr.String())
	}
	if got := stderr.String(); !strings.HasPrefix(got, "cascade: ") || !strings.Contains(got, "tele/fix-it") {
		t.Errorf("stderr = %q, want a cascade-prefixed refusal naming the run", got)
	}
	if len(*stages) != 0 {
		t.Errorf("dispatched %+v against a vanished run, want none", *stages)
	}
}

// The live-run half of the guard — that an in_progress run still
// cascades, on freshly loaded metadata — is pinned by the existing
// dispatchCascade tests (TestDispatchCascadeBlockedReviewParksToPrompt
// and the cascade-flag suite), which now seed run.json for the load
// this guard added.
