// Package runsock is a run's live endpoint: a local Unix socket the run's
// current guard holder serves, so the CLI and serve can read what the
// owner is doing and hand it control requests.
//
// One endpoint per execution-owned run, addressed by the canonical
// bureaucracy root, project and run. The socket path is a short hash of
// that identity in a private 0700 runtime directory (Unix socket paths
// are capped near 108 bytes); every request carries the full identity
// and the protocol version, and the server refuses a mismatch, so a hash
// collision or an old binary cannot cross-talk.
//
// Only the guard holder binds (internal/runowner is the authority):
// while it holds the guard no successor can exist, so it may unlink a
// stale socket left by a dead predecessor, and it closes and joins its
// handlers before releasing the guard, so cleanup never removes a
// successor's listener. A missing listener means no live owner — not
// that the run finished, and never permission to start a replacement.
//
// The wire is one bounded newline-terminated JSON request and one JSON
// reply per connection. Status is a full snapshot; polling it is the
// whole observation protocol. Durable state (control.json, trailers) is
// what correctness rests on; this is the live overlay.
package runsock

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/runcontrol"
)

// Version is the wire protocol version. A request with another version
// is refused rather than half-understood.
const Version = 1

// maxFrame bounds one request or reply. Status snapshots are small; a
// peer sending more is not a moe endpoint.
const maxFrame = 64 << 10

// handlerTimeout bounds how long a handler waits on a slow peer.
var handlerTimeout = 10 * time.Second

// Identity is the run an endpoint serves.
type Identity struct {
	Root    string `json:"root"`
	Project string `json:"project"`
	Run     string `json:"run"`
}

func (id Identity) String() string { return id.Project + "/" + id.Run }

// Phases an owner reports. Each is published by the code making that
// decision, never inferred from log text.
const (
	PhaseAdmission = "admission"  // taking the run, before quota
	PhaseQuotaWait = "quota wait" // waiting for agent quota
	PhaseRunning   = "agent running"
	PhaseSettling  = "settling" // committing a finished turn
	PhaseDeciding  = "awaiting operator"
	PhaseBetween   = "between stages"
	PhaseShipping  = "shipping"
)

// Origin is where an owner came from, as the launcher said explicitly.
// A manual launch has no sweep and no serve launch: nothing is invented.
type Origin struct {
	// Sweep is the parent sweep's pulse run, "<project>/<run>".
	Sweep string `json:"sweep,omitempty"`
	// Host is how the process was hosted: "terminal", "local-pty", "rex".
	Host string `json:"host,omitempty"`
	// ServeLaunch identifies the serve process and child that launched
	// it. It survives a serve restart that adopts the job; the server now
	// observing it is a separate fact, labelled by the reader.
	ServeLaunch string `json:"serve_launch,omitempty"`
}

// Terminal names the Rex block hosting an owner: full IDs supplied by
// the owner's own environment, never resolved from a label.
type Terminal struct {
	Session string `json:"session"`
	Block   string `json:"block"`
}

