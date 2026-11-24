package cli

import (
	"strings"
	"testing"
)

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
			defer withRideMode(rideNone)()
			if currentRideMode != rideNone {
				t.Fatalf("nested mode = %v, want none", currentRideMode)
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

// TestRideModeContextLine: a dynamic sweep is told its ordering opinion
// is about to become motion, so its placement judgment can adapt.
// Everywhere else the line is empty — "nothing will start" is context
// the agent can't act on.
func TestRideModeContextLine(t *testing.T) {
	if got := rideModeContextLine(); got != "" {
		t.Errorf("context line outside a sweep = %q, want empty", got)
	}
	func() {
		defer withRideMode(rideNone)()
		if got := rideModeContextLine(); got != "" {
			t.Errorf("bang-walk context line = %q, want empty — a bang sweeps nothing", got)
		}
	}()
	func() {
		defer withRideMode(rideDynamic)()
		got := rideModeContextLine()
		if !strings.Contains(got, "dynamic") {
			t.Errorf("dynamic context line = %q, want the mode named", got)
		}
		if !strings.Contains(got, "`next`") {
			t.Errorf("dynamic context line = %q, want the one pick named", got)
		}
	}()
}

// TestConsentTrailerValueTracksTheWalk: the flag the push stamp gates
// on. currentRideMode alone can't distinguish a `!` cascade (a machine
// walk consenting to nothing) from an operator's bare `moe push` —
// both read rideNone — so the active bit is what keeps the MoE-Consent
// trailer's "a machine shipped this" claim exact.
func TestConsentTrailerValueTracksTheWalk(t *testing.T) {
	value, active := consentTrailerValue()
	if active {
		t.Fatalf("a walk reads active at rest (value=%q)", value)
	}
	func() {
		defer withRideMode(rideNone)()
		value, active := consentTrailerValue()
		if !active || value != "none" {
			t.Errorf("inside a `!` walk: (%q, %v), want (\"none\", true)", value, active)
		}
	}()
	func() {
		defer withRideMode(rideDynamic)()
		value, active := consentTrailerValue()
		if !active || value != "dynamic" {
			t.Errorf("inside a dynamic sweep: (%q, %v), want (\"dynamic\", true)", value, active)
		}
	}()
	if _, active := consentTrailerValue(); active {
		t.Error("the active bit leaked past the restore")
	}
}

// TestSpawnConsentPairsWithTheSpawnEdge: a mint with no spawner is the
// operator's own `moe pulse` — no machine opened it, so nothing claims
// one did. The trailer decorates the spawn edge; it doesn't stand alone.
func TestSpawnConsentPairsWithTheSpawnEdge(t *testing.T) {
	if got := spawnConsent(""); got != "" {
		t.Errorf("spawnConsent(\"\") = %q, want empty", got)
	}
	if got := spawnConsent("moe/pulse-1"); got != "none" {
		t.Errorf("spawnConsent with a spawner = %q, want \"none\"", got)
	}
	func() {
		defer withRideMode(rideDynamic)()
		if got := spawnConsent("moe/pulse-1"); got != "dynamic" {
			t.Errorf("spawnConsent inside a dynamic ride = %q, want \"dynamic\"", got)
		}
	}()
}
