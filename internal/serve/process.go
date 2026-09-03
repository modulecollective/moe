package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/serve/pty"
)

// shutdownIntrGap is the pause between the two \x03 (Ctrl-C) bytes
// written to each child's PTY during shutdown. Two Ctrl-Cs is the
// same byte path a human at the terminal would take: in raw mode
// claude treats them as two interrupts and exits cleanly; in cooked
// mode (moe at a chain prompt) the kernel converts each to SIGINT
// on the foreground process group, which `readLineWithSignal` turns
// into a clean exit. 100ms is enough for the first byte to be
// consumed as a discrete event before the second arrives.
const shutdownIntrGap = 100 * time.Millisecond

// shutdownSoftGrace is how long the four-phase shutdown waits after
// the two Ctrl-Cs for a child to exit naturally. Moe at the chain
// prompt is the common end-state once the agent exits — this is the
// budget for the agent to flush, moe to run session.Close, and moe to
// print the chain prompt. Session close is local-only now (serve's own
// pusher takes the commit to origin), but the budget stays: the flush
// and the rebase are what it was really sized for.
//
// Variable rather than const so tests can shorten the wait; not
// part of the package's surface and not safe to mutate concurrently
// with a running ListenAndServe.
var shutdownSoftGrace = 10 * time.Second

// shutdownHangupGrace is the second wait after hanging up the PTY
// for stragglers that didn't exit on Ctrl-C. Total shutdown budget
// is roughly shutdownIntrGap + shutdownSoftGrace + shutdownHangupGrace.
var shutdownHangupGrace = 10 * time.Second

// childTailBytes is how much of a child's PTY output the registry keeps.
// A sweep that dies to a vendor error used to leave an exit code and
// nothing else; 8KB is a few screens — the error, whatever framed it, and
// the moe line around that — which is the difference between a glance
// and an ssh session.
//
// Bounded and in memory only. It never reaches the state file, which
// stays small enough for `moe dash` to re-read on every watch repaint.
const childTailBytes = 8 << 10

// child is one PTY-backed moe run the server is parenting.
type child struct {
	id      string // "<project>/<slug>" — known at spawn time
	cmd     *exec.Cmd
	pty     *pty.Pty
	started time.Time

	done    chan struct{}
	exitErr error

	// tailMu guards tailBuf, which the reader goroutine appends to while
	// a request handler may be rendering it.
	tailMu  sync.Mutex
	tailBuf []byte
}

// children is the live PTY-child registry, keyed by id.
type children struct {
	mu  sync.RWMutex
	all map[string]*child
	// notify fires once per child on natural exit. Empty by default;
	// Server.New wires it when Options.NotifyURL is set. Read through
	// exitNotifier rather than captured at spawn, so shutdown can
	// suppress it for children it is killing itself.
	notify func(id string, exitErr error)
	// stopping is raised once by shutdown and never cleared — a
	// registry only shuts down once. It is what makes notify's "on
	// natural exit" true: a child serve interrupted exits with a
	// status the operator caused, so posting it as a sweep failure
	// would be a false alarm.
	stopping bool
	// onSpawn and onExit are the activity record's hooks: one call per
	// child started, one per child reaped, with the output tail attached
	// to the second. Wired once in Server.New, at the registry level
	// rather than at the one spawn site there is today (the heartbeat
	// sweep), so any future spawner lands in the ring without
	// remembering to say so.
	onSpawn func(id string, at time.Time)
	onExit  func(id string, at time.Time, exitErr error, tail string)
}

func newChildren() *children {
	return &children{all: map[string]*child{}}
}

