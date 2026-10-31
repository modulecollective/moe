package serve

import (
	"context"
	"io"
	"os"
	"os/exec"
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

// localProc holds one process and its PTY master, drained by read and
// wound down by shutdown's four phases.
type localProc struct {
	cmd *exec.Cmd
	pty *pty.Pty
}

type localHost struct {
	cs    *children
	procs map[*child]*localProc
}

func newLocalHost(cs *children) *localHost { return &localHost{cs: cs, procs: map[*child]*localProc{}} }

func (h *localHost) reconcile() error { return nil }
func (h *localHost) spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs := h.cs
	if cs.beforeLaunch != nil {
		cs.beforeLaunch(id)
	}
	cs.mu.Lock()

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
	env = append(env, "TERM=xterm-256color", "MOE_SERVE_AGENT=1", "MOE_HOME="+root)
	cmd.Env = env

	p, err := pty.Start(cmd)
	if err != nil {
		cs.mu.Unlock()
		return nil, err
	}

	c := &child{
		id:      id,
		started: time.Now(),
		done:    make(chan struct{}), settled: make(chan struct{}), dispatched: true,
	}
	delete(h.procs, cs.all[id])
	h.procs[c] = &localProc{cmd: cmd, pty: p}
	cs.mu.Unlock()
	cs.register(c, "started on a local pty")
	go h.read(c, h.procs[c], logger)
	return c, nil
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
func (h *localHost) shutdown(ctx context.Context, logger io.Writer) {
	cs := h.cs
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
		_ = h.procs[c].writeRaw([]byte{0x03})
	}
	select {
	case <-time.After(shutdownIntrGap):
	case <-ctx.Done():
		return
	}
	for _, c := range live {
		_ = h.procs[c].writeRaw([]byte{0x03})
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
		_ = h.procs[c].pty.Close()
	}

	// Phase 4: bounded wait for the hung-up stragglers.
	final := waitForExit(stillLive, shutdownHangupGrace, ctx)
	if len(final) > 0 {
		shutLogf(logger, "shutdown: %d still live after hangup, leaving for kernel reap", len(final))
	}
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
func (h *localHost) read(c *child, proc *localProc, logger io.Writer) {
	buf := make([]byte, 4096)
	for {
		n, err := proc.pty.File().Read(buf)
		if n > 0 {
			c.appendTail(buf[:n])
		}
		if err != nil {
			break
		}
	}
	c.finish(h.cs, proc.cmd.Wait(), logger)
	close(c.settled)
}

// writeRaw writes b verbatim to the child's PTY — no newline, no
// other framing. Used for control characters (Ctrl-C / 0x03) sent
// during shutdown.
func (c *localProc) writeRaw(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := c.pty.File().Write(b)
	return err
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
