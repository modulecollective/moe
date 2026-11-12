package ui

import (
	"context"
	"errors"
	"io"
	"time"
)

// Config is what the caller supplies.
type Config struct {
	// Boards delivers complete boards. The caller coalesces: the console
	// only ever paints the latest.
	Boards <-chan Board
	// Do makes one mutation. It runs on the console's single worker, one
	// call at a time, never on the loop goroutine.
	Do func(Action) Result
	// Report publishes status for a new board through the console's
	// writer (Rex OSC records); Unreport clears it on exit. Both run on
	// the loop goroutine, which is the terminal's only writer.
	Report   func(w io.Writer, b Board) error
	Unreport func(w io.Writer)
}

// escWait is how long a lone Escape waits for the rest of a sequence.
const escWait = 40 * time.Millisecond

// loopIO is the terminal as the loop sees it, so tests can drive the
// loop with no terminal at all.
type loopIO struct {
	out     io.Writer
	input   <-chan []byte // closed when the terminal goes
	resize  <-chan struct{}
	size    func() (rows, cols int, err error)
	stop    <-chan struct{} // a terminating signal
	tstp    <-chan struct{} // a stop request from outside (SIGTSTP)
	suspend func() error    // restore, stop, and re-enter on continue
}

// errStopped reports a terminating signal.
var errStopped = errors.New("moe ui: stopped by signal")

// worker runs jobs one at a time. A save that fails ends its job: the
// tag after it never runs.
func worker(do func(Action) Result, jobs <-chan job, results chan<- jobResult) {
	for j := range jobs {
		r := jobResult{job: j}
		for _, a := range j.steps {
			res := do(a)
			r.results = append(r.results, res)
			if !res.OK {
				break
			}
		}
		results <- r
	}
}

// runLoop is the console. It returns the job still in flight when the
// operator quit mid-write, so the caller can report its outcome after
// the terminal is restored.
func runLoop(ctx context.Context, cfg Config, tio loopIO, jobs chan<- job, results <-chan jobResult) (*job, error) {
	m := newModel(jobs)
	rows, cols, err := tio.size()
	if err != nil {
		return nil, err
	}
	m.resize(rows, cols)
	var dec decoder
	var esc <-chan time.Time
	paint := func() error {
		_, err := tio.out.Write(m.view().paint())
		return err
	}
	if err := paint(); err != nil {
		return nil, err
	}
	for {
		var eff effect
		select {
		case <-ctx.Done():
			return m.busy, nil
		case <-tio.stop:
			return m.busy, errStopped
		case <-tio.tstp:
			eff = effSuspend
		case b := <-cfg.Boards:
			m.setBoard(b)
			if cfg.Report != nil {
				// Best effort: a failed report retries with the next board.
				_ = cfg.Report(tio.out, m.board)
			}
		case r := <-results:
			m.finished(r)
		case <-tio.resize:
			r, c, err := tio.size()
			if err == nil {
				m.resize(r, c)
			}
		case p, ok := <-tio.input:
			if !ok {
				return m.busy, nil // the terminal went away
			}
			for _, k := range dec.feed(p) {
				if eff = m.key(k); eff != effNone {
					break
				}
			}
			esc = nil
			if eff == effNone && dec.pending() {
				esc = time.After(escWait)
			}
		case <-esc:
			esc = nil
			for _, k := range dec.flush() {
				if eff = m.key(k); eff != effNone {
					break
				}
			}
		}
		switch eff {
		case effQuit:
			return m.busy, nil
		case effSuspend:
			if err := tio.suspend(); err != nil {
				return m.busy, err
			}
			if r, c, err := tio.size(); err == nil {
				m.resize(r, c)
			}
		}
		if err := paint(); err != nil {
			return m.busy, err
		}
	}
}
