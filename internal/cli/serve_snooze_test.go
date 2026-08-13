package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/serve"
)

// snoozeRoot stands up a bureaucracy and aims MOE_HOME at it, which is
// all the two verbs need: they write one file under `.moe/` and read
// nothing else.
func snoozeRoot(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	return root
}

// runVerb drives `moe serve <args...>` through the registered command,
// so the args-peek dispatch is part of what's under test rather than
// something the test routes around.
func runVerb(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(append([]string{"serve"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// TestServeSnoozeWritesTheHold is the CLI half of the transport: the
// verb writes the same file the running serve's tick reads, which is why
// it works with serve up, down, or not yet started.
func TestServeSnoozeWritesTheHold(t *testing.T) {
	root := snoozeRoot(t)

	code, stdout, stderr := runVerb(t, "snooze", "2h")
	if code != 0 {
		t.Fatalf("moe serve snooze 2h = %d, stderr=%s", code, stderr)
	}
	until, snoozed, err := serve.ReadSnooze(root, time.Now())
	if err != nil || !snoozed {
		t.Fatalf("ReadSnooze = %v, %v; want a hold on disk", snoozed, err)
	}
	if got := time.Until(until); got < 115*time.Minute || got > 2*time.Hour {
		t.Errorf("snooze lands in %s, want about two hours", got)
	}
	// The resume instant is the whole answer, so it has to be printed.
	if !strings.Contains(stdout, serve.SnoozeClock(until)) {
		t.Errorf("stdout = %q, want the resume time named", stdout)
	}
}

// TestServeSnoozeReportsWithNoArgument: `moe serve snooze` is the
// question as well as the command, and it answers with no serve running
// to ask — which is the case an operator hits after a reboot.
func TestServeSnoozeReportsWithNoArgument(t *testing.T) {
	root := snoozeRoot(t)

	code, stdout, _ := runVerb(t, "snooze")
	if code != 0 || !strings.Contains(stdout, "not snoozed") {
		t.Fatalf("bare snooze = %d, stdout=%q; want the awake state reported", code, stdout)
	}

	until := time.Now().Add(90 * time.Minute)
	if err := serve.WriteSnooze(root, until); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runVerb(t, "snooze")
	if code != 0 || !strings.Contains(stdout, serve.SnoozeClock(until)) {
		t.Errorf("bare snooze = %d, stdout=%q; want the hold reported", code, stdout)
	}
}

// TestServeWakeReleasesTheHold, and does so whether or not there was one
// — the operator asked for a heartbeat that sweeps either way.
func TestServeWakeReleasesTheHold(t *testing.T) {
	root := snoozeRoot(t)
	if err := serve.WriteSnooze(root, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runVerb(t, "wake"); code != 0 {
		t.Fatalf("moe serve wake = %d, stderr=%s", code, stderr)
	}
	if _, snoozed, _ := serve.ReadSnooze(root, time.Now()); snoozed {
		t.Error("wake should have dropped the hold")
	}
	if code, _, stderr := runVerb(t, "wake"); code != 0 {
		t.Errorf("a second wake = %d, stderr=%s; want it idempotent", code, stderr)
	}
}

// TestServeSnoozeRejectsNonsense: zero and negative are a typo whose two
// charitable readings point in opposite directions, so neither gets
// guessed at.
func TestServeSnoozeRejectsNonsense(t *testing.T) {
	root := snoozeRoot(t)
	for _, arg := range []string{"soon", "0", "-1h"} {
		if code, _, _ := runVerb(t, "snooze", arg); code != 2 {
			t.Errorf("moe serve snooze %s = %d, want a usage error", arg, code)
		}
	}
	if _, snoozed, _ := serve.ReadSnooze(root, time.Now()); snoozed {
		t.Error("a rejected duration must not have written a hold")
	}
}

// TestServeStillParsesItsFlags is what the args-peek buys and what it
// risks. Two bare words dispatch to the verbs; everything else — a flag
// above all — still reaches the flag set, which is why `moe serve` was
// not converted to a CommandGroup.
func TestServeStillParsesItsFlags(t *testing.T) {
	snoozeRoot(t)
	// A bad flag value fails in the parser (exit 2) rather than being
	// mistaken for a verb; a valid parse would go on to bind a listener,
	// which is not this test's business.
	if code, _, stderr := runVerb(t, "--port=not-a-number"); code != 2 {
		t.Errorf("moe serve --port=not-a-number = %d, stderr=%s; want the flag parser to own it", code, stderr)
	}
	if code, _, stderr := runVerb(t, "sleep"); code != 2 || !strings.Contains(stderr, "usage:") {
		t.Errorf("moe serve sleep = %d, stderr=%s; want the usage, not a verb", code, stderr)
	}
}
