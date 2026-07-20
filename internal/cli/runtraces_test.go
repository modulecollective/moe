package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// TestScanChecklistDisplayIsLenient pins the display scan's contract
// against parseChecklist's: it yields checked entries (the audit trail
// the harvest parser deliberately skips), it never errors, and a line
// that misses the grammar comes back as raw text rather than taking the
// section down with it.
func TestScanChecklistDisplayIsLenient(t *testing.T) {
	body := `<!--
followups.md — header the editor pop injected.
-->

# Follow-ups

- [ ] ` + "`still-open`" + ` — Not harvested yet

  Why: the reason nobody got to it.

- [x] ` + "`landed`" + ` — Promoted last close
- [x] ` + "`beta/cross`" + ` — Routed to another project
- [X] this line never matched the grammar
`
	got := scanChecklistDisplay([]byte(body))
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d: %+v", len(got), got)
	}
	if got[0].done || got[0].slug != "still-open" || got[0].title != "Not harvested yet" {
		t.Errorf("open entry: %+v", got[0])
	}
	if got[0].body != "Why: the reason nobody got to it." {
		t.Errorf("open entry body: %q", got[0].body)
	}
	if !got[1].done || got[1].slug != "landed" || got[1].title != "Promoted last close" {
		t.Errorf("checked entry: %+v", got[1])
	}
	if !got[2].done || got[2].slug != "beta/cross" {
		t.Errorf("cross-project entry: %+v", got[2])
	}
	if !got[3].done || got[3].raw != "- [X] this line never matched the grammar" || got[3].slug != "" {
		t.Errorf("malformed entry should render raw: %+v", got[3])
	}
}

// TestScanChecklistDisplayEmptyFiles: the shapes that must yield no
// entries at all, so the run page renders no section rather than an
// empty one.
func TestScanChecklistDisplayEmptyFiles(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"header only":  "<!--\nfollowups.md — nothing here.\n-->\n",
		"heading only": "# Follow-ups\n",
		// parseChecklist fails loud on this (an agent forgot the grammar);
		// display just shows nothing.
		"prose only": "We should probably clean up the foo helper.\n",
	} {
		if got := scanChecklistDisplay([]byte(body)); len(got) != 0 {
			t.Errorf("%s: want no entries, got %+v", name, got)
		}
	}
}

