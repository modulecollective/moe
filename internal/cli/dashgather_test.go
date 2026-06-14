package cli

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestGatherRunRowStaleRunStillFound pins the per-run detail page after
// the dormancy filter's removal. GatherRunRow used to pass All=true to
// bypass the dormancy gate, so a 60-day-old run's detail page kept its
// dash-row Note/When. The gate is gone, so the bypass is gone with it —
// this guards that a stale run is still found (the row, not the empty
// Started/Status fallback) without that crutch.
func TestGatherRunRowStaleRunStillFound(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "tele", "old-one", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update spec",
		"MoE-Run: old-one\nMoE-Document: spec",
		time.Now().UTC().Add(-60*24*time.Hour))

	row, found, err := GatherRunRow(root, "tele", "old-one", time.Now().UTC())
	if err != nil {
		t.Fatalf("GatherRunRow: %v", err)
	}
	if !found {
		t.Fatalf("stale run should still be found by GatherRunRow, got found=false")
	}
	if row.Note == "" || row.When.IsZero() {
		t.Fatalf("stale run row should carry Note/When, got Note=%q When=%v", row.Note, row.When)
	}
}
