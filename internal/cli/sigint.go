package cli

import (
	"bufio"
	"io"
	"os"
	"os/signal"
	"time"
)

// SIGINT in cooked mode (queue countdown, stage `[Y/n/o]`/`[N/m/p]` prompts)
// gets the operator's "stop the queue / abort the chain" intent, not a
// runtime-default process tear-down. Inside an interactive Claude session
// the tty is in raw mode and Ctrl-C is delivered to claude as a byte —
// that stays untouched by design (the operator's Ctrl-C inside an agent
// is meant for the agent).
//
// The two helpers below are the shared shape: register a scoped
// signal.Notify channel, select between it and stdin / a timer, return
// "interrupted" rather than letting Go's default handler tear the
// process down.

// installSigint registers a buffered channel for os.Interrupt and
// returns it together with a deregister closure. Buffered to size 1 per
// signal package guidance — drops are fine, the operator only needs the
// first delivery to land.
func installSigint() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}

// readLineWithSignal reads a line from r in a goroutine and selects
// between the read result and sigCh. Returns the line on a normal read,
// or interrupted=true if sigCh fires first.
//
// Caller owns sigCh's lifecycle — typically `installSigint()` paired
// with a deferred unregister, or a test-supplied channel.
//
// On interrupt the reader goroutine remains blocked on r until r
// produces or closes; that orphan is fine for moe's lifecycle (the
// process exits shortly after, and r is os.Stdin, which the OS reaps).
func readLineWithSignal(r io.Reader, sigCh <-chan os.Signal) (line string, interrupted bool, err error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, e := bufio.NewReader(r).ReadString('\n')
		ch <- result{line: s, err: e}
	}()
	select {
	case res := <-ch:
		return res.line, false, res.err
	case <-sigCh:
		return "", true, nil
	}
}

// queueCountdownTick is the per-tick interval the walker uses between
// countdown frames. var rather than const so tests can speed it up to
// milliseconds; production stays at one second.
var queueCountdownTick = 1 * time.Second

// runCountdown prints a "starting <label> in N…" line and ticks down
// to zero. Returns true if sigCh fired during the wait (caller stops),
// false if the countdown ran to completion (caller dispatches).
//
// Each tick rewrites the same line in place via \r so the countdown
// collapses to one visible line. On signal or completion a final
// newline is emitted so any subsequent output starts cleanly.
func runCountdown(seconds int, label string, stdout io.Writer, sigCh <-chan os.Signal) bool {
	for n := seconds; n > 0; n-- {
		moePrintf(stdout, "\rqueue: starting %s in %d…  (Ctrl-C to stop)", label, n)
		select {
		case <-sigCh:
			moePrintln(stdout, "")
			return true
		case <-time.After(queueCountdownTick):
		}
	}
	moePrintln(stdout, "")
	return false
}
