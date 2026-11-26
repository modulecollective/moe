package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/servepause"
	"github.com/modulecollective/moe/internal/session"
)

func TestHumanSessionAndOwnerAllowSiblingSurveyAndRide(t *testing.T) {
	defer withRideMode(rideDynamic)()
	defer withClockInvoked()()
	root, stages := nextFixture(t)
	a, b := mintIn(t, root, "moe", "human-work"), mintIn(t, root, "moe", "machine-work")
	human, err := session.Open(root, "moe", a, "design")
	if err != nil {
		t.Fatal(err)
	}
	stopBeat, err := session.Hold(human, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopBeat)
	owner := holdElsewhere(t, root, "moe", a)
	path := filepath.Join(human.WorktreePath, "human-draft")
	if err := os.WriteFile(path, []byte("unfinished human work"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch := gittest.Output(t, root, "rev-parse", human.Branch)
	claimPath := filepath.Join(root, ".moe", "claims", "moe", a, "design.json")
	claim, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	gate := newHeartbeatGate(root)
	if ids := sweepIDs(gate.Due(io.Discard)); len(ids) != 1 || ids[0] != "moe" {
		t.Fatalf("human session held sibling: %v", ids)
	}
	gate.Dispatching()
	code, log := sweepWithGate(t, root, `{"status":"ok","next":{"run":"moe/`+b+`","why":"sibling is ready"}}`)
	if code != 0 || len(*stages) == 0 || (*stages)[0].runID != b {
		t.Fatalf("sibling ride: %d %+v %s", code, *stages, log)
	}
	gate.Swept("_cabinet", true)
	if body, err := os.ReadFile(path); err != nil || string(body) != "unfinished human work" {
		t.Fatalf("human draft changed: %s %v", body, err)
	}
	afterClaim, err := os.ReadFile(claimPath)
	if err != nil || string(afterClaim) != string(claim) || gittest.Output(t, root, "rev-parse", human.Branch) != branch {
		t.Fatal("human reservation/claim changed")
	}
	if busy, err := runowner.Busy(root, "moe", a); err != nil || !busy {
		t.Fatalf("human owner lost: %v %v", busy, err)
	}
	*stages = nil
	code, log = sweepWithGate(t, root, `{"status":"ok","next":{"run":"moe/`+a+`","why":"must refuse"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(log, "live session") {
		t.Fatalf("same-run pick: %d %+v %s", code, *stages, log)
	}
	// Between-stage owner-only holds remain after the session reservation ends.
	stopBeat()
	if err := session.Abandon(human); err != nil {
		t.Fatal(err)
	}
	if hold := kickFloorHold(root, "moe/"+a, mustPulseScan(t, root)); !strings.Contains(hold, "owned") {
		t.Fatalf("owner-only hold lost: %s", hold)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	// Orphan refs and ambiguous claims still hold only their own run.
	gittest.Run(t, root, "branch", human.Branch)
	if hold := kickFloorHold(root, "moe/"+a, mustPulseScan(t, root)); !strings.Contains(hold, "live session") {
		t.Fatalf("orphan hold: %s", hold)
	}
}

func TestRepairClaimAllowsOtherRunAndExcludesOnlyOwnReservation(t *testing.T) {
	for _, kind := range []string{"other-run", "same-run", "own-reservation", "own-plus-other-document"} {
		t.Run(kind, func(t *testing.T) {
			root, pr := pushedRepairFixture(t)
			own, err := session.Open(root, "moe", "fix-ci", "build")
			if err != nil {
				t.Fatal(err)
			}
			reserved := own
			switch kind {
			case "other-run":
				seedRun(t, root, "moe", "human", "sdlc", run.StatusInProgress, time.Now(), nil)
				human, err := session.Open(root, "moe", "human", "design")
				if err != nil {
					t.Fatal(err)
				}
				stop, err := session.Hold(human, false)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(stop)
			case "same-run":
				reserved = nil
			case "own-plus-other-document":
				if _, err := session.Open(root, "moe", "fix-ci", "design"); err != nil {
					t.Fatal(err)
				}
			}
			var claimErr error
			if reserved == nil {
				_, claimErr = claimCIRepair(root, pr)
			} else {
				_, claimErr = claimCIRepair(root, pr, reserved)
			}
			wantHold := kind == "same-run" || kind == "own-plus-other-document"
			if wantHold {
				if claimErr == nil || !strings.Contains(claimErr.Error(), "open session") {
					t.Fatalf("same-run hold lost: %v", claimErr)
				}
			} else if claimErr != nil {
				t.Fatalf("eligible repair held: %v", claimErr)
			}
		})
	}
}

func TestPauseDoesNotAcknowledgeUnseenHumanOrRideWork(t *testing.T) {
	for _, clean := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "clean"}[clean], func(t *testing.T) {
			root := quietFixture(t)
			groomFixture(t, root, "fix-a")
			g := newHeartbeatGate(root)
			g.Due(io.Discard)
			g.Dispatching()
			before := g.tips["moe"]
			// The already-admitted survey may finish even after pause commits.
			if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
				t.Fatal(err)
			}
			journalCommit(t, root, "moe", "human: unseen work", "")
			journalCommit(t, root, "moe", "ride: unseen work", "MoE-Consent: dynamic\nMoE-Workflow: sdlc")
			g.Swept("_cabinet", clean)
			if g.tips["moe"] != before || g.surveyed["moe"] != "" {
				t.Fatal("paused completion acknowledged unseen work")
			}
			if _, err := servepause.Set(root, false, nil, time.Now); err != nil {
				t.Fatal(err)
			}
			if got := sweepIDs(g.Due(io.Discard)); len(got) != 1 {
				t.Fatalf("unseen work lost: %v", got)
			}
			if got := sweepIDs(newHeartbeatGate(root).Due(io.Discard)); len(got) != 1 {
				t.Fatalf("restart lost work: %v", got)
			}
		})
	}
}
