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
	"crypto/rand"
	"encoding/hex"
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

// hostnameFunc is os.Hostname indirected so tests can drive the
// hostname-failure path without the kernel's cooperation.
var hostnameFunc = os.Hostname

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
	path string
	rec  Record
	opts Options

	mu       sync.Mutex // guards rec during heartbeat rewrites
	stopHB   chan struct{}
	doneHB   chan struct{}
	released bool
}

// TimeoutError is returned by Acquire when the budget is exhausted and
// someone else still holds the lock. Holder is the record read at the
// moment of timeout.
type TimeoutError struct {
	Path   string
	Holder Record
}

func (e *TimeoutError) Error() string {
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
	localHost := hostHandle(moeDir)
	deadline := opts.Now().Add(opts.Budget)
	backoff := 50 * time.Millisecond

	for {
		now := opts.Now().UTC()
		rec := Record{
			Owner:       ownerString(localHost),
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
			if isStale(existing, now, localHost) {
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
			// Unparseable record. tryCreate writes atomically (tmp+link),
			// so a partial mid-write file is impossible — anything we
			// can't parse is real garbage and the right move is to take
			// over immediately.
			fmt.Fprintf(opts.Logger,
				"repolock: taking over unparseable lock at %s (parse error: %v)\n",
				lockPath, readErr)
			if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("repolock: remove corrupt lock: %w", err)
			}
			continue
		}

		// Live holder. Wait or give up.
		if !opts.Now().Before(deadline) {
			return nil, &TimeoutError{Path: lockPath, Holder: existing}
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

// record returns a copy of the on-disk record for this lock. Useful
// for tests and logging.
func (l *Lock) record() Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rec
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

// tryCreate materialises the lock file atomically: write a fully-formed
// temp file in the same directory, then link it to the lock path.
// link(2) refuses with EEXIST if the target exists, giving us the same
// "someone else got there first" signal as O_CREATE|O_EXCL but without
// the create-then-write window in which a peer could observe an empty
// lock file. Returns os.ErrExist on contention so Acquire's existing
// errors.Is branch keeps working.
func tryCreate(path string, rec Record, opts Options) (*Lock, error) {
	body, err := marshalRecord(rec)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "lock.tmp.*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // tmp pathname is disposable after link succeeds or fails
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, os.ErrExist
		}
		return nil, err
	}
	l := &Lock{path: path, rec: rec, opts: opts}
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
	if existing.Owner != l.rec.Owner || !existing.AcquiredAt.Equal(l.rec.AcquiredAt) {
		l.mu.Unlock()
		fmt.Fprintf(l.opts.Logger, "repolock: heartbeat lost (owner changed at %s)\n", l.path)
		return false
	}
	l.rec.HeartbeatAt = l.opts.Now().UTC()
	rec := l.rec
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
// localHost is the caller's host handle (see hostHandle); passing it in
// avoids a re-lookup per iteration and keeps the comparison stable
// across the Acquire loop.
func isStale(rec Record, now time.Time, localHost string) bool {
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
	if host != localHost {
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
// portable "is this pid a thing?" probe; no signal is delivered. Only
// ESRCH proves the pid is gone: EPERM (different uid) means the
// process exists but we can't signal it, and any other errno is
// ambiguous. Treating ambiguity as alive costs at worst a wait/timeout
// for a real dead pid; treating it as dead would steal a live lock.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// ownerString is what we write as Owner in a fresh record: <host>/<pid>.
// host is the caller's hostHandle output, supplied by Acquire so the
// owner string and the isStale comparison stay in agreement.
func ownerString(host string) string {
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// hostHandle returns a stable identifier for the host running this
// process. Prefers os.Hostname; on failure falls back to a per-checkout
// random ID cached at <moeDir>/instance-id so two boxes with broken
// hostname lookups don't both serialise to "unknown" and confuse the
// same-host pid-alive shortcut in isStale. Final "unknown" is only
// returned when the cache write also fails (e.g. read-only .moe/),
// preserving the prior semantics for that genuinely-unidentifiable case.
func hostHandle(moeDir string) string {
	h, err := hostnameFunc()
	if err == nil && h != "" {
		return h
	}
	if id, err := instanceID(moeDir); err == nil && id != "" {
		return id
	}
	return "unknown"
}

// instanceID returns a per-checkout random identifier persisted at
// <moeDir>/instance-id. First call generates 16 bytes from crypto/rand
// and atomically links the file into place; subsequent calls re-read
// it. Concurrent first callers race via os.Link — losers re-read the
// winner's file. Mirrors the tmp+link pattern in tryCreate so a
// half-written file is never visible to a peer.
func instanceID(moeDir string) (string, error) {
	p := filepath.Join(moeDir, "instance-id")
	if id, err := readInstanceID(p); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("repolock: instance-id rand: %w", err)
	}
	id := hex.EncodeToString(buf[:])
	tmp, err := os.CreateTemp(moeDir, "instance-id.tmp.*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write([]byte(id + "\n")); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmpPath, p); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the race; another caller created it first.
			return readInstanceID(p)
		}
		return "", err
	}
	return id, nil
}

func readInstanceID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("repolock: empty instance-id at %s", path)
	}
	return id, nil
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
