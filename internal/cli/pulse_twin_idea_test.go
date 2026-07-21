package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// seedTwinTaggedIdea parks an in-progress idea tagged `(twin)` — the
// shape harvestFollowups writes for a capture an agent nominated a
// reflect for.
func seedTwinTaggedIdea(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, projectID, run.Options{
		ID:        slug,
		Workflow:  "idea",
		PromoteTo: "twin",
		SeedDocs:  map[string]string{"idea": "# " + slug + "\n\nThe boundary moved; the twin says otherwise.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// seedUnrecordedTwinEdit makes DetectUnrecordedEdits flag vision.md: a
// managed doc whose latest commit carries no `MoE-Workflow: twin`
// trailer, plus a checkpoint to give the detector a baseline.
func seedUnrecordedTwinEdit(t *testing.T, root, projectID string) {
	t.Helper()
	twinDir := filepath.Join(root, "projects", projectID, wiki.TwinDirRel)
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# Vision\n\nEdited out of band.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", filepath.Join("projects", projectID, wiki.TwinDirRel, "vision.md"))
	gittest.Run(t, root, "commit", "-m", "operator edits vision")
	if err := wiki.WriteCheckpoint(twinDir, wiki.Checkpoint{
		Version:      wiki.CheckpointVersion,
		LastIngestAt: "2026-04-01T18:00:00Z",
		Project:      projectID,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTwinTaggedIdeaMintsReflectAndTakesNoSeed: a `(twin)`-tagged idea
// is a reflect nomination, not a run to mint from the idea's canvas.
// With no pass open it mints `reflect-YYYY-MM-DD` — not the idea's slug
// — and the reflect's run dir carries no seed doc, since a reflect reads
// the managed docs and never a promoted idea's canvas.
func TestTwinTaggedIdeaMintsReflectAndTakesNoSeed(t *testing.T) {
	root := twinSpawnFixture(t)
	idea := seedTwinTaggedIdea(t, root, "moe", "bring-the-twin-current")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "bring-the-twin-current", Why: "boundary move"},
	}, io.Discard, &errb)

	destID := minted["bring-the-twin-current"]
	if destID == "" {
		t.Fatalf("minted = %v, want the idea's slug keyed to the reflect; stderr=%q", minted, errb.String())
	}
	if !strings.HasPrefix(destID, "reflect") {
		t.Errorf("destination %q, want the harness-dated reflect slug, not the idea's", destID)
	}
	dest, err := run.Load(root, "moe", destID)
	if err != nil {
		t.Fatal(err)
	}
	if dest.Workflow != "twin" {
		t.Errorf("destination workflow = %q, want twin", dest.Workflow)
	}
	// No seed: the reflect's run dir has no documents/ tree at all.
	if entries, err := os.ReadDir(filepath.Join(root, run.Dir("moe", destID), "documents")); err == nil && len(entries) > 0 {
		t.Errorf("reflect run dir has seeded docs %v, want none", entries)
	}
	// The idea still carries its canvas, reachable through the edge.
	src, err := run.Load(root, "moe", idea.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != run.StatusPromoted {
		t.Errorf("idea status = %q, want promoted", src.Status)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.PromotedTo["moe/"+idea.ID]; got != "moe/"+destID {
		t.Errorf("PromotedTo edge = %q, want moe/%s", got, destID)
	}
}

// TestTwinTaggedIdeaMapsOntoOpenReflect: with a pass already open the
// nomination maps onto it — no second mint, and the idea's promotion
// edge points at the run that will actually do the work.
func TestTwinTaggedIdeaMapsOntoOpenReflect(t *testing.T) {
	root := twinSpawnFixture(t)
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")
	seedTwinTaggedIdea(t, root, "moe", "bring-the-twin-current")

	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "bring-the-twin-current", Why: "boundary move"},
	}, io.Discard, io.Discard)

	if got := minted["bring-the-twin-current"]; got != "reflect-2026-05-14" {
		t.Fatalf("minted = %v, want the spec on the open reflect", minted)
	}
	if tw := twinRuns(t, root, "moe"); len(tw) != 1 {
		t.Fatalf("twin runs = %v, want no second reflect", tw)
	}
	src, err := run.Load(root, "moe", "bring-the-twin-current")
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != run.StatusPromoted {
		t.Errorf("idea status = %q, want promoted onto the open reflect", src.Status)
	}
}

// TestPulseMapsOntoOpenReflectDespiteUnrecordedEdits pins the guard
// order: with a pass open *and* managed docs edited out of band, the
// open pass is the answer — it is where those edits get landed — so the
// pulse maps rather than warn-and-skipping.
func TestPulseMapsOntoOpenReflectDespiteUnrecordedEdits(t *testing.T) {
	root := twinSpawnFixture(t)
	seedUnrecordedTwinEdit(t, root, "moe")
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift"},
	}, io.Discard, &errb)

	if got := minted["twin-refresh"]; got != "reflect-2026-05-14" {
		t.Fatalf("minted = %v, want the nomination mapped; stderr=%q", minted, errb.String())
	}
	if strings.Contains(errb.String(), "the operator lands those first") {
		t.Errorf("stderr = %q, want the map, not the unrecorded refusal", errb.String())
	}
}

// TestVerbRefusesInProgressAheadOfUnrecordedEdits is the same guard
// order seen from the verb, which still refuses rather than resolving:
// with both conditions true the operator is told to resume the open
// pass, the more actionable of the two messages.
func TestVerbRefusesInProgressAheadOfUnrecordedEdits(t *testing.T) {
	root := twinSpawnFixture(t)
	seedUnrecordedTwinEdit(t, root, "moe")
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")

	canonical, err := twinWikiBuilder(root, "moe")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mintReflectRun(root, "moe", "", "", canonical, io.Discard, io.Discard)
	var refusal *reflectRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a reflect refusal", err)
	}
	if refusal.kind != reflectRefusalInProgress {
		t.Fatalf("refusal kind = %v, want in-progress ahead of unrecorded", refusal.kind)
	}
	if got := refusal.redirect("twin", "moe"); !strings.Contains(got, "resume it") {
		t.Errorf("redirect = %q, want the resume redirect", got)
	}
}

// TestPulseUnrecordedEditsWithNoReflectStillSkips: the residual edge
// this run accepts. Nothing to map onto and minting is refused, so the
// nomination resolves to nothing and the sweep says why.
func TestPulseUnrecordedEditsWithNoReflectStillSkips(t *testing.T) {
	root := twinSpawnFixture(t)
	seedUnrecordedTwinEdit(t, root, "moe")

	var errb bytes.Buffer
	minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{
		{Slug: "twin-refresh", Workflow: "twin", Why: "drift"},
	}, io.Discard, &errb)

	if len(minted) != 0 {
		t.Fatalf("minted = %v, want nothing while the edits block the mint", minted)
	}
	if tw := twinRuns(t, root, "moe"); len(tw) != 0 {
		t.Fatalf("twin runs = %v, want none", tw)
	}
	if !strings.Contains(errb.String(), "the operator lands those first") {
		t.Errorf("stderr = %q, want the unrecorded refusal named", errb.String())
	}
}