// spawn starts a moe run as a PTY child and records it under id.
// The caller has already validated id and constructed args. Returns
// an error if a live child already holds id.
//
// root is the bureaucracy root, used as cmd.Dir. The agent inside
// the spawned moe handles its own per-document transcript mirror
// (see internal/agent/claude/executor.go's CopyTranscript call) —
// serve doesn't snag JSONL on its own.
func (cs *children) spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs.mu.Lock()
	if existing, dup := cs.all[id]; dup {
		// If the prior run already exited, replace it. Otherwise refuse.
		select {
		case <-existing.done:
			delete(cs.all, id)
		default:
			cs.mu.Unlock()
			return nil, fmt.Errorf("serve: run %s already live", id)
		}
	}

	cmd := exec.Command(moeBin, args...)
	cmd.Dir = root
	// Inherit env, then force a recognized TERM so claude/codex
	// render, and set MOE_SERVE_AGENT=1 — the serve↔CLI handshake
	// that tells the spawned stage opener to suppress its post-turn
	// `next: …` chain prompt (SkipNextStage=true).
	//
	// Today's only child (the heartbeat sweep) is opened headless, so
	// the prompt is already suppressed structurally and the env var is
	// insurance. It stays because the hazard lives here, not in the
	// caller: this spawner is generic, and a PTY child passes
	// stdinIsTerminal() — so a future child that isn't headless by its
	// args would sit on a prompt aimed at a terminal nobody types into,
	// with no error and no exit. The env var inherits to any `moe`
	// grandchildren too, which the args-level suppression does not.
	env := append([]string{}, os.Environ()...)
	env = append(env, "TERM=xterm-256color", "MOE_SERVE_AGENT=1")
	cmd.Env = env

	p, err := pty.Start(cmd)
	if err != nil {
		cs.mu.Unlock()
		return nil, err
	}

	c := &child{
		id:      id,
		cmd:     cmd,
		pty:     p,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	onSpawn, onExit := cs.onSpawn, cs.onExit
	cs.all[id] = c
	cs.mu.Unlock()

	if onSpawn != nil {
		onSpawn(id, c.started)
	}
	go c.read(cs, logger, onExit)
	return c, nil
}

// exitNotifier returns the hook to fire for a child that has just
// exited, or nil once shutdown has started. Looked up at exit time
// rather than captured at spawn: a child spawned long before the
// Ctrl-C still has to be silenced by it.
func (cs *children) exitNotifier() func(id string, exitErr error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.stopping {
		return nil
	}
	return cs.notify
}

func (cs *children) get(id string) (*child, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c, ok := cs.all[id]
	return c, ok
}

// remove drops id from the registry. Idempotent — a no-op when id isn't
// present. Called after a successful sdlc close so a lingering exited
// child stops marking the now-gone run as parented on the dash and the
// per-run page.
func (cs *children) remove(id string) {
	cs.mu.Lock()
	delete(cs.all, id)
	cs.mu.Unlock()
}

// shutdown winds children down in four phases so the agent (and
// moe-the-parent of the agent) gets a real chance to commit, push,
// and exit cleanly before the kernel reaps anything:
//
//  1. Write \x03\x03 (two Ctrl-Cs, ~100ms apart) to every live
//     child's PTY master. The tty mode does the routing: raw mode
//     (claude active) sees two interrupts and exits; cooked mode
//     (moe at a chain prompt) converts each to SIGINT on the fg
//     pgrp and `readLineWithSignal` returns interrupted → moe
//     exits 0. In the transition gap the byte sits in the tty input
//     buffer until something reads it.
//  2. Wait up to shutdownSoftGrace for natural exit. The common
//     end-state: agent flushes and exits → moe runs session.Close
//     → moe prints the chain prompt → the second \x03
//     (now in cooked mode) interrupts the prompt → moe exits.
//  3. For stragglers, pty.Close() → kernel SIGHUP via the
//     controlling terminal. Same blunt instrument as the old
//     behavior, just gated on the agent having declined to leave
//     politely.
//  4. Wait up to shutdownHangupGrace for the hung-up stragglers to
//     drain. Anything still alive after that is left for the
//     kernel to reap on os.Exit; logged so the operator knows.
//
// logger is the serve logger (the per-server io.Writer); nil means
// quiet shutdown. ctx caps the whole operation — useful if the
// operator hits Ctrl-C twice (the second SIGINT collapses through
// the Go runtime's default handler).
func (cs *children) shutdown(ctx context.Context, logger io.Writer) {
	cs.mu.Lock()
	// Raised before the early return below, not alongside the live
	// children: the narrow case this closes is a child that exited a
	// moment ago whose reader hasn't reached its notify call yet, and
	// that child is not in live.
	cs.stopping = true
	live := make([]*child, 0, len(cs.all))
	for _, c := range cs.all {
		select {
		case <-c.done:
			// Already exited; nothing to do.
		default:
			live = append(live, c)
		}
	}
	cs.mu.Unlock()

	if len(live) == 0 {
		return
	}
	shutLogf(logger, "shutdown: sending Ctrl-C to %d children", len(live))

	// Phase 1: two Ctrl-Cs to every live child.
	for _, c := range live {
		_ = c.writeRaw([]byte{0x03})
	}
	select {
	case <-time.After(shutdownIntrGap):
	case <-ctx.Done():
		return
	}
	for _, c := range live {
		_ = c.writeRaw([]byte{0x03})
	}

	// Phase 2: wait for natural exit.
	stillLive := waitForExit(live, shutdownSoftGrace, ctx)
	if len(stillLive) == 0 {
		shutLogf(logger, "shutdown: %d/%d children exited cleanly", len(live), len(live))
		return
	}
	shutLogf(logger, "shutdown: %d/%d still live after grace, hanging up PTY", len(stillLive), len(live))

	// Phase 3: hang up the master fd for stragglers.
	for _, c := range stillLive {
		_ = c.pty.Close()
	}

	// Phase 4: bounded wait for the hung-up stragglers.
	final := waitForExit(stillLive, shutdownHangupGrace, ctx)
	if len(final) > 0 {
		shutLogf(logger, "shutdown: %d still live after hangup, leaving for kernel reap", len(final))
	}
}

