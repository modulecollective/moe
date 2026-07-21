package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// seedRun writes a run.json (and optional canvases) straight to disk —
// run.Scan reads nothing else, and going through run.New would drag in
// the whole open pipeline for a fixture.
func seedRun(t *testing.T, root, projectID, id, workflow, status string, created time.Time, canvases map[string]string) {
	t.Helper()
	md := run.Metadata{
		ID:        id,
		Project:   projectID,
		Status:    status,
		Workflow:  workflow,
		Created:   created.Format("2006-01-02"),
		Documents: map[string]*run.Document{},
	}
	for docID, body := range canvases {
		md.Documents[docID] = &run.Document{}
		rel := run.ContentPath(projectID, id, docID)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, run.Dir(projectID, id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSettledRunsBlockSelection pins what the block does and does not
// list. The exclusions are the whole point: a block padded with pulses
// and promoted ideas is one the agent stops reading.
func TestSettledRunsBlockSelection(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "settled-drop", "sdlc", run.StatusClosed, now,
		map[string]string{"idea": "# Tail pulses re-exec the installed binary\n\nbody\n"})
	seedRun(t, root, "moe", "landed-fix", "sdlc", run.StatusMerged, now.AddDate(0, 0, -3), nil)
	seedRun(t, root, "moe", "still-open", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "moe", "old-close", "sdlc", run.StatusClosed, now.AddDate(0, 0, -30), nil)
	seedRun(t, root, "moe", "a-pulse", "pulse", run.StatusClosed, now, nil)
	seedRun(t, root, "moe", "an-idea", "idea", run.StatusPromoted, now, nil)
	seedRun(t, root, "other", "foreign-close", "sdlc", run.StatusClosed, now, nil)

	got := settledRunsBlock(mustPulseScan(t, root), "moe")
	for _, want := range []string{
		"`settled-drop` (sdlc, closed) — Tail pulses re-exec the installed binary",
		"`landed-fix` (sdlc, merged) — landed-fix",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"still-open", "old-close", "a-pulse", "an-idea", "foreign-close"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("block lists %q, which should be excluded:\n%s", unwanted, got)
		}
	}
	// Newest first.
	if strings.Index(got, "settled-drop") > strings.Index(got, "landed-fix") {
		t.Errorf("rows are not newest-first:\n%s", got)
	}
}

// TestSettledRunsBlockEmpty: a project with nothing settled recently
// renders no block at all, rather than a "none" line that costs tokens
// on every quiet pulse.
func TestSettledRunsBlockEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "still-open", "sdlc", run.StatusInProgress, time.Now().Local(), nil)

	if got := settledRunsBlock(mustPulseScan(t, root), "moe"); got != "" {
		t.Errorf("block rendered with nothing settled:\n%s", got)
	}
}

// TestSettledRunsBlockTitleFallsBackToDesign: an sdlc run spawned from
// a pulse has no idea canvas, so the design H1 is the headline.
func TestSettledRunsBlockTitleFallsBackToDesign(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "spawned-fix", "sdlc", run.StatusClosed, time.Now().Local(),
		map[string]string{"design": "intro line\n\n# Stop chains refiling one observation\n"})

	got := settledRunsBlock(mustPulseScan(t, root), "moe")
	if !strings.Contains(got, "— Stop chains refiling one observation") {
		t.Errorf("design H1 not used as the title:\n%s", got)
	}
}

// TestSettledRunsBlockCapped keeps a busy fortnight from crowding out
// the rest of the kickoff.
func TestSettledRunsBlockCapped(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	for i := range settledRunsCap + 5 {
		seedRun(t, root, "moe", "closed-"+string(rune('a'+i)), "sdlc", run.StatusClosed, now, nil)
	}
	got := settledRunsBlock(mustPulseScan(t, root), "moe")
	if n := strings.Count(got, "(sdlc, closed)"); n != settledRunsCap {
		t.Errorf("block has %d rows, want the cap of %d", n, settledRunsCap)
	}
}

// TestPulseKickoffCarriesSettledBlock pins the wiring, not just the
// renderer — the block has to reach the survey's first turn.
func TestPulseKickoffCarriesSettledBlock(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRun(t, root, "moe", "settled-drop", "sdlc", run.StatusClosed, time.Now().Local(), nil)

	got := pulseKickoffWithContext(root, "moe", "pulse-x", io.Discard)
	if !strings.HasPrefix(got, pulseKickoff) {
		t.Fatal("kickoff no longer leads with the static prompt")
	}
	if !strings.Contains(got, "Recently settled runs") || !strings.Contains(got, "settled-drop") {
		t.Errorf("settled-runs block missing from the kickoff:\n%s", got)
	}
}

// mustPulseScan builds the one-per-sweep scan the context blocks read
// from. The blocks take it rather than a root so a sweep pays for one
// read instead of five.
func mustPulseScan(t *testing.T, root string) *pulseScan {
	t.Helper()
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatalf("newPulseScan(%q) failed", root)
	}
	return sc
}
