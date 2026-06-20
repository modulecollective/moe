package cli

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestDailyRunCountsProjectFilter pins the histogram source switch: an
// empty filter projects the global DailyRunCount; a project filter reads
// that project's own slice; an unknown project reads as all-zero (which
// the renderer collapses to the (quiet) state).
func TestDailyRunCountsProjectFilter(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	today := "2026-06-20"
	yesterday := "2026-06-19"
	idx := &run.JournalIndex{
		DailyRunCount: map[string]int{today: 5, yesterday: 3},
		DailyRunCountByProject: map[string]map[string]int{
			"alpha": {today: 2},
		},
	}

	// Empty filter → global window: today's bucket is the last element,
	// yesterday's the one before it.
	global := dailyRunCounts(idx, now, "")
	if got := global[dash.HistDays-1]; got != 5 {
		t.Errorf("global today = %d, want 5", got)
	}
	if got := global[dash.HistDays-2]; got != 3 {
		t.Errorf("global yesterday = %d, want 3", got)
	}

	// Project filter → only that project's slice; days it had no activity
	// read as zero even though the global window has counts there.
	alpha := dailyRunCounts(idx, now, "alpha")
	if got := alpha[dash.HistDays-1]; got != 2 {
		t.Errorf("alpha today = %d, want 2", got)
	}
	if got := alpha[dash.HistDays-2]; got != 0 {
		t.Errorf("alpha yesterday = %d, want 0 (no alpha activity)", got)
	}

	// Unknown project → nil slice → every day zero.
	ghost := dailyRunCounts(idx, now, "ghost")
	for i, c := range ghost {
		if c != 0 {
			t.Fatalf("ghost[%d] = %d, want all-zero", i, c)
		}
	}
}

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
