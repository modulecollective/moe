package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// designOnlySpec is the shape the gate has to carry for the bit to
// survive: a fresh slug and a brief. Every skip rule below is this spec
// with one thing changed.
func designOnlySpec(slug string) pulseRunSpec {
	return pulseRunSpec{
		Slug:       slug,
		Title:      "A finding worth an hour",
		Why:        "speculative — the report baseline drifts after a skipped sweep",
		Design:     "# The brief\n\nWhat we saw, and what we want decided.\n",
		DesignOnly: true,
	}
}

// seedDesignOnlyRun writes a machine-minted design-only run straight to
// disk. seedRun is the fixture every floor test uses; this adds the two
// fields that make the run what it is.
func seedDesignOnlyRun(t *testing.T, root, projectID, id string) {
	t.Helper()
	seedRun(t, root, projectID, id, "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# The brief\n\nWhat we saw.\n"})
	md, err := run.Load(root, projectID, id)
	if err != nil {
		t.Fatal(err)
	}
	md.SpawnedBy = projectID + "/pulse-one"
	md.DesignOnly = true
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// TestDesignOnlySpecMintsTheBitOntoTheRun: the gate field reaches
// run.json, which is where every reader of it looks. The trailers are
// deliberately unchanged — the run is still machine-minted and still
// says so.
func TestDesignOnlySpecMintsTheBitOntoTheRun(t *testing.T) {
	root := spawnFixture(t)
	var errb bytes.Buffer

	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{designOnlySpec("baseline-drift")}, io.Discard, &errb)
	id := minted["baseline-drift"]
	if id == "" {
		t.Fatalf("nothing minted; stderr=%q", errb.String())
	}
	md, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	if !md.DesignOnly {
		t.Errorf("run.json design_only = false, want the spec's bit carried onto the run")
	}
	if md.SpawnedBy == "" {
		t.Errorf("spawned_by = %q, want the run to still mark itself machine-minted", md.SpawnedBy)
	}
	if !strings.Contains(errb.String(), "spawned design-only run") {
		t.Errorf("stderr = %q, want the spawn line to say the ride is short", errb.String())
	}
}

// TestRootDesignSettledOnADesignOnlySpawn pins the leg-1 narrowing: the
// machine-minted shortcut no longer covers a run whose seed was a
// brief, so such a run walks the same past-first-stage road an
// operator's own `--from-idea` run walks. The last case is the
// regression guard — an ordinary spawn is settled exactly as before.
func TestRootDesignSettledOnADesignOnlySpawn(t *testing.T) {
	for _, tc := range []struct {
		name           string
		designOnly     bool
		turn, advanced bool
		wantSettled    bool
		wantTurnClosed bool
	}{
		{name: "design-only, seed only", designOnly: true},
		{name: "design-only, turn closed", designOnly: true, turn: true, wantTurnClosed: true},
		{name: "design-only, advanced", designOnly: true, turn: true, advanced: true, wantSettled: true},
		{name: "ordinary spawn, seed only", wantSettled: true},
		{name: "ordinary spawn, turn closed", turn: true, wantSettled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			now := time.Now().Local()
			seedRun(t, root, "moe", "brief", "sdlc", run.StatusInProgress, now,
				map[string]string{"design": "# The brief\n\nbody\n"})
			md, err := run.Load(root, "moe", "brief")
			if err != nil {
				t.Fatal(err)
			}
			md.SpawnedBy = "moe/pulse-one"
			md.DesignOnly = tc.designOnly
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.advanced:
				advanceAt(t, root, "moe", "brief", "design", now.Add(-time.Hour))
			case tc.turn:
				trailerstest.CommitWorkTurnAt(t, root, "moe", "brief", "sdlc", "design", now.Add(-time.Hour))
			}
			idx, err := run.BuildJournalIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			md, err = run.Load(root, "moe", "brief")
			if err != nil {
				t.Fatal(err)
			}

			settled, turnClosed := rootDesignSettled(root, md, idx)
			if settled != tc.wantSettled || turnClosed != tc.wantTurnClosed {
				t.Errorf("rootDesignSettled = (%v, %v), want (%v, %v)",
					settled, turnClosed, tc.wantSettled, tc.wantTurnClosed)
			}
		})
	}
}

// TestParkedKickableThreadAgreesWithTheFloor: the tick's pre-ask and the
// kick's admit have to answer the same question, or the heartbeat
// re-offers a board the kick then holds — the shape that stranded a run
// for two days on 2026-08-13.
func TestParkedKickableThreadAgreesWithTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn bool
		want string
	}{
		{name: "owed its design turn", want: "moe/brief"},
		{name: "turn closed, nothing to cause", turn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := kickFixture(t)
			seedDesignOnlyRun(t, root, "moe", "brief")
			if tc.turn {
				trailerstest.CommitWorkTurnAt(t, root, "moe", "brief", "sdlc", "design",
					time.Now().Local().Add(-time.Hour))
			}

			if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != tc.want {
				t.Errorf("parkedKickableThread = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParkedKickableThreadSkipsAnOccupiedDesignOnlySpawn: the admit is
// the floor's, occupancy included. A live session at design is the
// operator inside the run, or a corpse branch — either way not
// something a sweep may drive into.
func TestParkedKickableThreadSkipsAnOccupiedDesignOnlySpawn(t *testing.T) {
	root, _, _ := kickFixture(t)
	seedDesignOnlyRun(t, root, "moe", "brief")
	if _, err := session.Open(root, "moe", "brief", "design"); err != nil {
		t.Fatal(err)
	}

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want nothing — the run is occupied", got)
	}
}

// TestStageLocationSectionOnADesignOnlyRun: the paragraph is the only
// thing telling the agent nobody is coming to walk its open questions
// with it, and it fires nowhere else — a design turn on an ordinary run
// gets the plain block, and the successor stages of a design-only run
// (reachable once the operator advances it) do too.
func TestStageLocationSectionOnADesignOnlyRun(t *testing.T) {
	const marker = "This run is **design-only**"
	designOnly := &run.Metadata{Project: "moe", ID: "brief", Workflow: "sdlc", DesignOnly: true}
	ordinary := &run.Metadata{Project: "moe", ID: "fix", Workflow: "sdlc"}

	if got := stageLocationSection(designOnly, "design"); !strings.Contains(got, marker) {
		t.Errorf("design-only design block =\n%s\nwant the paragraph", got)
	}
	if got := stageLocationSection(designOnly, "code"); strings.Contains(got, marker) {
		t.Errorf("design-only code block =\n%s\nwant no paragraph past design", got)
	}
	if got := stageLocationSection(ordinary, "design"); strings.Contains(got, marker) {
		t.Errorf("ordinary design block =\n%s\nwant no paragraph", got)
	}

	// The idea is now a second carrier of the bit, and the idea
	// workflow's single stage is also its first — so without the
	// successor gate an operator sitting in `moe idea edit --chat` on
	// their own design-only tagged idea would be told nobody is going
	// to walk their open questions with them.
	taggedIdea := &run.Metadata{Project: "moe", ID: "worth-a-think", Workflow: "idea", DesignOnly: true}
	if got := stageLocationSection(taggedIdea, "idea"); strings.Contains(got, marker) {
		t.Errorf("design-only idea block =\n%s\nwant no paragraph — a capture stage has no ride to bound", got)
	}
}
