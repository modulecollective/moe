package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu   sync.Mutex
	b    bytes.Buffer
	fail bool
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return 0, errors.New("terminal gone")
	}
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type loopHarness struct {
	out     *syncBuf
	input   chan []byte
	boards  chan Board
	jobs    chan job
	results chan jobResult
	done    chan struct{}
	pending *job
	err     error
}

func startLoop(t *testing.T, do func(Action) Result) *loopHarness {
	t.Helper()
	h := &loopHarness{out: &syncBuf{}, input: make(chan []byte), boards: make(chan Board), jobs: make(chan job, 1), results: make(chan jobResult, 1), done: make(chan struct{})}
	go worker(do, h.jobs, h.results)
	tio := loopIO{
		out: h.out, input: h.input, resize: make(chan struct{}), stop: make(chan struct{}), tstp: make(chan struct{}),
		size: func() (int, int, error) { return 24, 80, nil }, suspend: func() error { return nil },
	}
	go func() {
		defer close(h.done)
		h.pending, h.err = runLoop(context.Background(), Config{Boards: h.boards}, tio, h.jobs, h.results)
	}()
	t.Cleanup(func() {
		select {
		case <-h.done:
		case h.input <- []byte("q"):
			<-h.done
		}
		close(h.jobs)
	})
	return h
}

func (h *loopHarness) waitOut(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.out.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("never painted %q", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A pushed board is painted; typing is edited text; Ctrl-S runs the
// worker and its result is painted, all on one loop.
func TestLoopPaintsBoardsAndResults(t *testing.T) {
	got := make(chan Action, 1)
	h := startLoop(t, func(a Action) Result { got <- a; return Result{OK: true, Message: "noted for p/a"} })
	h.boards <- board(1, run("p", "a"))
	h.waitOut(t, "p/a")
	h.input <- []byte("nhi \xc3")
	h.input <- []byte("\xa9\x13") // é split across reads, then Ctrl-S
	if a := <-got; a.Text != "hi é" || a.Kind != ActNote {
		t.Fatalf("action = %+v", a)
	}
	h.waitOut(t, "noted for p/a")
}

// Quitting mid-write hands the in-flight job back so its outcome can be
// reported once the terminal is restored.
func TestLoopQuitDuringWrite(t *testing.T) {
	release := make(chan struct{})
	h := startLoop(t, func(a Action) Result { <-release; return Result{OK: true} })
	h.boards <- board(1, run("p", "a"))
	h.waitOut(t, "p/a")
	h.input <- []byte(":pause\r")
	h.waitOut(t, "working: pause p/a")
	h.input <- []byte("q")
	<-h.done
	if h.pending == nil || h.pending.label != "pause p/a" {
		t.Fatalf("pending = %+v", h.pending)
	}
	close(release)
	if r := <-h.results; !r.results[0].OK {
		t.Fatal("job lost")
	}
}

// A lone Escape leaves the editor after the wait; an output error ends
// the loop with that error.
func TestLoopEscapeAndOutputError(t *testing.T) {
	h := startLoop(t, func(Action) Result { return Result{OK: true} })
	h.boards <- board(1, run("p", "a"))
	h.input <- []byte("n")
	h.waitOut(t, "editing note")
	h.input <- []byte("\x1b")
	time.Sleep(2 * escWait)
	h.input <- []byte(":")
	h.waitOut(t, "→ p/a") // the Escape was a key: the editor closed and : opened
	h.out.mu.Lock()
	h.out.fail = true
	h.out.mu.Unlock()
	select {
	case h.input <- []byte("x"):
		<-h.done
	case <-h.done:
	}
	if h.err == nil || !strings.Contains(h.err.Error(), "terminal gone") {
		t.Fatalf("err = %v", h.err)
	}
}
