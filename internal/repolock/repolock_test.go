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
	if l.record().Purpose != "takeover" {
		t.Errorf("Purpose = %q, want %q", l.record().Purpose, "takeover")
	}
}

func TestDeadPIDStaleness(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fresh heartbeat but owner is us-as-pid-1 (actually dead pid on this host).
	// Pid 999999 is almost certainly not alive; host matches ours so the
	// same-host + dead-pid check fires even though heartbeat is fresh.
	localHost := hostHandle(moeDir)
	rec := Record{
		Owner:       fmt.Sprintf("%s/%d", localHost, 999_999),
		Purpose:     "crashed",
		AcquiredAt:  time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(), // fresh heartbeat
	}
	body, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(moeDir, "lock"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isStale(rec, time.Now().UTC(), localHost) {
		t.Skip("pid 999999 happens to exist on this host; skipping")
	}
	l, err := Acquire(root, silentOpts("takeover"))
	if err != nil {
		t.Fatalf("Acquire over dead-pid lock: %v", err)
	}
	defer l.Release()
}

// TestProcessAliveEPERMTreatedAsAlive guards the EPERM-as-alive
// branch. Pid 1 (launchd on macOS, init/systemd on linux) always
// exists; signalling it from a non-root uid yields EPERM, which
// processAlive must read as "alive." Skipped when the test runs as
// root because Signal(0) on pid 1 returns nil there and exercises
// the existing happy path instead.
func TestProcessAliveEPERMTreatedAsAlive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — Signal(0) on pid 1 returns nil, not EPERM")
	}
	if !processAlive(1) {
		t.Errorf("processAlive(1) = false; want true (pid 1 exists, Signal(0) returns EPERM for non-root)")
	}
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
	if isStale(rec, time.Now().UTC(), "this-host") {
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

	initial := l.record().HeartbeatAt
	// Force an immediate heartbeat rather than waiting a full tick.
	if ok := l.beat(); !ok {
		t.Fatal("beat returned false")
	}
	updated := l.record().HeartbeatAt
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

	l, err := Acquire(root, silentOpts("takeover-corrupt"))
	if err != nil {
		t.Fatalf("Acquire over corrupt: %v", err)
	}
	defer l.Release()
	if l.record().Purpose != "takeover-corrupt" {
		t.Errorf("Purpose = %q, want %q", l.record().Purpose, "takeover-corrupt")
	}
}

// TestTryCreateNoEmptyFileVisible churns Acquire/Release in parallel
// while a reader scrutinises the lock path. Every read must yield
// either ErrNotExist or a fully-parseable Record — never an empty or
// truncated body. Regression guard for the atomic-create invariant.
func TestTryCreateNoEmptyFileVisible(t *testing.T) {
	root := t.TempDir()
	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(moeDir, "lock")

	opts := silentOpts("race")
	opts.Budget = 30 * time.Second
	opts.BackoffCap = 5 * time.Millisecond

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l, err := Acquire(root, opts)
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if err := l.Release(); err != nil {
					t.Errorf("Release: %v", err)
					return
				}
			}
		}()
	}

	var reads int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(lockPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Errorf("ReadFile: %v", err)
				return
			}
			atomic.AddInt64(&reads, 1)
			var rec Record
			if uerr := json.Unmarshal(b, &rec); uerr != nil {
				t.Errorf("unparseable lock file (%d bytes): %v\nbody=%q", len(b), uerr, b)
				return
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	if reads == 0 {
		t.Skip("reader never observed the lock file in the test window")
	}
}

