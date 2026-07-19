package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/wiki"
)

// sealFixture builds a bureaucracy where a reflect pass filed its own
// feedback/twin.md and sealed the checkpoint in the same stage-exit
// commit — the aa4d9b97 shape. lastIngestRun is the run the checkpoint
// names; passing "" models a pre-LastIngestRun checkpoint.
func sealFixture(t *testing.T, lastIngestRun string) (root string, cfg wiki.Config, threshold time.Time) {
	t.Helper()
	threshold = time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	root = newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, md := range twinManagedDocs {
		if err := writeWikiDoc(t, twinDir, md.Filename, "# "+md.Title+"\n\nReal content.\n"); err != nil {
			t.Fatal(err)
		}
	}

	writeRunMeta(t, root, "tele", "reflect-run", "twin")
	writeFeedbackAndCommit(t, root, "tele", "reflect-run", "twin",
		"architecture.md's twin-feedback bullet is stale", threshold)

	if err := wiki.WriteCheckpoint(twinDir, wiki.Checkpoint{
		Version:       wiki.CheckpointVersion,
		LastIngestAt:  threshold.Format(time.RFC3339),
		LastIngestRun: lastIngestRun,
		Project:       "tele",
	}); err != nil {
		t.Fatal(err)
	}

	built, err := twinWikiBuilder(root, "tele")
	if err != nil {
		t.Fatal(err)
	}
	return root, *built, threshold
}

// The loader unit test stops at loadTwinFeedback. This drives the whole
// kickoff render: the sealing pass's note must actually reach the
// "Workflow feedback" block the agent reads, under the reworded header.
func TestReflectKickoffSurfacesSealingRunsResidue(t *testing.T) {
	root, cfg, _ := sealFixture(t, "reflect-run")

	got, err := reflectKickoffContext(root, "tele", cfg, false)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}

	for _, want := range []string{
		"### Workflow feedback",
		"including the previous reflect pass's own residue",
		"#### reflect-run (2026-05-10)",
		"architecture.md's twin-feedback bullet is stale",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q; block was:\n%s", want, got)
		}
	}
	if strings.Contains(got, "(no workflow feedback since the last reflect)") {
		t.Errorf("kickoff rendered the empty-feedback branch:\n%s", got)
	}
}

// The ages-out property: once the *next* pass seals, LastIngestRun moves
// on and the previous pass's note fails both filter branches. Nothing
// else changes — same fixture, same threshold, only the checkpoint's
// run name advances. No unit test covers this sequence.
func TestReflectKickoffSealingResidueAgesOutAfterNextPass(t *testing.T) {
	root, cfg, threshold := sealFixture(t, "reflect-run")

	before, err := reflectKickoffContext(root, "tele", cfg, false)
	if err != nil {
		t.Fatalf("reflectKickoffContext (pass N+1): %v", err)
	}
	if !strings.Contains(before, "#### reflect-run") {
		t.Fatalf("pass N+1 kickoff should carry reflect-run's note:\n%s", before)
	}

	// Pass N+1 seals: same LastIngestAt, new LastIngestRun.
	if err := wiki.WriteCheckpoint(cfg.ContentDir, wiki.Checkpoint{
		Version:       wiki.CheckpointVersion,
		LastIngestAt:  threshold.Format(time.RFC3339),
		LastIngestRun: "next-reflect-run",
		Project:       "tele",
	}); err != nil {
		t.Fatal(err)
	}

	after, err := reflectKickoffContext(root, "tele", cfg, false)
	if err != nil {
		t.Fatalf("reflectKickoffContext (pass N+2): %v", err)
	}
	if strings.Contains(after, "#### reflect-run") {
		t.Errorf("reflect-run's note should have aged out once N+1 sealed:\n%s", after)
	}
	if !strings.Contains(after, "(no workflow feedback since the last reflect)") {
		t.Errorf("expected the empty-feedback branch after ageing out:\n%s", after)
	}
}

// A checkpoint written before LastIngestRun existed must behave exactly
// as it did pre-fix: the empty field short-circuits the exception and
// the time filter drops the note.
func TestReflectKickoffEmptyLastIngestRunUnchanged(t *testing.T) {
	root, cfg, _ := sealFixture(t, "")

	got, err := reflectKickoffContext(root, "tele", cfg, false)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}
	if !strings.Contains(got, "(no workflow feedback since the last reflect)") {
		t.Errorf("empty LastIngestRun should drop the note as before:\n%s", got)
	}
}

// pendingTwinObservationsLine shares the loader, so the pulse heartbeat
// now counts the sealing pass's residue. Pin the number: this is a
// user-visible behavior change the design calls a correction, and it
// should fail loudly if it ever silently reverts.
func TestPendingTwinObservationsCountsSealingRunsResidue(t *testing.T) {
	root, _, _ := sealFixture(t, "reflect-run")

	got := pendingTwinObservationsLine(root, "tele")
	want := "Twin-reflect context: 1 twin observation(s) pending since the last reflect, from reflect-run."
	if got != want {
		t.Errorf("pulse line =\n  %q\nwant\n  %q", got, want)
	}
}
