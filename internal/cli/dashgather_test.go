package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestGatherTimerLogsSegments pins the serve-timing contract: when a
// gather is handed a logging timer, it emits exactly one grep-friendly
// line carrying the label, a total, and a stable key for every coarse
// segment. The segment keys are what an operator greps for to find the
// real hot path (journal= is the suspected one), so a future reshuffle
// that drops or renames a segment fails here.
func TestGatherTimerLogsSegments(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var buf bytes.Buffer
	if _, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{}, newGatherTimer(&buf, "dash")); err != nil {
		t.Fatalf("GatherDashSnapshot: %v", err)
	}

	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one timing line, got %d:\n%s", len(lines), got)
	}
	for _, want := range []string{
		"serve-timing gather=dash",
		"total=",
		"scan=", "journal=", "sessions=", "next=", "chores=", "rows=", "projects=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("timing line missing %q:\n%s", want, got)
		}
	}
}

// TestGatherTimerNilNoOutput pins that the CLI dash path — which passes
// a nil *gatherTimer — neither logs nor panics. The nil receiver is the
// mechanism that keeps `moe dash` an unlogged fresh scan while serve
// shares the same gather body.
func TestGatherTimerNilNoOutput(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	// newGatherTimer(nil, …) must return a nil *gatherTimer whose
	// deferred done()/lap() calls are no-ops.
	if tm := newGatherTimer(nil, "dash"); tm != nil {
		t.Fatalf("expected nil timer for nil writer, got %#v", tm)
	}
	if _, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{}, nil); err != nil {
		t.Fatalf("GatherDashSnapshot with nil timer: %v", err)
	}
}

// TestGatherRunRowLogsRunRowLabel pins that the per-run detail gather
// labels its line gather=run-row, so a slow detail page (which still
// pays the bureaucracy-wide journal index) is distinguishable in the
// log from a home-page render.
func TestGatherRunRowLogsRunRowLabel(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var buf bytes.Buffer
	if _, _, err := GatherRunRow(root, "tele", "fix-it", time.Now().UTC(), &buf); err != nil {
		t.Fatalf("GatherRunRow: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "serve-timing gather=run-row") {
		t.Fatalf("expected gather=run-row timing line, got:\n%s", got)
	}
}