// waitForExit waits up to grace (or ctx cancellation) for every
// child in cs to close c.done. Returns the children that didn't
// exit in time.
func waitForExit(cs []*child, grace time.Duration, ctx context.Context) []*child {
	deadline := time.Now().Add(grace)
	var stillLive []*child
	for _, c := range cs {
		remaining := max(time.Until(deadline), 0)
		timer := time.NewTimer(remaining)
		select {
		case <-c.done:
			timer.Stop()
		case <-timer.C:
			stillLive = append(stillLive, c)
		case <-ctx.Done():
			timer.Stop()
			// Treat remaining children as still-live; caller
			// decides what to do.
			stillLive = append(stillLive, c)
		}
	}
	return stillLive
}

func shutLogf(logger io.Writer, format string, a ...any) {
	if logger == nil {
		return
	}
	fmt.Fprintf(logger, format+"\n", a...)
}

// read drains the master PTY until EIO, then reaps the child and
// closes done. It then asks the registry for a notify hook — nil once
// shutdown has raised stopping, so a child serve killed itself sends
// nothing — and finally calls onExit with the output tail.
//
// Only the last childTailBytes are kept, and only so a child that died
// can say what of. This is still not a remote terminal — nothing streams,
// nothing is stored past the process's own lifetime, and the agent inside
// moe handles its own per-document transcript mirror at session close.
func (c *child) read(cs *children, logger io.Writer, onExit func(string, time.Time, error, string)) {
	buf := make([]byte, 4096)
	for {
		n, err := c.pty.File().Read(buf)
		if n > 0 {
			c.appendTail(buf[:n])
		}
		if err != nil {
			break
		}
	}
	c.exitErr = c.cmd.Wait()
	close(c.done)
	if logger != nil {
		fmt.Fprintf(logger, "serve: child %s exited: %v\n", c.id, c.exitErr)
	}
	if notify := cs.exitNotifier(); notify != nil {
		notify(c.id, c.exitErr)
	}
	if onExit != nil {
		onExit(c.id, time.Now(), c.exitErr, c.tail())
	}
}

// appendTail folds one PTY read into the bounded tail, dropping the
// oldest bytes once it is full.
func (c *child) appendTail(p []byte) {
	c.tailMu.Lock()
	defer c.tailMu.Unlock()
	c.tailBuf = append(c.tailBuf, p...)
	if len(c.tailBuf) > childTailBytes {
		c.tailBuf = c.tailBuf[len(c.tailBuf)-childTailBytes:]
	}
}

// tail returns the kept output as a string. Safe to call while the
// reader is still running.
func (c *child) tail() string {
	c.tailMu.Lock()
	defer c.tailMu.Unlock()
	return string(c.tailBuf)
}

// writeRaw writes b verbatim to the child's PTY — no newline, no
// other framing. Used for control characters (Ctrl-C / 0x03) sent
// during shutdown.
func (c *child) writeRaw(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := c.pty.File().Write(b)
	return err
}

// snapshot reports the child's exit state. Safe to call from request
// handlers without blocking the reader.
func (c *child) snapshot() (exited bool, exitErr error) {
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
	default:
	}
	return
}
