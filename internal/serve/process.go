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
// budget for the agent to flush, moe to run session.Close +
// sync.AutoPush, and moe to print the chain prompt.
//
// Variable rather than const so tests can shorten the wait; not
// part of the package's surface and not safe to mutate concurrently
// with a running ListenAndServe.
var shutdownSoftGrace = 10 * time.Second

// shutdownHangupGrace is the second wait after hanging up the PTY
// for stragglers that didn't exit on Ctrl-C. Total shutdown budget
// is roughly shutdownIntrGap + shutdownSoftGrace + shutdownHangupGrace.
var shutdownHangupGrace = 10 * time.Second

// child is one PTY-backed moe run the server is parenting.
type child struct {
	id      string // "<project>/<slug>" — known at spawn time
	cmd     *exec.Cmd
	pty     *pty.Pty
	started time.Time

	done    chan struct{}
	exitErr error
}

// children is the live PTY-child registry, keyed by id.
type children struct {
	mu  sync.RWMutex
	all map[string]*child
	// notify fires once per child on natural exit (not on server
	// shutdown). Empty by default; Server.New wires it when
	// Options.NotifyURL is set.
	notify func(id string, exitErr error)
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
	// `next: …` chain prompt (SkipNextStage=true). Without it the
	// child blocks forever on a prompt with no input source under
	// serve.
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
	notify := cs.notify
	cs.all[id] = c
	cs.mu.Unlock()

	go c.read(logger, notify)
	return c, nil
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
//     + AutoPush → moe prints the chain prompt → the second \x03
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
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
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
// closes done. Calls notify (if non-nil) once the exit status is
// known. Output is dropped on the floor — the per-run page is a
// monitor surface, not a remote terminal; the agent inside moe
// handles its own per-document transcript mirror at session close.
func (c *child) read(logger io.Writer, notify func(string, error)) {
	buf := make([]byte, 4096)
	for {
		_, err := c.pty.File().Read(buf)
		if err != nil {
			break
		}
	}
	c.exitErr = c.cmd.Wait()
	close(c.done)
	if logger != nil {
		fmt.Fprintf(logger, "serve: child %s exited: %v\n", c.id, c.exitErr)
	}
	if notify != nil {
		notify(c.id, c.exitErr)
	}
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