// TestInstanceIDCreatesFileWhenMissing exercises the first-call path:
// no instance-id on disk, instanceID generates and persists a fresh
// random hex value.
func TestInstanceIDCreatesFileWhenMissing(t *testing.T) {
	moeDir := t.TempDir()
	id, err := instanceID(moeDir)
	if err != nil {
		t.Fatalf("instanceID: %v", err)
	}
	if id == "" {
		t.Fatal("instanceID returned empty string")
	}
	if len(id) != 32 {
		t.Errorf("id length = %d, want 32 hex chars", len(id))
	}
	b, err := os.ReadFile(filepath.Join(moeDir, "instance-id"))
	if err != nil {
		t.Fatalf("read instance-id: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != id {
		t.Errorf("on-disk id = %q, want %q", got, id)
	}
}

// TestInstanceIDReusesExistingFile exercises the read-existing path:
// a pre-written id is returned verbatim and the file is not rewritten.
func TestInstanceIDReusesExistingFile(t *testing.T) {
	moeDir := t.TempDir()
	want := "deadbeefdeadbeefdeadbeefdeadbeef"
	if err := os.WriteFile(filepath.Join(moeDir, "instance-id"), []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := instanceID(moeDir)
	if err != nil {
		t.Fatalf("instanceID: %v", err)
	}
	if got != want {
		t.Errorf("id = %q, want %q", got, want)
	}
}

// TestInstanceIDConcurrentCreate races N goroutines on a fresh dir.
// All must observe the same id and exactly one instance-id file must
// remain on disk (no leaked tmp files).
func TestInstanceIDConcurrentCreate(t *testing.T) {
	moeDir := t.TempDir()
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = instanceID(moeDir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("ids diverged: %q vs %q", ids[0], ids[i])
		}
	}
	entries, err := os.ReadDir(moeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "instance-id" {
			t.Errorf("leftover entry %q (want only instance-id)", e.Name())
		}
	}
}

// TestAcquireWithFailingHostnameUsesInstanceID forces the hostname
// failure path and asserts the recorded Owner is keyed off the cached
// instance-id rather than the literal "unknown" string.
func TestAcquireWithFailingHostnameUsesInstanceID(t *testing.T) {
	prev := hostnameFunc
	hostnameFunc = func() (string, error) { return "", errors.New("hostname unavailable") }
	t.Cleanup(func() { hostnameFunc = prev })

	root := t.TempDir()
	l, err := Acquire(root, silentOpts("hostless"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	id, err := readInstanceID(filepath.Join(root, ".moe", "instance-id"))
	if err != nil {
		t.Fatalf("readInstanceID: %v", err)
	}
	wantPrefix := id + "/"
	if !strings.HasPrefix(l.record().Owner, wantPrefix) {
		t.Errorf("Owner = %q, want prefix %q", l.record().Owner, wantPrefix)
	}
	if strings.HasPrefix(l.record().Owner, "unknown/") {
		t.Error("Owner still uses literal 'unknown' fallback")
	}
}

// TestTryCreateCleansUpTmpOnLoss verifies that a losing acquirer
// removes its tmp file. After a contended Acquire that times out, the
// only entries in <root>/.moe should be `lock` (held by the winner)
// and `.gitignore`.
func TestTryCreateCleansUpTmpOnLoss(t *testing.T) {
	root := t.TempDir()
	held, err := Acquire(root, silentOpts("holder"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer held.Release()

	opts := silentOpts("waiter")
	opts.Budget = 80 * time.Millisecond
	opts.BackoffCap = 5 * time.Millisecond
	if _, err := Acquire(root, opts); err == nil {
		t.Fatal("expected TimeoutError, got nil")
	}

	entries, err := os.ReadDir(filepath.Join(root, ".moe"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "lock" && name != ".gitignore" {
			t.Errorf(".moe contains unexpected entry %q (want only lock + .gitignore)", name)
		}
	}
}

// TestWithRunsFnAndReleases covers the happy path of With: fn runs while
// the lock is held, and the lock file is gone once With returns.
func TestWithRunsFnAndReleases(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, ".moe", "lock")
	ran := false
	err := With(root, silentOpts("with"), func() error {
		ran = true
		if _, err := os.Stat(lockPath); err != nil {
			t.Errorf("lock file absent while fn runs: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	if !ran {
		t.Error("fn did not run")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still present after With: err=%v", err)
	}
}

// TestWithPropagatesFnError verifies fn's error is returned verbatim and
// the lock is still released on the error path.
func TestWithPropagatesFnError(t *testing.T) {
	root := t.TempDir()
	want := errors.New("boom")
	got := With(root, silentOpts("with"), func() error { return want })
	if !errors.Is(got, want) {
		t.Errorf("With error = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".moe", "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still present after With error: err=%v", err)
	}
}

// TestWithShortCircuitsOnAcquireError verifies that when Acquire fails
// (here: a live holder plus an exhausted budget), With returns the
// acquire error and never calls fn.
func TestWithShortCircuitsOnAcquireError(t *testing.T) {
	root := t.TempDir()
	held, err := Acquire(root, silentOpts("holder"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer held.Release()

	opts := silentOpts("with")
	opts.Budget = 40 * time.Millisecond
	opts.BackoffCap = 5 * time.Millisecond
	ran := false
	err = With(root, opts, func() error {
		ran = true
		return nil
	})
	if ran {
		t.Error("fn ran despite acquire failure")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("With error = %v, want *TimeoutError", err)
	}
}