// Status is one snapshot of an owner's observed state.
type Status struct {
	Identity Identity `json:"identity"`
	// OwnerPID and Generation identify this owner. Generation is fresh
	// per endpoint, so a reader can tell a replacement owner from the
	// same one answering again.
	OwnerPID   int    `json:"owner_pid"`
	Generation string `json:"generation"`
	// Revision increases with every change the owner publishes.
	Revision uint64 `json:"revision"`
	Workflow string `json:"workflow,omitempty"`
	// Stage is the stage actually running or last run, not the next.
	Stage string `json:"stage,omitempty"`
	// Interactive is true for an attended owner (a person at its
	// terminal), false for a headless walk.
	Interactive bool `json:"interactive"`
	// Continuation is the owner's effective continuation authority:
	// "prompt", "!", "!!", "!!!". A pending finish request is separate.
	Continuation string    `json:"continuation,omitempty"`
	Phase        string    `json:"phase"`
	PhaseSince   time.Time `json:"phase_since"`
	// Waiting is why the owner is waiting, from the code that decided.
	Waiting string `json:"waiting,omitempty"`
	// Activity is the last observed agent output, when known; zero is
	// unknown. A fresh reply proves contact, not progress.
	Activity  time.Time `json:"activity,omitzero"`
	Origin    Origin    `json:"origin"`
	Terminal  *Terminal `json:"terminal,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type frame struct {
	Version    int                 `json:"version"`
	Op         string              `json:"op"`
	Identity   Identity            `json:"identity"`
	Generation string              `json:"generation,omitempty"`
	Control    *runcontrol.Request `json:"control,omitempty"`
}

type reply struct {
	Version int                 `json:"version"`
	OK      bool                `json:"ok"`
	Error   string              `json:"error,omitempty"`
	Status  *Status             `json:"status,omitempty"`
	Request *runcontrol.Request `json:"request,omitempty"`
}

// RuntimeDir is the private directory sockets live in: under
// $XDG_RUNTIME_DIR when set, else the temp dir, per user. Never inside
// the bureaucracy or a session worktree.
func RuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "moe", "runs")
	}
	return filepath.Join(os.TempDir(), "moe-"+strconv.Itoa(os.Getuid()), "runs")
}

// Path is the socket for id under dir.
func Path(dir string, id Identity) string {
	sum := sha256.Sum256([]byte(id.Root + "\x00" + id.Project + "/" + id.Run))
	return filepath.Join(dir, hex.EncodeToString(sum[:10])+".sock")
}

// Server is one owner's endpoint. Publish status through Update; set
// Submit to persist control requests.
type Server struct {
	id   Identity
	path string
	ln   net.Listener

	// Submit persists a control request and returns it as recorded. It
	// runs under the dispatch lock, so it is serialised with the owner's
	// checkpoints: a request lands either before a checkpoint reads the
	// record, or after the transition it admitted is published.
	Submit func(runcontrol.Request) (runcontrol.Request, error)

	dispatch sync.Mutex

	mu     sync.Mutex
	status Status
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen binds id's endpoint under dir. The caller must hold the run's
// guard: that is what makes removing a leftover socket safe.
func Listen(dir string, id Identity, initial Status) (*Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runsock: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runsock: %w", err)
	}
	p := Path(dir, id)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("runsock: clear stale %s: %w", p, err)
	}
	ln, err := listenUnix(p)
	if err != nil {
		return nil, fmt.Errorf("runsock: listen %s: %w", p, err)
	}
	var gen [6]byte
	if _, err := rand.Read(gen[:]); err != nil {
		ln.Close()
		return nil, err
	}
	now := time.Now().UTC()
	st := initial
	st.Identity, st.OwnerPID, st.Generation = id, os.Getpid(), hex.EncodeToString(gen[:])
	st.Revision, st.UpdatedAt = 1, now
	if st.Phase == "" {
		st.Phase = PhaseAdmission
	}
	if st.PhaseSince.IsZero() {
		st.PhaseSince = now
	}
	s := &Server{id: id, path: p, ln: ln, conns: map[net.Conn]struct{}{}, status: st}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is the socket path.
func (s *Server) Addr() string { return s.path }

// Update applies change to the published status and bumps its revision.
// A phase change restamps PhaseSince.
func (s *Server) Update(change func(*Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	phase := s.status.Phase
	change(&s.status)
	now := time.Now().UTC()
	if s.status.Phase != phase {
		s.status.PhaseSince = now
	}
	s.status.Revision++
	s.status.UpdatedAt = now
}

// Checkpoint runs fn under the dispatch lock. fn reads the control
// record and returns whether the owner may proceed; if so, admitted is
// applied to the status before the lock is released, so the transition
// is published before a later request can be acknowledged.
func (s *Server) Checkpoint(fn func() bool, admitted func(*Status)) bool {
	s.dispatch.Lock()
	defer s.dispatch.Unlock()
	ok := fn()
	if ok && admitted != nil {
		s.Update(admitted)
	}
	return ok
}

// Snapshot returns the current status.
func (s *Server) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Close stops serving: the listener goes (and with it the socket file),
// open connections are cut, and Close waits for every handler to return.
// Call it before releasing the run guard.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	c.SetDeadline(time.Now().Add(handlerTimeout))
	answer := func(r reply) {
		r.Version = Version
		json.NewEncoder(c).Encode(r)
	}
	line, err := readFrame(c)
	if err != nil {
		answer(reply{Error: err.Error()})
		return
	}
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		answer(reply{Error: "malformed request: " + err.Error()})
		return
	}
	if f.Version != Version {
		answer(reply{Error: fmt.Sprintf("protocol version %d, endpoint speaks %d", f.Version, Version)})
		return
	}
	if f.Identity != s.id {
		answer(reply{Error: fmt.Sprintf("endpoint serves %s in %s, not %s in %s",
			s.id, s.id.Root, f.Identity, f.Identity.Root)})
		return
	}
	switch f.Op {
	case "status":
		snap := s.Snapshot()
		answer(reply{OK: true, Status: &snap})
	case "control":
		if f.Control == nil || s.Submit == nil {
			answer(reply{Error: "no control request"})
			return
		}
		if gen := s.Snapshot().Generation; f.Generation != "" && f.Generation != gen {
			answer(reply{Error: fmt.Sprintf("addressed to owner %s; %s is owned by %s now", f.Generation, s.id, gen)})
			return
		}
		// The submit may outlive a slow peer's deadline (it waits for the
		// repository lock); the request is durable either way, and the
		// client reads it back by id.
		c.SetDeadline(time.Time{})
		s.dispatch.Lock()
		r, err := s.Submit(*f.Control)
		s.dispatch.Unlock()
		c.SetDeadline(time.Now().Add(handlerTimeout))
		if err != nil {
			answer(reply{Error: err.Error()})
			return
		}
		snap := s.Snapshot()
		answer(reply{OK: true, Request: &r, Status: &snap})
	default:
		answer(reply{Error: "unknown op " + strconv.Quote(f.Op)})
	}
}

// readFrame reads one newline-terminated frame of at most maxFrame bytes.
func readFrame(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, maxFrame+1), 4096)
	line, err := br.ReadBytes('\n')
	if len(line) > maxFrame {
		return nil, fmt.Errorf("frame exceeds %d bytes", maxFrame)
	}
	if err != nil {
		return nil, fmt.Errorf("read frame: %w", err)
	}
	return line, nil
}

// ErrNoOwner reports that nothing is listening for the run: no live
// owner right now. It says nothing about whether the run finished.
var ErrNoOwner = errors.New("runsock: no live owner")

func dial(ctx context.Context, dir string, id Identity) (net.Conn, error) {
	c, err := dialUnix(ctx, Path(dir, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w for %s", ErrNoOwner, id)
		}
		return nil, fmt.Errorf("runsock: dial %s: %w", id, err)
	}
	return c, nil
}

func roundTrip(ctx context.Context, dir string, id Identity, f frame) (reply, error) {
	c, err := dial(ctx, dir, id)
	if err != nil {
		return reply{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	f.Version, f.Identity = Version, id
	if err := json.NewEncoder(c).Encode(f); err != nil {
		return reply{}, fmt.Errorf("runsock: send to %s: %w", id, err)
	}
	line, err := readFrame(c)
	if err != nil {
		return reply{}, fmt.Errorf("runsock: reply from %s: %w", id, err)
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return reply{}, fmt.Errorf("runsock: reply from %s: %w", id, err)
	}
	if !r.OK {
		return r, &RefusedError{Run: id.String(), Reason: r.Error}
	}
	if r.Status == nil || (f.Op == "control" && r.Request == nil) {
		return r, fmt.Errorf("runsock: reply from %s is incomplete", id)
	}
	return r, nil
}

// ReadStatus asks id's owner for one snapshot.
func ReadStatus(ctx context.Context, dir string, id Identity) (Status, error) {
	r, err := roundTrip(ctx, dir, id, frame{Op: "status"})
	if err != nil {
		return Status{}, err
	}
	return *r.Status, nil
}

// SendControl hands a control request to id's owner, which persists it
// before answering. generation, when set, refuses delivery to any other
// owner. A RefusedError is definite: nothing was recorded. Any other
// error after dialling leaves the outcome unknown — read the request
// back by id, and retry only with the same id.
func SendControl(ctx context.Context, dir string, id Identity, generation string, req runcontrol.Request) (runcontrol.Request, Status, error) {
	r, err := roundTrip(ctx, dir, id, frame{Op: "control", Generation: generation, Control: &req})
	if err != nil {
		return runcontrol.Request{}, Status{}, err
	}
	return *r.Request, *r.Status, nil
}

// The socket seams. Tests swap in an in-memory network keyed by path,
// because agent sandboxes may refuse AF_UNIX sockets outright.
var (
	listenUnix = func(path string) (net.Listener, error) {
		return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	}
	dialUnix = func(ctx context.Context, path string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
)

// SetTransport replaces how endpoints listen and dial, returning the
// restore. For tests (see runsocktest); production uses Unix sockets.
func SetTransport(listen func(path string) (net.Listener, error), dial func(ctx context.Context, path string) (net.Conn, error)) (restore func()) {
	prevL, prevD := listenUnix, dialUnix
	listenUnix, dialUnix = listen, dial
	return func() { listenUnix, dialUnix = prevL, prevD }
}

// RefusedError is the owner's own answer that it will not take a
// request. It is definite: nothing was recorded.
type RefusedError struct {
	Run, Reason string
}

func (e *RefusedError) Error() string { return "runsock: " + e.Run + ": " + e.Reason }
