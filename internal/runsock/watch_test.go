package runsock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func shortWatch(t *testing.T, heartbeat, silence, write time.Duration) {
	t.Helper()
	ph, ps, pw := watchHeartbeat, watchSilence, watchWrite
	watchHeartbeat, watchSilence, watchWrite = heartbeat, silence, write
	t.Cleanup(func() { watchHeartbeat, watchSilence, watchWrite = ph, ps, pw })
}

// watchInto runs Watch in the background, collecting snapshots.
type watchRun struct {
	mu    sync.Mutex
	got   []Status
	err   chan error
	stop  context.CancelFunc
	frame chan Status
}

func startWatch(t *testing.T, dir string, id Identity) *watchRun {
	t.Helper()
	c, cancel := context.WithCancel(context.Background())
	w := &watchRun{err: make(chan error, 1), stop: cancel, frame: make(chan Status, 1024)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.err <- Watch(c, dir, id, func(st Status) {
			w.mu.Lock()
			w.got = append(w.got, st)
			w.mu.Unlock()
			w.frame <- st
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return w
}

func (w *watchRun) next(t *testing.T) Status {
	t.Helper()
	select {
	case st := <-w.frame:
		return st
	case err := <-w.err:
		t.Fatalf("watch ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return Status{}
}

// waitFor reads frames until one satisfies ok.
func (w *watchRun) waitFor(t *testing.T, ok func(Status) bool) Status {
	t.Helper()
	for {
		if st := w.next(t); ok(st) {
			return st
		}
	}
}

// The first frame is complete and current; an update reaches the reader
// as a push, with no poll in between.
func TestWatchStreamsSnapshots(t *testing.T) {
	shortWatch(t, time.Hour, 5*time.Second, time.Second)
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	s.Update(func(st *Status) { st.Stage = "design" })
	w := startWatch(t, dir, runA)
	first := w.next(t)
	if first.Identity != runA || first.Stage != "design" || first.Generation == "" {
		t.Fatalf("first frame = %+v", first)
	}
	s.Update(func(st *Status) { st.Phase, st.Stage = PhaseRunning, "code" })
	got := w.next(t)
	if got.Phase != PhaseRunning || got.Stage != "code" || got.Revision <= first.Revision {
		t.Fatalf("pushed frame = %+v", got)
	}
}

// Updates racing the subscription are never lost: whatever the reader
// saw first, it converges on the owner's final snapshot.
func TestWatchSubscriptionRacesUpdate(t *testing.T) {
	shortWatch(t, time.Hour, 5*time.Second, time.Second)
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	for range 20 {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.Update(func(st *Status) { st.Waiting = "x" })
			}
		}()
		w := startWatch(t, dir, runA)
		wg.Wait()
		final := s.Snapshot().Revision
		w.waitFor(t, func(st Status) bool { return st.Revision == final })
		w.stop()
	}
}

// A reader that is not reading costs the publisher nothing: updates
// coalesce into the latest snapshot rather than queueing.
func TestWatchCoalescesAndNeverBlocksPublisher(t *testing.T) {
	shortWatch(t, time.Hour, 5*time.Second, time.Second)
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	c, err := dial(ctx(t), dir, runA)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go fmt.Fprintf(c, `{"version":1,"op":"watch","identity":{"root":"/b","project":"moe","run":"a"}}`+"\n")
	fr := newFrameReader(c)
	if _, err := fr.next(); err != nil {
		t.Fatal(err)
	}
	// Nobody reads now; on net.Pipe every write blocks until read.
	done := make(chan struct{})
	go func() {
		for range 1000 {
			s.Update(func(st *Status) { st.Waiting = "busy" })
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Update blocked behind a reader")
	}
	final := s.Snapshot().Revision
	frames := 0
	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := fr.next()
		if err != nil {
			t.Fatal(err)
		}
		frames++
		if strings.Contains(string(line), fmt.Sprintf(`"revision":%d,`, final)) {
			break
		}
	}
	if frames > 3 {
		t.Fatalf("%d frames for 1000 updates; want them coalesced", frames)
	}
}

// Heartbeats repeat the unchanged snapshot; activity is untouched.
func TestWatchHeartbeat(t *testing.T) {
	shortWatch(t, 20*time.Millisecond, 5*time.Second, time.Second)
	dir := sockDir(t, false)
	listen(t, dir, runA)
	w := startWatch(t, dir, runA)
	a, b := w.next(t), w.next(t)
	if a.Revision != b.Revision || !b.Activity.IsZero() {
		t.Fatalf("heartbeat changed the snapshot: %+v → %+v", a, b)
	}
}

// Close cuts idle watchers and writers stuck on a reader that never
// reads, and joins them.
func TestWatchCloseJoinsWatchers(t *testing.T) {
	shortWatch(t, time.Hour, 5*time.Second, time.Hour)
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	idle := startWatch(t, dir, runA)
	idle.next(t)
	stuck, err := dial(ctx(t), dir, runA)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	fmt.Fprintf(stuck, `{"version":1,"op":"watch","identity":{"root":"/b","project":"moe","run":"a"}}`+"\n")
	// The stuck reader never reads its first frame.
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join watch handlers")
	}
	select {
	case err := <-idle.err:
		if err == nil {
			t.Fatal("watch of a closed owner ended cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle watcher not cut")
	}
}

// A writer that cannot deliver within watchWrite gives up, so a dead
// peer never pins a handler.
func TestWatchDropsSlowReader(t *testing.T) {
	shortWatch(t, 10*time.Millisecond, 5*time.Second, 50*time.Millisecond)
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	c, err := dial(ctx(t), dir, runA)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, `{"version":1,"op":"watch","identity":{"root":"/b","project":"moe","run":"a"}}`+"\n")
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler kept a reader that never reads")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeOwner listens at id's path and answers each connection with body.
func fakeOwner(t *testing.T, dir string, id Identity, serve func(c net.Conn)) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := listenUnix(Path(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				newFrameReader(c).next()
				serve(c)
			}()
		}
	}()
}

func TestWatchRefusals(t *testing.T) {
	shortWatch(t, time.Hour, 200*time.Millisecond, time.Second)
	dir := sockDir(t, false)
	listen(t, dir, runA)

	// Wrong identity: the real owner refuses; it is not a fallback.
	fakeOwner(t, dir, runB, func(c net.Conn) {
		fmt.Fprintln(c, `{"version":1,"ok":false,"error":"endpoint serves moe/a in /b, not moe/b in /b"}`)
	})
	err := Watch(ctx(t), dir, runB, func(Status) {})
	var refused *RefusedError
	if !errors.As(err, &refused) || errors.Is(err, ErrWatchUnsupported) {
		t.Fatalf("identity refusal = %v", err)
	}

	// An old owner that does not know the op: fall back.
	old := Identity{Root: "/b", Project: "moe", Run: "old"}
	fakeOwner(t, dir, old, func(c net.Conn) {
		fmt.Fprintln(c, `{"version":1,"ok":false,"error":"unknown op \"watch\""}`)
	})
	if err := Watch(ctx(t), dir, old, func(Status) {}); !errors.Is(err, ErrWatchUnsupported) {
		t.Fatalf("old owner = %v", err)
	}

	// A reply in another protocol version is not this owner.
	v2 := Identity{Root: "/b", Project: "moe", Run: "v2"}
	fakeOwner(t, dir, v2, func(c net.Conn) {
		fmt.Fprintln(c, `{"version":2,"ok":true,"status":{"identity":{"root":"/b","project":"moe","run":"v2"}}}`)
	})
	if err := Watch(ctx(t), dir, v2, func(Status) {}); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("version mismatch = %v", err)
	}
	if _, err := ReadStatus(ctx(t), dir, v2); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("one-shot version mismatch = %v", err)
	}

	// A frame describing another run is refused, one-shot or streamed.
	liar := Identity{Root: "/b", Project: "moe", Run: "liar"}
	fakeOwner(t, dir, liar, func(c net.Conn) {
		fmt.Fprintln(c, `{"version":1,"ok":true,"status":{"identity":{"root":"/b","project":"moe","run":"a"}}}`)
	})
	if err := Watch(ctx(t), dir, liar, func(Status) {}); err == nil || !strings.Contains(err.Error(), "carries status for moe/a") {
		t.Fatalf("identity mismatch = %v", err)
	}
	if _, err := ReadStatus(ctx(t), dir, liar); err == nil || !strings.Contains(err.Error(), "carries status for moe/a") {
		t.Fatalf("one-shot identity mismatch = %v", err)
	}

	// A silent owner is lost contact, not a hang.
	silent := Identity{Root: "/b", Project: "moe", Run: "silent"}
	fakeOwner(t, dir, silent, func(c net.Conn) { time.Sleep(2 * time.Second) })
	if err := Watch(ctx(t), dir, silent, func(Status) {}); !errors.Is(err, ErrContactLost) {
		t.Fatalf("silent owner = %v", err)
	}
}

