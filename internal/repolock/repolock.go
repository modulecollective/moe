// Package repolock serializes mutating access to the bureaucracy repo.
//
// A single lock file at <root>/.moe/lock gates every moe code path that
// inspects-and-mutates the working tree. Acquire writes a JSON record
// naming the holder; Release removes the file. A long holder may opt
// into a heartbeat goroutine that rewrites heartbeat_at every few
// seconds so other callers can distinguish "someone is working" from
// "someone crashed mid-op."
//
// Staleness rules (design: short numbers, because no holder should keep
// the lock for more than a few seconds):
//
//   - heartbeat_at older than StaleThreshold → stale, next acquirer
//     takes over.
//   - owner names a pid on the same host that isn't running → stale.
//
// Lock scope is one-per-repo, not per-run. Bureaucracy ops are short
// and serializing them is fine; stage sessions escape the single-writer
// bottleneck by running on their own branch in a separate worktree —
// see internal/session.
package repolock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Default timing knobs. Short numbers on purpose: anything longer than
// a few seconds is a bug we want to surface fast.
const (
	// DefaultBudget caps how long an interactive caller waits for a
	// contended lock before giving up.
	DefaultBudget = 30 * time.Second
	// CronBudget caps a cron-fired caller's wait. Longer than
	// DefaultBudget because no human is watching and deferring a tick
	// is worse than waiting a minute.
	CronBudget = 5 * time.Minute
	// HeartbeatInterval is how often a long-holding lock rewrites
	// heartbeat_at.
	HeartbeatInterval = 5 * time.Second
	// StaleThreshold is the heartbeat age past which a holder is
	// declared stale and may be taken over. Four heartbeat intervals
	// gives a missed heartbeat or two of slack.
	StaleThreshold = 20 * time.Second
	// corruptSettle is how long an unparseable record must persist
	// before Acquire takes it over. Long enough that a reader who
	// catches tryCreate between O_CREATE and Write doesn't steal a
	// healthy live lock; short enough that a genuinely corrupt file
	// resolves well inside DefaultBudget.
	corruptSettle = 1 * time.Second
)

