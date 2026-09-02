package cli

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
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

// TestDesignOnlyIsSkippedOrIgnored walks the four shapes that may not
// carry the bit. Two skip the entry outright, because honouring the
// field there would consume something the operator is holding — a live
// idea's brake, or the runs queued behind a thread position — or would
// mint the one-line idea the rung exists to replace. Two only warn: on
// a chore or twin entry the run's shape comes from elsewhere entirely,
// and dropping the operator's chore over a meaningless bool would cost
// more than the warn.
func TestDesignOnlyIsSkippedOrIgnored(t *testing.T) {
	t.Run("no design body", func(t *testing.T) {
		root := spawnFixture(t)
		spec := designOnlySpec("baseline-drift")
		spec.Design = ""
		var errb bytes.Buffer

		if minted := mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{spec}, io.Discard, &errb); len(minted) != 0 {
			t.Errorf("minted %v, want nothing — the brief is the point", minted)
		}
		if !strings.Contains(errb.String(), "design_only with no design body") {
			t.Errorf("stderr = %q, want the skip named", errb.String())
		}
	})

	t.Run("slug already live", func(t *testing.T) {
		root := spawnFixture(t)
		seedRun(t, root, "moe", "baseline-drift", "idea", run.StatusInProgress, time.Now().Local(),
			map[string]string{"idea": "# Baseline drift\n\ncaptured\n"})
		var errb bytes.Buffer

		if minted := mintSpecs(root, "moe", "pulse-one",
			[]pulseRunSpec{designOnlySpec("baseline-drift")}, io.Discard, &errb); len(minted) != 0 {
			t.Errorf("minted %v, want nothing — design-only opens fresh slugs only", minted)
		}
		if !strings.Contains(errb.String(), "already names live work") {
			t.Errorf("stderr = %q, want the skip named", errb.String())
		}
		// The capture is untouched: the refusal exists so a design-only
		// promotion can't spend an idea past the operator's brake.
		md, err := run.Load(root, "moe", "baseline-drift")
		if err != nil {
			t.Fatal(err)
		}
		if md.Status != run.StatusInProgress {
			t.Errorf("idea status = %q, want it left alone", md.Status)
		}
	})

	t.Run("at a thread position", func(t *testing.T) {
		root := spawnFixture(t)
		t.Chdir(root)
		spec := designOnlySpec("baseline-drift")
		var errb bytes.Buffer

		groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
			Status:  "ok",
			Threads: []pulseThread{{Runs: []pulseThreadEntry{{Spec: &spec}}}},
		}, io.Discard, &errb)

		if len(groups) != 1 || len(groups[0].Runs) != 0 {
			t.Errorf("groups = %+v, want the position left empty", groups)
		}
		if !strings.Contains(errb.String(), "design_only at a thread position") {
			t.Errorf("stderr = %q, want the skip named", errb.String())
		}
	})

	t.Run("on a twin entry", func(t *testing.T) {
		root := spawnFixture(t)
		var errb bytes.Buffer
		mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{{
			Workflow: "twin", Why: "the boundary moved", DesignOnly: true,
		}}, io.Discard, &errb)

		if !strings.Contains(errb.String(), "ignoring design_only") {
			t.Errorf("stderr = %q, want the bit warned and ignored on a reflect", errb.String())
		}
	})

	t.Run("on a chore entry", func(t *testing.T) {
		root := spawnFixture(t)
		var errb bytes.Buffer
		mintSpecs(root, "moe", "pulse-one", []pulseRunSpec{{
			Chore: "no-such-chore", Why: "the condition holds", DesignOnly: true,
		}}, io.Discard, &errb)

		if !strings.Contains(errb.String(), "ignoring design_only on chore entry") {
			t.Errorf("stderr = %q, want the bit warned and ignored on a chore", errb.String())
		}
	})
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

