package ui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func animatedBoard() Board {
	b := board(1, run("p", "a"))
	b.Overview = []string{"smoke A", "fixed rail"}
	b.Art = Art{Start: 0, Height: 2, Frames: [][]string{{"smoke A", "fixed rail"}, {"smoke B", "fixed rail"}}}
	return b
}

func TestAnimationPatchesOnlyVisibleArt(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(animatedBoard())
	keyboard(m, "nmy draft")
	m.say("failure and recovery stay here")
	old := m.view()
	before := slices.Clone(old.rows)
	out := string(m.animate(&old))
	if !strings.Contains(out, "\x1b[3;1Hsmoke B") || strings.Contains(out, "fixed rail") || strings.Contains(out, "draft") || strings.Contains(out, "failure") {
		t.Fatalf("wrong patch: %q", out)
	}
	cursor := fmt.Sprintf("\x1b[%d;%dH\x1b[?25h", old.curRow+1, old.curCol+1)
	if !strings.Contains(out, cursor) {
		t.Fatal("cursor not restored")
	}
	for i := range before {
		if i != 2 && before[i] != old.rows[i] {
			t.Fatalf("non-art row %d changed", i)
		}
	}
	if got := m.view(); !slices.Equal(got.rows, old.rows) || got.curRow != old.curRow || got.curCol != old.curCol {
		t.Fatal("full paint disagrees with animation")
	}
	// Scrolling the board does not move the pinned art.
	m.board.Lines = append(m.board.Lines, make([]string, 100)...)
	m.boardTop = 10
	if p := m.animate(&old); len(p) == 0 {
		t.Fatal("scroll hid pinned art")
	}
	m.boardTop = 0
	m.board.Art.Frames[1] = slices.Clone(m.board.Art.Frames[0])
	old = m.view()
	if p := m.animate(&old); len(p) != 0 {
		t.Fatalf("identical frame wrote: %q", p)
	}
	m.resize(10, 30)
	old = m.view()
	if p := m.animate(&old); len(p) != 0 {
		t.Fatal("animation overwrote resize notice")
	}
}

func TestLoopAnimationIsIndependentOfObservation(t *testing.T) {
	if animationInterval != 500*time.Millisecond {
		t.Fatalf("cadence: %v", animationInterval)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	boards := make(chan Board)
	input := make(chan []byte)
	ticks := make(chan time.Time)
	var reports atomic.Int32
	jobs := make(chan job, 1)
	results := make(chan jobResult, 1)
	done := make(chan error, 1)

	go func() {
		_, err := runLoop(ctx, Config{Boards: boards, Report: func(_ io.Writer, b Board) error { reports.Add(1); return nil }}, loopIO{
			out: out, input: input, animation: ticks, size: func() (int, int, error) { return 30, 80, nil },
		}, jobs, results)
		done <- err
	}()
	h := &loopHarness{out: out}
	boards <- animatedBoard()
	h.waitOut(t, "smoke A")
	input <- []byte("nprivate draft")
	h.waitOut(t, "private draft")
	// No board arrives while a source's next refresh is blocked. The UI
	// can still animate, without calling the status reporter again.
	for i := 0; i < 4; i++ {
		start := len(out.String())
		ticks <- time.Now()
		deadline := time.Now().Add(time.Second)
		for len(out.String()) == start {
			if time.Now().After(deadline) {
				t.Fatal("cached smoke did not advance")
			}
			time.Sleep(time.Millisecond)
		}
		patch := out.String()[start:]
		if strings.Contains(patch, "MoE ui") || strings.Contains(patch, "private") || !strings.Contains(patch, "smoke ") {
			t.Fatalf("full paint on tick: %q", patch)
		}
	}
	if reports.Load() != 1 {
		t.Fatalf("animation reported status %d times", reports.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
