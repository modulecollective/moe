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

// TestDispatchCascadeTailAfterRedeferSaysNothingMore: `!!` whose push
// defers to headless recovery twice stops without shipping, and
// dispatchCascade re-enters the chain anchored at "push" — sdlc's
// terminal stage. That tail used to hit a terminal-stage branch that
// announced "push sealed — run `moe sdlc close …`" (a `[Y/n/x]` close
// prompt with Y default at a real terminal), which is a lie: nothing
// shipped, and a reflex Enter would have closed an unshipped run. The
// cascade summary is now the last word.
func TestDispatchCascadeTailAfterRedeferSaysNothingMore(t *testing.T) {
	root, md := seedCascadeEntryRun(t, run.StatusInProgress)
	stubOpenSdlcStage(t, nil)
	deferred := &PushDeferredError{Recovery: "rebase-conflict", Project: "tele", Run: "fix-it"}
	stubPushFromCascadeSeq(t, []pushOutcome{
		{exit: 0, deferred: deferred},
		{exit: 0, deferred: deferred},
	})

	var stdout, stderr bytes.Buffer
	code := dispatchCascade("!!", "code", root, md, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit=%d, want 0 (both recoveries exited cleanly); stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	wantSummary := "cascade tele/fix-it: code ok · test ok · review ok · push deferred to recovery (rebase conflict) · push deferred to recovery (rebase conflict) — stopped\n"
	if !strings.HasSuffix(got, wantSummary) {
		t.Fatalf("stdout = %q, want it to end with the cascade summary %q", got, wantSummary)
	}
	if strings.Contains(got, "sealed") || strings.Contains(got, "close") {
		t.Fatalf("stdout = %q, want no close nudge or prompt after a cascade that never shipped", got)
	}
}
