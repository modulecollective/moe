package repolock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentOpts returns an Options suitable for tests: no retries beyond
// the fake clock, discarded logs, deterministic now.
func silentOpts(purpose string) Options {
	return Options{
		Purpose: purpose,
		Budget:  5 * time.Second,
		Logger:  io.Discard,
	}
}

func TestAcquireReleaseRoundtrip(t *testing.T) {
	root := t.TempDir()
	l, err := Acquire(root, silentOpts("test"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The lock file should exist with a parseable record.
	b, err := os.ReadFile(filepath.Join(root, ".moe", "lock"))
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	if rec.Purpose != "test" {
		t.Errorf("Purpose = %q, want %q", rec.Purpose, "test")
	}
	if rec.Owner == "" {
		t.Error("Owner empty")
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".moe", "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still present after Release: err=%v", err)
	}
	// Release again: idempotent.
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestAcquireWritesGitignore(t *testing.T) {
	root := t.TempDir()
	l, err := Acquire(root, silentOpts("test"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	b, err := os.ReadFile(filepath.Join(root, ".moe", ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(b) != "*\n" {
		t.Errorf(".gitignore body = %q, want %q", b, "*\n")
	}
}

func TestContendedAcquireTimesOut(t *testing.T) {
	root := t.TempDir()
	held, err := Acquire(root, silentOpts("holder"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer held.Release()

	opts := silentOpts("waiter")
	opts.Budget = 80 * time.Millisecond
	start := time.Now()
	_, err = Acquire(root, opts)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected TimeoutError, got nil")
	}
	var toErr *TimeoutError
	if !errors.As(err, &toErr) {
		t.Fatalf("error is not *TimeoutError: %v", err)
	}
	if toErr.Holder.Purpose != "holder" {
		t.Errorf("Holder.Purpose = %q, want %q", toErr.Holder.Purpose, "holder")
	}
	// Should have waited roughly the budget, not given up instantly.
	if elapsed < 40*time.Millisecond {
		t.Errorf("timed out too fast (%s); did we actually wait?", elapsed)
	}
}

func TestStaleTakeover(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a stale lock: heartbeat well past threshold, owner is a
	// fake non-pid string so processAlive can't be the stale signal.
	stale := Record{
		Owner:       "other-host/99999999",
		Purpose:     "abandoned",
		AcquiredAt:  time.Now().UTC().Add(-time.Hour),
		HeartbeatAt: time.Now().UTC().Add(-time.Hour),
	}
	body, _ := json.MarshalIndent(stale, "", "  ")
	if err := os.WriteFile(filepath.Join(moeDir, "lock"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(root, silentOpts("takeover"))
	if err != nil {
		t.Fatalf("Acquire over stale: %v", err)
	}
	defer l.Release()
	if l.Record().Purpose != "takeover" {
		t.Errorf("Purpose = %q, want %q", l.Record().Purpose, "takeover")
	}
}

func TestDeadPIDStaleness(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fresh heartbeat but owner is us-as-pid-1 (actually dead pid on this host).
	// Pid 999999 is almost certainly not alive; hostname matches ours so the
	// same-host + dead-pid check fires even though heartbeat is fresh.
	rec := Record{
		Owner:       fmt.Sprintf("%s/%d", hostname(), 999_999),
		Purpose:     "crashed",
		AcquiredAt:  time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(), // fresh heartbeat
	}
	body, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(moeDir, "lock"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isStale(rec, time.Now().UTC()) {
		t.Skip("pid 999999 happens to exist on this host; skipping")
	}
	l, err := Acquire(root, silentOpts("takeover"))
	if err != nil {
		t.Fatalf("Acquire over dead-pid lock: %v", err)
	}
	defer l.Release()
}

func TestOtherHostWithFreshHeartbeatIsNotStale(t *testing.T) {
	// Even if the PID is unknown-dead, a different host means we can't
	// test liveness — treat as live until heartbeat goes stale.
	rec := Record{
		Owner:       "some-other-host/1",
		Purpose:     "alive-elsewhere",
		AcquiredAt:  time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
	}
	if isStale(rec, time.Now().UTC()) {
		t.Error("remote-host record with fresh heartbeat incorrectly flagged stale")
	}
}

func TestConcurrentAcquireExclusivity(t *testing.T) {
	root := t.TempDir()
	const goroutines = 16
	const holdFor = 3 * time.Millisecond

	opts := silentOpts("concurrent")
	opts.Budget = 30 * time.Second
	opts.BackoffCap = 5 * time.Millisecond // short holds — tight poll

	var active int32
	var maxActive int32
	var successes int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			l, err := Acquire(root, opts)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			a := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxActive)
				if a <= m || atomic.CompareAndSwapInt32(&maxActive, m, a) {
					break
				}
			}
			time.Sleep(holdFor)
			atomic.AddInt32(&active, -1)
			atomic.AddInt32(&successes, 1)
			if err := l.Release(); err != nil {
				t.Errorf("Release: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes != goroutines {
		t.Errorf("successes = %d, want %d", successes, goroutines)
	}
	if maxActive > 1 {
		t.Errorf("maxActive = %d, want 1 (lock did not exclude)", maxActive)
	}
}

func TestHeartbeatRewritesHeartbeatAt(t *testing.T) {
	root := t.TempDir()
	opts := silentOpts("long")
	opts.Heartbeat = true
	l, err := Acquire(root, opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	initial := l.Record().HeartbeatAt
	// Force an immediate heartbeat rather than waiting a full tick.
	if ok := l.beat(); !ok {
		t.Fatal("beat returned false")
	}
	updated := l.Record().HeartbeatAt
	if !updated.After(initial) {
		t.Errorf("heartbeat did not advance: initial=%s updated=%s", initial, updated)
	}
	// The on-disk record should reflect the update.
	rec, err := readRecord(filepath.Join(root, ".moe", "lock"))
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	if !rec.HeartbeatAt.Equal(updated) {
		t.Errorf("disk heartbeat %s != in-memory %s", rec.HeartbeatAt, updated)
	}
}

func TestBeatDetectsTakeover(t *testing.T) {
	root := t.TempDir()
	opts := silentOpts("holder")
	opts.Heartbeat = true
	l, err := Acquire(root, opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	// Simulate a third party overwriting the lock file with a different
	// owner — e.g. a stale-takeover race. beat() should refuse to
	// clobber their record and return false.
	other := Record{
		Owner:       "other/1",
		Purpose:     "stolen",
		AcquiredAt:  time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(),
	}
	body, _ := marshalRecord(other)
	if err := os.WriteFile(filepath.Join(root, ".moe", "lock"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok := l.beat(); ok {
		t.Error("beat returned true on a taken-over lock; should stop")
	}
	// Disk record must not have been clobbered.
	rec, err := readRecord(filepath.Join(root, ".moe", "lock"))
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	if rec.Owner != "other/1" {
		t.Errorf("disk owner = %q, want %q", rec.Owner, "other/1")
	}
}

func TestAcquireRespectsBudgetZeroUsesDefault(t *testing.T) {
	root := t.TempDir()
	l, err := Acquire(root, Options{Purpose: "default-budget", Logger: io.Discard})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
}

// fakeClock advances only when Sleep is called, so a test can run the
// settle/backoff machinery without burning real wall time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now().UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestCorruptRecordTakeover(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(moeDir, "lock")
	if err := os.WriteFile(lockPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	clock := newFakeClock()
	opts := silentOpts("takeover-corrupt")
	opts.Now = clock.Now
	opts.Sleep = clock.Sleep
	opts.Budget = 10 * time.Second // generous; takeover should land first

	l, err := Acquire(root, opts)
	if err != nil {
		t.Fatalf("Acquire over corrupt: %v", err)
	}
	defer l.Release()
	if l.Record().Purpose != "takeover-corrupt" {
		t.Errorf("Purpose = %q, want %q", l.Record().Purpose, "takeover-corrupt")
	}
}

func TestCorruptRecordTimeoutBeforeSettle(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moeDir, "lock"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := silentOpts("waiter")
	opts.Budget = 50 * time.Millisecond // far below corruptSettle

	_, err := Acquire(root, opts)
	if err == nil {
		t.Fatal("expected TimeoutError, got nil")
	}
	var toErr *TimeoutError
	if !errors.As(err, &toErr) {
		t.Fatalf("error is not *TimeoutError: %v", err)
	}
	if toErr.ParseErr == nil {
		t.Error("ParseErr is nil; expected non-nil for unparseable record")
	}
	if toErr.Holder != (Record{}) {
		t.Errorf("Holder = %+v, want zero Record (parse failed)", toErr.Holder)
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("error message = %q, want it to mention 'unparseable'", err.Error())
	}
}

func TestPartialWriteDoesNotStealLock(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(moeDir, "lock")
	// Plant the mid-write state: the file exists but has no body yet.
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A "creator" goroutine completes the write inside the settle window.
	finishWrite := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		body, _ := marshalRecord(Record{
			Owner:       "real-holder/1",
			Purpose:     "real-holder",
			AcquiredAt:  time.Now().UTC(),
			HeartbeatAt: time.Now().UTC(),
		})
		_ = os.WriteFile(lockPath, body, 0o644)
		close(finishWrite)
	}()

	opts := silentOpts("waiter")
	opts.Budget = 200 * time.Millisecond // well under corruptSettle

	_, err := Acquire(root, opts)
	<-finishWrite
	if err == nil {
		t.Fatal("expected TimeoutError, got nil (did we steal the lock?)")
	}
	var toErr *TimeoutError
	if !errors.As(err, &toErr) {
		t.Fatalf("error is not *TimeoutError: %v", err)
	}
	if toErr.ParseErr != nil {
		t.Errorf("ParseErr = %v, want nil after creator's write completed", toErr.ParseErr)
	}
	if toErr.Holder.Purpose != "real-holder" {
		t.Errorf("Holder.Purpose = %q, want %q", toErr.Holder.Purpose, "real-holder")
	}
	// Lock file must still hold the creator's record — not removed by us.
	rec, err := readRecord(lockPath)
	if err != nil {
		t.Fatalf("readRecord after timeout: %v", err)
	}
	if rec.Owner != "real-holder/1" {
		t.Errorf("disk owner = %q, want %q (we stole the live lock)", rec.Owner, "real-holder/1")
	}
}
