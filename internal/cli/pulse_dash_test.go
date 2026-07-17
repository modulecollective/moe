package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestParsePullNextWellFormed: entries inside the section parse in file
// order (= rank order), slug and trimmed reason preserved.
func TestParsePullNextWellFormed(t *testing.T) {
	content := "# Pulse\n\n## Backlog hygiene\n\n- `not-a-pick` — this bullet is in another section\n\n" +
		"## Pull next\n\n- `first-idea` — unblocked by the merge\n- `second-idea` —  padded reason  \n"
	picks := parsePullNext([]byte(content))
	if len(picks) != 2 {
		t.Fatalf("picks = %d, want 2: %+v", len(picks), picks)
	}
	if picks[0].Slug != "first-idea" || picks[0].Reason != "unblocked by the merge" {
		t.Errorf("picks[0] = %+v", picks[0])
	}
	if picks[1].Slug != "second-idea" || picks[1].Reason != "padded reason" {
		t.Errorf("picks[1] = %+v (reason should be trimmed)", picks[1])
	}
}

// TestParsePullNextSkipsMalformed: the parse is lenient — a bullet with
// the wrong separator (a hyphen, not an em-dash), a bare bullet, or a
// slug missing its backticks is skipped, while the well-formed lines
// around it still parse.
func TestParsePullNextSkipsMalformed(t *testing.T) {
	content := "## Pull next\n\n" +
		"- `good-one` — kept\n" +
		"- `hyphen-sep` - dropped, hyphen not em-dash\n" +
		"- bare bullet with no slug\n" +
		"- missing-backticks — dropped\n" +
		"just prose in the section\n" +
		"- `good-two` — also kept\n"
	picks := parsePullNext([]byte(content))
	if len(picks) != 2 {
		t.Fatalf("picks = %d, want 2 (only the well-formed lines): %+v", len(picks), picks)
	}
	if picks[0].Slug != "good-one" || picks[1].Slug != "good-two" {
		t.Fatalf("picks = %+v, want good-one then good-two", picks)
	}
}

// TestParsePullNextMissingSection: no "## Pull next" heading yields no
// picks, and a pick-shaped line outside the section is ignored. A later
// heading also ends the section.
func TestParsePullNextMissingSection(t *testing.T) {
	if picks := parsePullNext([]byte("# Pulse\n\n## Surveyed\n\n- `x` — not a pull-next line\n")); len(picks) != 0 {
		t.Fatalf("missing section should yield no picks, got %+v", picks)
	}
	// A heading after the section ends it — a pick-shaped line below the
	// next heading must not be captured.
	content := "## Pull next\n\n- `inside` — captured\n\n## Notes\n\n- `outside` — must be ignored\n"
	picks := parsePullNext([]byte(content))
	if len(picks) != 1 || picks[0].Slug != "inside" {
		t.Fatalf("picks = %+v, want only the in-section entry", picks)
	}
}

func writePulseCanvas(t *testing.T, root, project, runID, body string) {
	t.Helper()
	path := filepath.Join(root, run.ContentPath(project, runID, pulseDoc))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGatherPullNextUsesLatestRun: gatherPullNext reads only the most
// recent pulse run per project (any status), and ignores non-pulse
// runs.
func TestGatherPullNextUsesLatestRun(t *testing.T) {
	root := t.TempDir()
	writePulseCanvas(t, root, "moe", "pulse-old", "## Pull next\n\n- `stale-pick` — from the old sweep\n")
	writePulseCanvas(t, root, "moe", "pulse-new", "## Pull next\n\n- `fresh-pick` — from the new sweep\n")

	mds := []*run.Metadata{
		{ID: "pulse-old", Project: "moe", Workflow: pulseWorkflow, Status: run.StatusClosed},
		{ID: "pulse-new", Project: "moe", Workflow: pulseWorkflow, Status: run.StatusInProgress},
		{ID: "some-run", Project: "moe", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	idx := &run.JournalIndex{LastActivity: map[string]time.Time{
		"moe/pulse-old": time.Unix(100, 0),
		"moe/pulse-new": time.Unix(200, 0),
	}}

	picks := gatherPullNext(root, mds, idx)
	if len(picks) != 1 {
		t.Fatalf("picks = %+v, want 1 (from the latest run only)", picks)
	}
	if picks[0].Slug != "fresh-pick" || picks[0].Project != "moe" {
		t.Fatalf("picks[0] = %+v, want fresh-pick in moe", picks[0])
	}
}
