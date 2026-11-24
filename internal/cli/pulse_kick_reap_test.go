package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// The reap hold's timeline, fixed so every assertion in this file reads
// against one clock. reapAt is when the session was reaped; before and
// after straddle it.
var (
	reapBefore = time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	reapAt     = time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	reapAfter  = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
)

// reapHeldPhrase is the wording every surface reporting the hold has to
// carry, spelled out here rather than derived from reapHeldReason so
// the tests pin the operator-facing sentence rather than agreeing with
// whatever the code says today.
const reapHeldPhrase = "its design turn was unfinished and was reaped at 2026-08-22T09:00:00Z" +
	" — an operator touch on the thread releases it"

// runJournalCommit lands one run-scoped journal commit at a chosen
// committer time, with whatever extra trailers the caller needs. extra
// is the whole of the difference between an operator's commit and the
// machine's, which is the difference the release rule turns on.
func runJournalCommit(t *testing.T, root, projectID, runID, subject, extra string, when time.Time) {
	t.Helper()
	stamp := when.UTC().Format(time.RFC3339)
	body := subject + "\n\nMoE-Project: " + projectID + "\nMoE-Run: " + runID + "\n" + extra
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "--allow-empty", "-m", body)
}

// tombstone reaps a run the way tombstoneReap does: the note on
// run.json, plus the consent-stamped journal commit that carries it —
// which lands a second *after* the note's own timestamp. That ordering
// is the whole reason the release rule cannot be "any journal movement
// after At": the reap's own commit is journal movement after At.
func tombstone(t *testing.T, root, projectID, runID, doc string, at time.Time) {
	t.Helper()
	md := loadRun(t, root, projectID, runID)
	md.Reaped = &run.ReapNote{Doc: doc, At: at.UTC().Format(time.RFC3339), Tip: strings.Repeat("a", 40)}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", filepath.Join(run.Dir(projectID, runID), "run.json"))
	stamp := at.Add(time.Second).UTC().Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "-m", "reap: "+projectID+"/"+runID+"\n\nMoE-Project: "+projectID+
		"\nMoE-Run: "+runID+"\nMoE-Consent: dynamic\n")
}

// chainJournalCommit lands one chain commit in the shape `moe chain
// edit` and `moe chain clear` write it: MoE-Chained-To / -Removed lines
// and no MoE-Run trailer at all, because one save touches several
// parents and there is no single run to scope it to. That missing scope
// is exactly why the release rule had to learn to read these.
func chainJournalCommit(t *testing.T, root, subject, lines string, when time.Time) {
	t.Helper()
	stamp := when.UTC().Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "--allow-empty", "-m", subject+"\n\n"+lines)
}