// seedTraceRun writes a run.json so run.Load resolves it.
func seedTraceRun(t *testing.T, root, projectID, runID, status string) {
	t.Helper()
	md := &run.Metadata{
		ID:        runID,
		Project:   projectID,
		Status:    status,
		Workflow:  "idea",
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// TestGatherRunTracesResolvesLandedFollowups: the whole point of the
// section. A harvested line carries the resolved slug, which *is* the
// idea run's ID — so each checked entry links to where it landed and
// badges that run's current status. The cases that must not link:
// an open entry (nothing landed), a checked entry whose run is gone
// (the operator checked it by hand to drop it), and a malformed line.
func TestGatherRunTracesResolvesLandedFollowups(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTraceRun(t, root, "alpha", "src", run.StatusClosed)
	seedTraceRun(t, root, "alpha", "landed", run.StatusInProgress)
	seedTraceRun(t, root, "beta", "cross", run.StatusClosed)

	writeTraceFile(t, root, run.FollowupsPath("alpha", "src"), `# Follow-ups

- [ ] `+"`still-open`"+` — Not harvested yet
- [x] `+"`landed`"+` — Promoted last close
- [x] `+"`beta/cross`"+` — Routed to another project
- [x] `+"`vanished`"+` — Dropped by hand
- [x] this line never matched the grammar
`)

	got, err := GatherRunTraces(root, "alpha", "src")
	if err != nil {
		t.Fatalf("GatherRunTraces: %v", err)
	}
	if len(got.Followups) != 5 {
		t.Fatalf("want 5 followups, got %d: %+v", len(got.Followups), got.Followups)
	}
	if fu := got.Followups[0]; fu.Done || fu.TargetURL != "" {
		t.Errorf("open entry must not link: %+v", fu)
	}
	if fu := got.Followups[1]; fu.TargetURL != "/run/alpha/landed" || fu.TargetStatus != run.StatusInProgress {
		t.Errorf("landed entry: %+v", fu)
	}
	if fu := got.Followups[2]; fu.TargetURL != "/run/beta/cross" || fu.TargetStatus != run.StatusClosed {
		t.Errorf("cross-project entry must link into its own project: %+v", fu)
	}
	if fu := got.Followups[3]; fu.TargetURL != "" || !fu.Done {
		t.Errorf("missing target must render checked and unlinked: %+v", fu)
	}
	if fu := got.Followups[4]; fu.Raw == "" || fu.TargetURL != "" {
		t.Errorf("malformed line must render raw: %+v", fu)
	}
}

// TestGatherRunTracesResolvesLore: lore entries link to the promoted
// file at the bureaucracy root, existence-checked, with no status badge
// (lore has no lifecycle).
func TestGatherRunTracesResolvesLore(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTraceRun(t, root, "alpha", "src", run.StatusClosed)
	writeTraceFile(t, root, filepath.Join(wiki.LoreDirRel, "promoted-fact.md"), "# Promoted\n")
	writeTraceFile(t, root, run.FeedbackPath("alpha", "src", "lore"), `- [x] `+"`promoted-fact`"+` — A portable fact
- [x] `+"`never-promoted`"+` — Checked but no file
- [ ] `+"`pending-fact`"+` — Not promoted yet
`)

	got, err := GatherRunTraces(root, "alpha", "src")
	if err != nil {
		t.Fatalf("GatherRunTraces: %v", err)
	}
	if len(got.Lore) != 3 {
		t.Fatalf("want 3 lore entries, got %d", len(got.Lore))
	}
	if l := got.Lore[0]; l.TargetURL != "/lore/promoted-fact" || l.TargetStatus != "" {
		t.Errorf("promoted lore: %+v", l)
	}
	if l := got.Lore[1]; l.TargetURL != "" {
		t.Errorf("missing lore file must not link: %+v", l)
	}
	if l := got.Lore[2]; l.Done || l.TargetURL != "" {
		t.Errorf("open lore entry must not link: %+v", l)
	}
}

// TestGatherRunTracesAbsentFilesYieldNothing: most runs leave no
// traces, and the page must render no sections rather than empty ones.
func TestGatherRunTracesAbsentFilesYieldNothing(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTraceRun(t, root, "alpha", "src", run.StatusClosed)
	got, err := GatherRunTraces(root, "alpha", "src")
	if err != nil {
		t.Fatalf("GatherRunTraces: %v", err)
	}
	if len(got.Followups) != 0 || len(got.Lore) != 0 || got.TwinNote != nil {
		t.Errorf("want empty traces, got %+v", got)
	}
}

// writeTraceFile writes a root-relative file, creating parents. Not
// committed — callers that need a git time commit it themselves.
func writeTraceFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitAt commits everything staged in root with a fixed committer
// date, so LastFileActivity (which reads %ct) is deterministic.
func commitAt(t *testing.T, root, msg string, when time.Time) {
	t.Helper()
	stamp := when.Format(time.RFC3339)
	gittest.Run(t, root, "add", "-A")
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "-m", msg)
}

// writeCheckpoint commits one revision of a project's digital-twin
// checkpoint.json — one reflect pass's seal.
func writeCheckpoint(t *testing.T, root, projectID, lastIngestAt, lastIngestRun string) {
	t.Helper()
	cp := wiki.Checkpoint{
		Version:       wiki.CheckpointVersion,
		LastIngestAt:  lastIngestAt,
		LastIngestRun: lastIngestRun,
		Project:       projectID,
	}
	b, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("projects", projectID, wiki.TwinDirRel, "checkpoint.json")
	writeTraceFile(t, root, rel, string(b))
	commitAt(t, root, "seal "+lastIngestRun, mustTime(t, lastIngestAt))
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// TestGatherRunTracesTwinNoteStatus walks the reflect attribution the
// run page derives from checkpoint history — the only record of which
// pass ingested a note, since nothing marks a twin note consumed.
//
// The carve-out case is the subtle one, and mirrors loadTwinFeedback: a
// reflect pass seals the checkpoint and writes its own feedback/twin.md
// in the same commit, so that note can't post-date the threshold it
// created. It belongs to the *next* pass.
func TestGatherRunTracesTwinNoteStatus(t *testing.T) {
	noteAt := "2026-01-15T00:00:00Z"
	for _, tc := range []struct {
		name        string
		runID       string
		seals       [][2]string // {lastIngestAt, lastIngestRun}, oldest first
		wantRefl    bool
		wantRunLink string
	}{
		{
			name:  "pending when no seal covers the note",
			runID: "src",
			seals: [][2]string{{"2025-12-01T00:00:00Z", "reflect-old"}},
		},
		{
			name:  "pending when the project has no checkpoint history",
			runID: "src",
		},
		{
			name:        "attributed to the earliest seal at or after the note",
			runID:       "src",
			seals:       [][2]string{{"2025-12-01T00:00:00Z", "reflect-old"}, {"2026-02-01T00:00:00Z", "reflect-new"}, {"2026-03-01T00:00:00Z", "reflect-later"}},
			wantRefl:    true,
			wantRunLink: "reflect-new",
		},
		{
			name:        "a pass does not ingest the note it filed itself",
			runID:       "reflect-new",
			seals:       [][2]string{{"2026-02-01T00:00:00Z", "reflect-new"}, {"2026-03-01T00:00:00Z", "reflect-later"}},
			wantRefl:    true,
			wantRunLink: "reflect-later",
		},
		{
			name:     "seal without a recorded run reflects with no link",
			runID:    "src",
			seals:    [][2]string{{"2026-02-01T00:00:00Z", ""}},
			wantRefl: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			seedTraceRun(t, root, "alpha", tc.runID, run.StatusClosed)
			writeTraceFile(t, root, run.FeedbackPath("alpha", tc.runID, "twin"),
				"architecture.md understates the serve seam.\n")
			commitAt(t, root, "note", mustTime(t, noteAt))
			for _, s := range tc.seals {
				writeCheckpoint(t, root, "alpha", s[0], s[1])
			}

			got, err := GatherRunTraces(root, "alpha", tc.runID)
			if err != nil {
				t.Fatalf("GatherRunTraces: %v", err)
			}
			if got.TwinNote == nil {
				t.Fatal("want a twin note trace, got nil")
			}
			if got.TwinNote.Reflected != tc.wantRefl {
				t.Errorf("Reflected = %v, want %v", got.TwinNote.Reflected, tc.wantRefl)
			}
			if got.TwinNote.ReflectRun != tc.wantRunLink {
				t.Errorf("ReflectRun = %q, want %q", got.TwinNote.ReflectRun, tc.wantRunLink)
			}
		})
	}
}

// TestGatherRunTracesUncommittedTwinNoteIsPending: an uncommitted note
// is invisible to the journal, so reflect can't have ingested it — the
// same when.IsZero() skip loadTwinFeedback makes.
func TestGatherRunTracesUncommittedTwinNoteIsPending(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTraceRun(t, root, "alpha", "src", run.StatusInProgress)
	writeCheckpoint(t, root, "alpha", "2026-06-01T00:00:00Z", "reflect-new")
	// Written after the seal and never committed.
	writeTraceFile(t, root, run.FeedbackPath("alpha", "src", "twin"), "A fresh observation.\n")

	got, err := GatherRunTraces(root, "alpha", "src")
	if err != nil {
		t.Fatalf("GatherRunTraces: %v", err)
	}
	if got.TwinNote == nil || got.TwinNote.Reflected {
		t.Errorf("uncommitted note should read pending, got %+v", got.TwinNote)
	}
}
