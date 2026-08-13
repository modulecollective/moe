package serve

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSnoozeRoundTrips: the file is the whole transport between a
// terminal `moe serve snooze` and a running serve's next tick, so what
// one writes the other has to read back.
func TestSnoozeRoundTrips(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	want := now.Add(2 * time.Hour)

	if err := WriteSnooze(root, want); err != nil {
		t.Fatalf("WriteSnooze: %v", err)
	}
	until, snoozed, err := ReadSnooze(root, now)
	if err != nil {
		t.Fatalf("ReadSnooze: %v", err)
	}
	if !snoozed {
		t.Fatal("a snooze two hours out should be in force")
	}
	if !until.Equal(want.Truncate(time.Second)) {
		t.Errorf("until = %s, want %s", until, want.Truncate(time.Second))
	}
}

// TestSnoozeAbsentReadsAsAwake is the ordinary case — no file, no hold —
// and it must not be an error: an operator who never snoozes would
// otherwise get a warning on every tick.
func TestSnoozeAbsentReadsAsAwake(t *testing.T) {
	_, snoozed, err := ReadSnooze(t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("ReadSnooze on a bare root: %v", err)
	}
	if snoozed {
		t.Error("no file should read as not snoozed")
	}
}

// TestSnoozeExpiresOnItsOwn: nothing sweeps the file up, so an elapsed
// instant has to read as awake by itself. This is the fail-safe the
// whole design leans on — the worst case of forgetting a snooze is that
// sweeps resume.
func TestSnoozeExpiresOnItsOwn(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	if err := WriteSnooze(root, now.Add(-time.Minute)); err != nil {
		t.Fatalf("WriteSnooze: %v", err)
	}
	if _, snoozed, err := ReadSnooze(root, now); err != nil || snoozed {
		t.Errorf("ReadSnooze = snoozed %v, err %v; want an elapsed snooze to read as awake", snoozed, err)
	}
}

// TestSnoozeMalformedWarnsButReadsAwake: a brake that can't be parsed is
// worth a word, but it must not hold the clock — failing open is the
// same direction the rest of the heartbeat takes on a bad read.
func TestSnoozeMalformedWarnsButReadsAwake(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnoozePath(root), []byte("tomorrow-ish\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, snoozed, err := ReadSnooze(root, time.Now())
	if err == nil {
		t.Error("a file that won't parse should come back with an error")
	}
	if snoozed {
		t.Error("a malformed snooze must not hold the clock")
	}
}

// TestClearSnoozeIsIdempotent: `moe serve wake` on a serve that isn't
// snoozed asked for a heartbeat that sweeps, and that's what it has.
func TestClearSnoozeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := ClearSnooze(root); err != nil {
		t.Fatalf("ClearSnooze with no file: %v", err)
	}
	if err := WriteSnooze(root, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ClearSnooze(root); err != nil {
		t.Fatalf("ClearSnooze: %v", err)
	}
	if _, snoozed, _ := ReadSnooze(root, time.Now()); snoozed {
		t.Error("wake should have dropped the hold")
	}
}
