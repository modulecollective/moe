package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestRideModeForAnswer pins the bang-to-mode ladder. The two
// non-riding forms carry no mode at all: there is no unit for the
// machine to grow or refrain from growing.
func TestRideModeForAnswer(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   rideMode
	}{
		{"!", rideNone},
		{"!code", rideNone},
		{"!!", rideNone},
		{"!!!", rideStatic},
		{"!!!!", rideDynamic},
	} {
		if got := rideModeForAnswer(tc.answer); got != tc.want {
			t.Errorf("rideModeForAnswer(%q) = %v, want %v", tc.answer, got, tc.want)
		}
	}
}

// TestWithRideModeRestores: a prompt loop that dispatches two cascades
// in one session must not leak the first answer's consent into the
// second. The restore is what makes process-scoped state safe here.
func TestWithRideModeRestores(t *testing.T) {
	if currentRideMode != rideNone {
		t.Fatalf("mode = %v at rest, want none", currentRideMode)
	}
	func() {
		defer withRideMode(rideDynamic)()
		if currentRideMode != rideDynamic {
			t.Fatalf("mode = %v inside, want dynamic", currentRideMode)
		}
		func() {
			defer withRideMode(rideStatic)()
			if currentRideMode != rideStatic {
				t.Fatalf("nested mode = %v, want static", currentRideMode)
			}
		}()
		if currentRideMode != rideDynamic {
			t.Fatalf("mode = %v after nested restore, want dynamic", currentRideMode)
		}
	}()
	if currentRideMode != rideNone {
		t.Fatalf("mode = %v after restore, want none", currentRideMode)
	}
}

// TestRideModeContextLine: a mid-ride survey is told which kind of ride
// it is inside, so its placement judgment can adapt. Outside a ride the
// line is empty — "nothing is riding" is context the agent can't act
// on.
func TestRideModeContextLine(t *testing.T) {
	if got := rideModeContextLine(); got != "" {
		t.Errorf("context line outside a ride = %q, want empty", got)
	}
	func() {
		defer withRideMode(rideStatic)()
		if got := rideModeContextLine(); !strings.Contains(got, "static") {
			t.Errorf("static context line = %q, want the mode named", got)
		}
	}()
	func() {
		defer withRideMode(rideDynamic)()
		got := rideModeContextLine()
		if !strings.Contains(got, "dynamic") {
			t.Errorf("dynamic context line = %q, want the mode named", got)
		}
		if !strings.Contains(got, "kick") {
			t.Errorf("dynamic context line = %q, want the self-kick license named", got)
		}
	}()
}

// TestPulseKickoffCarriesRideLineWithNothingChained pins the *wiring*,
// not the renderer. The line used to hang off chainStateBlock, which
// renders only for an active chain of two or more members — so it
// reached the agent in neither case it exists for. A tail pulse fires
// after its spawner merged (the ridden unit drops below the bar), and
// the self-kick door is an unchained spawner with no chain at all.
func TestPulseKickoffCarriesRideLineWithNothingChained(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, time.Now().Local(), nil)

	if got := chainStateBlock(root, "moe"); got != "" {
		t.Fatalf("fixture wants no chain block, got %q", got)
	}
	func() {
		defer withRideMode(rideDynamic)()
		got := pulseKickoffWithContext(root, "moe", "pulse-x", io.Discard)
		if !strings.Contains(got, "firing inside a **dynamic** ride") {
			t.Errorf("dynamic ride line missing from the kickoff:\n%s", got)
		}
	}()
	// Outside a ride there is nothing to say, and a "nothing is riding"
	// block would be context the agent can't act on.
	got := pulseKickoffWithContext(root, "moe", "pulse-x", io.Discard)
	if strings.Contains(got, "firing inside") {
		t.Errorf("kickoff names a ride outside one:\n%s", got)
	}
}