// Two frames arriving in one read are both delivered, in order; a
// revision older than one already seen for that generation is dropped,
// and a new generation starts over.
func TestWatchFramingAndRevisionOrder(t *testing.T) {
	shortWatch(t, time.Hour, 5*time.Second, time.Second)
	dir := sockDir(t, false)
	id := Identity{Root: "/b", Project: "moe", Run: "multi"}
	mk := func(gen string, rev int) string {
		return fmt.Sprintf(`{"version":1,"ok":true,"status":{"identity":{"root":"/b","project":"moe","run":"multi"},"generation":%q,"revision":%d}}`+"\n", gen, rev)
	}
	fakeOwner(t, dir, id, func(c net.Conn) {
		c.Write([]byte(mk("g1", 5) + mk("g1", 3) + mk("g1", 6) + mk("g2", 1)))
		time.Sleep(time.Second)
	})
	var got []string
	c, cancel := context.WithCancel(ctx(t))
	err := Watch(c, dir, id, func(st Status) {
		got = append(got, fmt.Sprintf("%s/%d", st.Generation, st.Revision))
		if len(got) == 3 {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "g1/5 g1/6 g2/1" {
		t.Fatalf("frames = %v", got)
	}
}

func TestFrameReaderBounds(t *testing.T) {
	r := newFrameReader(strings.NewReader("a\nb\n" + strings.Repeat("x", maxFrame+1) + "\n"))
	for _, want := range []string{"a\n", "b\n"} {
		if got, err := r.next(); err != nil || string(got) != want {
			t.Fatalf("next = %q, %v", got, err)
		}
	}
	if _, err := r.next(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized = %v", err)
	}
}
