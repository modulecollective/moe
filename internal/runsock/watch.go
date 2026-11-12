package runsock

// The watch op: one connection, a stream of complete Status snapshots.
//
// It is an overlay on the one-shot protocol, not a second one. The first
// frame is a full snapshot taken under the same mutex that registers the
// subscriber, so no Update can fall between them. After that the owner
// sends the latest snapshot whenever it has changed — rapid updates
// coalesce, there is no replay log and no per-reader queue — and an
// unchanged one every watchHeartbeat. A heartbeat proves contact with
// the owner, never agent progress. Publishing never waits on a reader:
// Update only sets a one-slot flag.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Watch cadence. A reader that hears nothing for watchSilence treats
// contact as lost; the owner writes at least every watchHeartbeat, and
// gives up on a reader that cannot take a frame within watchWrite.
var (
	watchHeartbeat = 5 * time.Second
	watchSilence   = 15 * time.Second
	watchWrite     = 5 * time.Second
)

// ErrWatchUnsupported reports an owner that explicitly refused the watch
// op: an older binary. Callers fall back to polling ReadStatus. Identity
// and version refusals are RefusedError, never this.
var ErrWatchUnsupported = errors.New("runsock: owner does not support watch")

// ErrContactLost reports a watch that heard nothing — not even a
// heartbeat — for watchSilence.
var ErrContactLost = errors.New("runsock: no frame from owner")

type subscriber struct {
	changed chan struct{} // one slot: "the snapshot moved since you last sent"
}

// subscribe registers a reader and returns the snapshot it starts from,
// atomically with respect to Update.
func (s *Server) subscribe() (*subscriber, Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, Status{}, false
	}
	sub := &subscriber{changed: make(chan struct{}, 1)}
	s.subs[sub] = struct{}{}
	return sub, s.status, true
}

func (s *Server) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
}

// notifyLocked flags every subscriber; s.mu is held. A full slot already
// means "send the latest", which is what coalescing is.
func (s *Server) notifyLocked() {
	for sub := range s.subs {
		select {
		case sub.changed <- struct{}{}:
		default:
		}
	}
}

// serveWatch streams snapshots to c until the peer goes, a write stalls,
// or the server closes. The handler's request deadline is replaced by a
// per-write one: an idle watch is not a slow peer.
func (s *Server) serveWatch(c net.Conn) {
	sub, st, ok := s.subscribe()
	if !ok {
		return
	}
	defer s.unsubscribe(sub)
	c.SetDeadline(time.Time{})
	send := func(st Status) bool {
		b, err := json.Marshal(reply{Version: Version, OK: true, Status: &st})
		if err != nil || len(b)+1 > maxFrame {
			return false
		}
		c.SetWriteDeadline(time.Now().Add(watchWrite))
		_, err = c.Write(append(b, '\n'))
		return err == nil
	}
	if !send(st) {
		return
	}
	heartbeat := time.NewTicker(watchHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-sub.changed:
		case <-heartbeat.C:
		case <-s.done:
			return
		}
		if !send(s.Snapshot()) {
			return
		}
	}
}

// frameReader reads newline-terminated frames of at most maxFrame bytes
// from one connection. It keeps its buffer across frames: a reader built
// per frame could swallow the start of the next one.
type frameReader struct{ br *bufio.Reader }

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{br: bufio.NewReaderSize(r, 4096)}
}

func (fr *frameReader) next() ([]byte, error) {
	var line []byte
	for {
		chunk, err := fr.br.ReadSlice('\n')
		if len(line)+len(chunk) > maxFrame {
			return nil, fmt.Errorf("frame exceeds %d bytes", maxFrame)
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("read frame: %w", err)
		}
	}
}

// checkReply validates a reply's version and, when it carries a status,
// that the status is for id. Anything else is not this run's owner.
func checkReply(id Identity, r reply) error {
	if r.Version != Version {
		return fmt.Errorf("runsock: reply from %s speaks protocol version %d, not %d", id, r.Version, Version)
	}
	if r.Status != nil && r.Status.Identity != id {
		return fmt.Errorf("runsock: reply for %s carries status for %s in %s", id, r.Status.Identity, r.Status.Identity.Root)
	}
	return nil
}

// Watch subscribes to id's owner and calls fn with each snapshot, in
// order, starting with a complete one. A snapshot older than one already
// delivered for the same generation is dropped. It returns when ctx ends
// (nil), the owner refuses (RefusedError or ErrWatchUnsupported), contact
// is lost (ErrContactLost), or the connection fails. fn runs on the
// caller's goroutine and must not block for long: frames queue in the
// socket while it runs.
func Watch(ctx context.Context, dir string, id Identity, fn func(Status)) error {
	c, err := dial(ctx, dir, id)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetWriteDeadline(time.Now().Add(watchWrite))
	if err := json.NewEncoder(c).Encode(frame{Version: Version, Op: "watch", Identity: id}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("runsock: send to %s: %w", id, err)
	}
	fr := newFrameReader(c)
	var gen string
	var rev uint64
	for first := true; ; first = false {
		c.SetReadDeadline(time.Now().Add(watchSilence))
		line, err := fr.next()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("%w for %s in %s", ErrContactLost, id, watchSilence)
			}
			return fmt.Errorf("runsock: watch %s: %w", id, err)
		}
		var r reply
		if err := json.Unmarshal(bytes.TrimSpace(line), &r); err != nil {
			return fmt.Errorf("runsock: watch %s: %w", id, err)
		}
		if !r.OK {
			if first && r.Error == "unknown op "+strconv.Quote("watch") {
				return fmt.Errorf("%w (%s)", ErrWatchUnsupported, id)
			}
			return &RefusedError{Run: id.String(), Reason: r.Error}
		}
		if err := checkReply(id, r); err != nil {
			return err
		}
		if r.Status == nil {
			return fmt.Errorf("runsock: watch frame from %s is incomplete", id)
		}
		st := *r.Status
		if st.Generation == gen && st.Revision < rev {
			continue
		}
		gen, rev = st.Generation, st.Revision
		fn(st)
	}
}