// TestKickFloorHoldOnADesignOnlySpawn is the whole lifecycle at the one
// seam that decides whether the machine may move: admitted once for the
// turn it was minted for, held afterwards, admitted once more per
// undelivered note, held again the moment the note is consumed.
func TestKickFloorHoldOnADesignOnlySpawn(t *testing.T) {
	turn := func(t *testing.T, root string) {
		t.Helper()
		trailerstest.CommitWorkTurnAt(t, root, "moe", "brief", "sdlc", "design",
			time.Now().Local().Add(-time.Hour))
	}
	note := func(t *testing.T, root string) {
		t.Helper()
		if _, err := input.Add(root, "moe", "brief", "narrow it to the read path", io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name  string
		setUp func(t *testing.T, root string)
		want  string
	}{
		{
			name:  "fresh mint rides",
			setUp: func(t *testing.T, root string) {},
		},
		{
			name:  "turn closed, held",
			setUp: turn,
			want:  designOnlyHeldReason,
		},
		{
			name: "an undelivered note buys one more turn",
			setUp: func(t *testing.T, root string) {
				turn(t, root)
				note(t, root)
			},
		},
		{
			name: "delivery consumes it",
			setUp: func(t *testing.T, root string) {
				turn(t, root)
				note(t, root)
				f, err := input.Load(root, "moe", "brief")
				if err != nil {
					t.Fatal(err)
				}
				ids := []int{}
				for _, e := range f.Pending() {
					ids = append(ids, e.ID)
				}
				if err := input.MarkDelivered(root, "moe", "brief", "design", ids, "", io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
			},
			want: designOnlyHeldReason,
		},
		{
			name: "advanced, settled, no hold",
			setUp: func(t *testing.T, root string) {
				advanceAt(t, root, "moe", "brief", "design", time.Now().Local().Add(-time.Hour))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, _, _ := kickFixture(t)
			seedDesignOnlyRun(t, root, "moe", "brief")
			tc.setUp(t, root)
			groomed := groomChains(root, "moe", "pulse-groom",
				nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

			if got := kickFloorHold(root, "moe/brief", groomed); got != tc.want {
				t.Errorf("kickFloorHold = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDesignOnlyTakesNoSafeModeExemption: a one-stage design ride is
// still a start, so `safe` binds it like any other. The note the
// operator pushes from their phone is the mark, which is what keeps
// "write me a design for this" a one-act request under safe.
func TestDesignOnlyTakesNoSafeModeExemption(t *testing.T) {
	for _, tc := range []struct {
		name string
		note bool
		want string
	}{
		{name: "no mark, held", want: "is held by safe mode — no operator mark (an advance, a tag, a chore, or an undelivered note licenses it)"},
		{name: "a note is a mark", note: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			defer withClockInvoked()()
			root, _, _ := kickFixture(t)
			seedDesignOnlyRun(t, root, "moe", "brief")
			if err := project.SetMode(root, "moe", project.ModeSafe); err != nil {
				t.Fatal(err)
			}
			if tc.note {
				if _, err := input.Add(root, "moe", "brief", "go ahead", io.Discard, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			groomed := groomChains(root, "moe", "pulse-groom",
				nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

			if got := kickFloorHold(root, "moe/brief", groomed); got != tc.want {
				t.Errorf("kickFloorHold = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKickPlanMarksTheBoundedRide: the plan is what the sweep stamps
// before it executes, so a ride the operator will read about hours
// later has to say it was the short one.
func TestKickPlanMarksTheBoundedRide(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	seedDesignOnlyRun(t, root, "moe", "brief")
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	plan := planKick(root, wantKick(groomed, groomedThread{Root: "moe/brief"}))
	if len(plan.Steps) != 1 || !plan.Steps[0].OneStage {
		t.Fatalf("plan = %+v, want one step bounded to a single stage", plan.Steps)
	}
	if got := planSteps(plan); !slices.Equal(got, []string{"moe/brief|gate|queued, design only — one stage, then it parks"}) {
		t.Errorf("plan = %v, want the section to say the ride is bounded", got)
	}
	if !strings.Contains(renderKickSection(plan), "design only") {
		t.Errorf("section =\n%s\nwant the bound named", renderKickSection(plan))
	}
}

// TestSelfKickRidesADesignOnlySpawnExactlyOneStage is the behaviour the
// whole rung is for: the sweep spends one design turn and stops. A
// second stage or a push here would be the blast radius the design-only
// bar was priced against.
func TestSelfKickRidesADesignOnlySpawnExactlyOneStage(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages, pushes := kickFixture(t)
	seedDesignOnlyRun(t, root, "moe", "brief")
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/brief"}), io.Discard, &errb)

	if got := kickStages(*stages); !slices.Equal(got, []string{"brief:design"}) {
		t.Fatalf("dispatched %v, want exactly the design stage; stderr=%q", got, errb.String())
	}
	if len(*pushes) != 0 {
		t.Errorf("pushes = %v, want none — a design-only ride never ships", *pushes)
	}
	if !strings.Contains(errb.String(), "kicking moe/brief (dynamic, design only)") {
		t.Errorf("stderr = %q, want the bounded ride announced", errb.String())
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
}

// TestChainStateBlockNamesADesignOnlyHead: the survey reads this block
// when it decides where to put work. "only a seed" would send it
// looking for a design someone is going to write; the design-only
// phrase names the actual repair, which is to move the members out and
// leave the run to its operator.
func TestChainStateBlockNamesADesignOnlyHead(t *testing.T) {
	root := newTestBureaucracy(t)
	seedDesignOnlyRun(t, root, "moe", "brief")
	seedRun(t, root, "moe", "readme-update", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# Update the README\n\nthe chore's own prompt\n"})
	chainEdge(t, root, "moe/brief", "moe/readme-update")

	got := chainStateBlock(mustPulseScan(t, root), "moe")
	if !strings.Contains(got, "held: a design-only spawn — it rides one design turn and parks for the operator") {
		t.Errorf("block renders a design-only head as an ordinary seed:\n%s", got)
	}
	if strings.Contains(got, "held: only a seed") {
		t.Errorf("block still calls a written brief a seed:\n%s", got)
	}
}