// Record is the on-disk JSON payload of .moe/lock. Exposed so callers
// can render it in error messages ("held by …").
type Record struct {
	Owner       string    `json:"owner"`
	Run         string    `json:"run,omitempty"`
	Purpose     string    `json:"purpose"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// Options configures Acquire. Zero-valued fields get sensible defaults.
type Options struct {
	// Purpose is a short label for the operation taking the lock
	// (run-new, push, stage-open, stage-close, …). Shown in error
	// messages so a contended caller knows who they're waiting on.
	Purpose string
	// Run optionally scopes the lock to a run ("<project>/<run>").
	// Advisory — the lock is repo-wide either way.
	Run string
	// Budget is the maximum wall-clock time Acquire will wait for a
	// contended lock. Defaults to DefaultBudget.
	Budget time.Duration
	// Heartbeat requests a background goroutine that rewrites
	// heartbeat_at every HeartbeatInterval so other acquirers
	// distinguish us from a crashed holder. Only worth setting for
	// holders that keep the lock longer than StaleThreshold.
	Heartbeat bool
	// Now is injected for deterministic tests. Defaults to time.Now.
	Now func() time.Time
	// Sleep is injected for deterministic tests. Defaults to time.Sleep.
	Sleep func(time.Duration)
	// BackoffCap limits the per-retry sleep. Defaults to 2s. Tests
	// with short holds set this lower to avoid stranding waiters.
	BackoffCap time.Duration
	// Logger receives takeover and heartbeat-loss messages. Defaults
	// to os.Stderr. Set io.Discard in tests that shouldn't spew.
	Logger io.Writer
}

// Lock is an acquired lock on <root>/.moe/lock. Release it exactly once.
type Lock struct {
	path   string
	record Record
	opts   Options

	mu       sync.Mutex // guards record during heartbeat rewrites
	stopHB   chan struct{}
	doneHB   chan struct{}
	released bool
}

// TimeoutError is returned by Acquire when the budget is exhausted and
// someone else still holds the lock. Holder is the record read at the
// moment of timeout. ParseErr is non-nil when the timeout fired while
// the lock file was unparseable — Holder is the zero Record in that
// case.
type TimeoutError struct {
	Path     string
	Holder   Record
	ParseErr error
}

func (e *TimeoutError) Error() string {
	if e.ParseErr != nil {
		return fmt.Sprintf("repolock: timed out waiting for %s (unparseable: %v)",
			e.Path, e.ParseErr)
	}
	age := ""
	if !e.Holder.HeartbeatAt.IsZero() {
		age = fmt.Sprintf(", heartbeat %s ago", time.Since(e.Holder.HeartbeatAt).Round(time.Second))
	}
	return fmt.Sprintf("repolock: timed out waiting for %s (held by %s for %q%s)",
		e.Path, e.Holder.Owner, e.Holder.Purpose, age)
}

// Acquire obtains the repo-wide lock at <root>/.moe/lock.
//
// Retries with bounded backoff while someone else holds the lock,
// gives up after opts.Budget with a TimeoutError, and takes over a
// lock whose heartbeat has gone stale (see StaleThreshold). Ensures
// <root>/.moe exists and drops a `*` gitignore inside so the lock
// file never leaks into git history.
func Acquire(root string, opts Options) (*Lock, error) {
	opts = applyDefaults(opts)

	moeDir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(moeDir, 0o755); err != nil {
		return nil, fmt.Errorf("repolock: mkdir %s: %w", moeDir, err)
	}
	if err := ensureGitignore(moeDir); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(moeDir, "lock")
	deadline := opts.Now().Add(opts.Budget)
	backoff := 50 * time.Millisecond
	var firstBadAt time.Time

	for {
		now := opts.Now().UTC()
		rec := Record{
			Owner:       ownerString(),
			Run:         opts.Run,
			Purpose:     opts.Purpose,
			AcquiredAt:  now,
			HeartbeatAt: now,
		}
		if l, err := tryCreate(lockPath, rec, opts); err == nil {
			return l, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("repolock: open %s: %w", lockPath, err)
		}

		// Someone else holds it (or the file is stale/corrupt). Read the
		// record to decide between waiting and taking over.
		existing, readErr := readRecord(lockPath)
		switch {
		case readErr == nil:
			firstBadAt = time.Time{}
			if isStale(existing, now) {
				fmt.Fprintf(opts.Logger,
					"repolock: taking over stale lock at %s (prev owner %q, purpose %q, heartbeat age %s)\n",
					lockPath, existing.Owner, existing.Purpose,
					now.Sub(existing.HeartbeatAt).Round(time.Second))
				if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("repolock: remove stale lock: %w", err)
				}
				continue
			}
		case errors.Is(readErr, os.ErrNotExist):
			// File disappeared between ErrExist and read (holder released
			// in the gap). Retry immediately — the blocker is gone.
			continue
		default:
			// Unparseable record. Treat as stale after a settle window so
			// a genuinely corrupt file doesn't block forever, but tolerate
			// the brief window between tryCreate's O_CREATE and Write.
			if firstBadAt.IsZero() {
				firstBadAt = now
			}
			if now.Sub(firstBadAt) > corruptSettle {
				fmt.Fprintf(opts.Logger,
					"repolock: taking over unparseable lock at %s (parse error: %v)\n",
					lockPath, readErr)
				if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("repolock: remove corrupt lock: %w", err)
				}
				firstBadAt = time.Time{}
				continue
			}
			// Within the settle window: fall through to deadline check.
			// If we time out here, ParseErr surfaces what blocked us.
		}

		// Live holder (or unparseable but still inside settle). Wait or
		// give up.
		if !opts.Now().Before(deadline) {
			return nil, &TimeoutError{Path: lockPath, Holder: existing, ParseErr: readErr}
		}
		sleep := backoff
		if remaining := deadline.Sub(opts.Now()); sleep > remaining {
			sleep = remaining
		}
		if sleep > 0 {
			opts.Sleep(sleep)
		}
		if backoff < opts.BackoffCap {
			backoff *= 2
			if backoff > opts.BackoffCap {
				backoff = opts.BackoffCap
			}
		}
	}
}

// Release removes the lock file and stops the heartbeat goroutine (if
// any). Safe on a nil receiver and idempotent — calling twice is a
// no-op after the first success.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	stopHB := l.stopHB
	doneHB := l.doneHB
	l.mu.Unlock()

	if stopHB != nil {
		close(stopHB)
		<-doneHB
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Record returns a copy of the on-disk record for this lock. Useful
// for tests and logging.
func (l *Lock) Record() Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record
}

func applyDefaults(opts Options) Options {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.BackoffCap <= 0 {
		opts.BackoffCap = 2 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = os.Stderr
	}
	return opts
}

func tryCreate(path string, rec Record, opts Options) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	body, marshalErr := marshalRecord(rec)
	if marshalErr != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, marshalErr
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	l := &Lock{path: path, record: rec, opts: opts}
	if opts.Heartbeat {
		l.startHeartbeat()
	}
	return l, nil
}

func (l *Lock) startHeartbeat() {
	l.stopHB = make(chan struct{})
	l.doneHB = make(chan struct{})
	go l.heartbeatLoop()
}

func (l *Lock) heartbeatLoop() {
	defer close(l.doneHB)
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stopHB:
			return
		case <-t.C:
			if !l.beat() {
				return
			}
		}
	}
}

// beat rewrites heartbeat_at. Before writing it re-reads the on-disk
// record and verifies the owner matches — if another process took over
// our lock (crash recovery, manual tampering), we stop rather than
// clobber their record. Returns false when the caller should exit the
// heartbeat loop.
func (l *Lock) beat() bool {
	existing, err := readRecord(l.path)
	if err != nil {
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat lost (cannot read %s: %v)\n", l.path, err)
		return false
	}
	l.mu.Lock()
	if existing.Owner != l.record.Owner || !existing.AcquiredAt.Equal(l.record.AcquiredAt) {
		l.mu.Unlock()
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat lost (owner changed at %s)\n", l.path)
		return false
	}
	l.record.HeartbeatAt = l.opts.Now().UTC()
	rec := l.record
	l.mu.Unlock()

	body, marshalErr := marshalRecord(rec)
	if marshalErr != nil {
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat marshal: %v\n", marshalErr)
		return true // keep trying; a transient failure shouldn't drop the lock
	}
	// Rename-over is atomic within the same filesystem and keeps
	// concurrent readers from seeing a half-written file.
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat write: %v\n", err)
		return true
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat rename: %v\n", err)
		return true
	}
	return true
}

func marshalRecord(rec Record) ([]byte, error) {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("repolock: marshal: %w", err)
	}
	return append(b, '\n'), nil
}

func readRecord(path string) (Record, error) {
	var rec Record
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("repolock: parse %s: %w", path, err)
	}
	return rec, nil
}

// isStale returns true when rec was abandoned and another caller may
// safely take over. Two cheap signals: heartbeat age past threshold,
// and — if owner names a pid on this host — the pid no longer existing.
func isStale(rec Record, now time.Time) bool {
	if rec.HeartbeatAt.IsZero() {
		// No heartbeat recorded at all — treat as stale only if the
		// acquisition time is also old, so a racing partial-write
		// doesn't get clobbered the instant it appears.
		return !rec.AcquiredAt.IsZero() && now.Sub(rec.AcquiredAt) > StaleThreshold
	}
	if now.Sub(rec.HeartbeatAt) > StaleThreshold {
		return true
	}
	host, pid, ok := parseOwner(rec.Owner)
	if !ok {
		return false
	}
	if host != hostname() {
		return false
	}
	return !processAlive(pid)
}

// parseOwner splits an "<host>/<pid>" owner string. Returns ok=false
// for malformed or non-pid owners (tests may use sentinel names).
func parseOwner(owner string) (host string, pid int, ok bool) {
	i := strings.LastIndex(owner, "/")
	if i < 0 || i == len(owner)-1 {
		return "", 0, false
	}
	p, err := strconv.Atoi(owner[i+1:])
	if err != nil || p <= 0 {
		return "", 0, false
	}
	return owner[:i], p, true
}

// processAlive tests whether pid exists on this host. Signal 0 is the
// portable "is this pid a thing?" probe; no signal is delivered.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ownerString is what we write as Owner in a fresh record: <hostname>/<pid>.
func ownerString() string {
	return fmt.Sprintf("%s/%d", hostname(), os.Getpid())
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// ensureGitignore drops `*` into <dir>/.gitignore so `.moe/`'s contents
// never appear in git status. Lazy-write: existing file is left alone.
func ensureGitignore(dir string) error {
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("repolock: stat %s: %w", p, err)
	}
	if err := os.WriteFile(p, []byte("*\n"), 0o644); err != nil {
		return fmt.Errorf("repolock: write %s: %w", p, err)
	}
	return nil
}
