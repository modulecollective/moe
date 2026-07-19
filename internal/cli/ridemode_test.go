package cli

import (
	"strings"
	"testing"
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
