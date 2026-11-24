package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
)

const unreadyDesignCanvas = designCanvasSkeleton + "\nStill needs a contract decision.\n"

// machineParkFixture is controlFixture's run, spawned by a sweep so the
// floor would let a pulse pick it.
func machineParkFixture(t *testing.T) (string, *run.Metadata) {
	t.Helper()
	root, md := controlFixture(t)
	md.SpawnedBy = "moe/pulse-one"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	return root, md
}

// A stage that failed is a failure even when its gate is also unready:
// the park needs the turn itself to have run cleanly.
func TestRideFailedStageOverUnreadyGateStillFails(t *testing.T) {
	root, md := machineParkFixture(t)
	stubSdlcStageWritingCanvas(t, root, md, "design", unreadyDesignCanvas)
	inner := openSdlcStage
	openSdlcStage = func(s, p, r string, h bool, o, e io.Writer) int {
		inner(s, p, r, h, o, e)
		return 1
	}
	if code := rideRun(root, md.Project, md.ID, rideDynamic, false, io.Discard, io.Discard); code != 1 {
		t.Fatalf("failed design turn: exit %d, want 1", code)
	}
}

// A full ride whose review loop is spent and still blocked parks; the
// same walk under the operator's `!!` keeps its exit 1.
func TestRideSpentReviewLoopParks(t *testing.T) {
	root, md := machineParkFixture(t)
	writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
	stubSdlcStageWritingCanvas(t, root, md, "review", escalatedReviewCanvas)
	realGate := checkCascadeStageGate
	checkCascadeStageGate = func(wf *Workflow, md *run.Metadata, stage string, stderr io.Writer) (bool, int) {
		if stage != "review" {
			return true, 0
		}
		return realGate(wf, md, stage, stderr)
	}
	t.Cleanup(func() { checkCascadeStageGate = realGate })
	kicks := stubHeadlessKickback(t, 0, nil)
	pushes := stubPushFromCascade(t, 0, nil)

	var errb bytes.Buffer
	if code := rideRun(root, md.Project, md.ID, rideDynamic, false, io.Discard, &errb); code != exitParked {
		t.Fatalf("spent review ride: exit %d, want exitParked; stderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "parked at its review gate") {
		t.Fatalf("stderr does not say where it parked:\n%s", errb.String())
	}
	if len(*kicks) != 0 || len(*pushes) != 0 {
		t.Fatalf("escalated review kicked back %d times, pushed %d", len(*kicks), len(*pushes))
	}
	if _, code := cascadeFromGate("review", "", false, md, io.Discard, io.Discard); code != 1 {
		t.Fatalf("the cascade's own exit changed: %d, want 1", code)
	}
}

// An operator finish open across a ride that parks settles as failed on
// the walk's real code; only the ride's return becomes the park.
func TestRideParkSettlesFinishOnRealCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during bool
	}{
		{"finish before the ride", false},
		{"finish during the walk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, md := machineParkFixture(t)
			stubSdlcStageWritingCanvas(t, root, md, "design", unreadyDesignCanvas)
			if tc.during {
				inner := openSdlcStage
				openSdlcStage = func(s, p, r string, h bool, o, e io.Writer) int {
					submitControl(t, root, runcontrol.KindFinish, "finish-park")
					return inner(s, p, r, h, o, e)
				}
			} else {
				submitControl(t, root, runcontrol.KindFinish, "finish-park")
			}
			pushes := stubPushFromCascade(t, 0, nil)
			if code := rideRun(root, md.Project, md.ID, rideDynamic, false, io.Discard, io.Discard); code != exitParked {
				t.Fatalf("exit %d, want exitParked", code)
			}
			f, _ := controlRecord(t, root).Find("finish-park")
			if f.State != runcontrol.StateFailed || !strings.Contains(f.Outcome, "exit 1") {
				t.Fatalf("finish receipt = %+v, want failed on exit 1", f)
			}
			if len(*pushes) != 0 {
				t.Fatal("parked finish shipped")
			}
		})
	}
}

// The sweep's view: a picked run whose design turn leaves the gate
// blocked is a clean ride for the exit (and so for the backoff), and
// the log says where it parked instead of "exited 1".
func TestPulseRideParkedAtGateIsClean(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, md := machineParkFixture(t)
	calls := stubSdlcStageWritingCanvas(t, root, md, "design", unreadyDesignCanvas)
	stubPushFromCascade(t, 0, nil)

	var errb bytes.Buffer
	pick := &pulsePick{Key: md.Project + "/" + md.ID, Why: "test"}
	if code := pulseRide(root, pick, nil, io.Discard, &errb); code != 0 {
		t.Fatalf("pulse ride exit %d, want 0; stderr:\n%s", code, errb.String())
	}
	if len(*calls) != 1 || (*calls)[0] != "design" {
		t.Fatalf("dispatches = %v, want one design turn", *calls)
	}
	if want := "pulse: ride moe/change-auth parked at its design gate"; !strings.Contains(errb.String(), want) {
		t.Fatalf("stderr missing %q:\n%s", want, errb.String())
	}
	if strings.Contains(errb.String(), "exited") {
		t.Fatalf("a park reported as an exit:\n%s", errb.String())
	}
}
