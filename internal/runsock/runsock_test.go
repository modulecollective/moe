package runsock

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runsock/runsocktest"
)

// sockDir is short on purpose: t.TempDir embeds the test name and can
// push a socket path past the ~108-byte limit. Every test but the real
// binding probe runs over the in-memory namespace, because agent
// sandboxes may refuse AF_UNIX.
func sockDir(t *testing.T, real bool) string {
	t.Helper()
	if !real {
		n := runsocktest.New()
		t.Cleanup(SetTransport(n.Listen, n.Dial))
	}
	d, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d + "/runs"
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func listen(t *testing.T, dir string, id Identity) *Server {
	t.Helper()
	s, err := Listen(dir, id, Status{Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var (
	runA = Identity{Root: "/b", Project: "moe", Run: "a"}
	runB = Identity{Root: "/b", Project: "moe", Run: "b"}
)

// rawRequest sends body as one frame on a's socket and returns the reply.
func rawRequest(t *testing.T, dir string, id Identity, body string) reply {
	t.Helper()
	c, err := dial(ctx(t), dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go c.Write([]byte(body))
	line, err := readFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The binding probe: an actual Unix socket in a private directory, a
// stale socket file cleared, closing removes it, a missing listener
// reads as ErrNoOwner. Skips only when the sandbox refuses AF_UNIX —
// the protocol tests below cover the rest over the in-memory seam.
func TestRealSocket(t *testing.T) {
	dir := sockDir(t, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	probe, err := net.ListenUnix("unix", &net.UnixAddr{Name: dir + "/p.sock", Net: "unix"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("this environment refuses AF_UNIX sockets (%v); protocol tests run over runsocktest", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()

	if _, err := ReadStatus(ctx(t), dir, runA); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("no listener: %v", err)
	}
	if err := os.WriteFile(Path(dir, runA), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatus(ctx(t), dir, runA); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("stale file: %v", err)
	}
	s := listen(t, dir, runA)
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir mode = %v, %v", fi.Mode(), err)
	}
	if st, err := ReadStatus(ctx(t), dir, runA); err != nil || st.Identity != runA {
		t.Fatalf("status = %+v, %v", st, err)
	}
	s.Close()
	if _, err := os.Stat(Path(dir, runA)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file outlived Close: %v", err)
	}
}

// Two runs, two endpoints; each answers only for its own identity, and
// a phase change restamps its since-time.
func TestEndpointsAnswerOnlyForThemselves(t *testing.T) {
	dir := sockDir(t, false)
	a, b := listen(t, dir, runA), listen(t, dir, runB)
	before := a.Snapshot()
	a.Update(func(s *Status) { s.Phase, s.Stage = PhaseRunning, "code" })
	b.Update(func(s *Status) { s.Phase, s.Stage = PhaseDeciding, "review" })

	sa, err := ReadStatus(ctx(t), dir, runA)
	if err != nil || sa.Stage != "code" || sa.Identity != runA || sa.Revision <= before.Revision {
		t.Fatalf("a = %+v, %v", sa, err)
	}
	if sa.PhaseSince.Before(before.PhaseSince) {
		t.Fatal("phase change did not restamp PhaseSince")
	}
	sb, err := ReadStatus(ctx(t), dir, runB)
	if err != nil || sb.Stage != "review" || sb.Generation == sa.Generation {
		t.Fatalf("b = %+v, %v", sb, err)
	}

	r := rawRequest(t, dir, runA, `{"version":1,"op":"status","identity":{"root":"/b","project":"moe","run":"b"}}`+"\n")
	if r.OK || !strings.Contains(r.Error, "not moe/b") {
		t.Fatalf("cross-identity reply = %+v", r)
	}
	r = rawRequest(t, dir, runA, `{"version":99,"op":"status","identity":{"root":"/b","project":"moe","run":"a"}}`+"\n")
	if r.OK || !strings.Contains(r.Error, "protocol version 99") {
		t.Fatalf("wrong-version reply = %+v", r)
	}
}

// An oversized frame is refused; a peer that sends nothing is cut off
// at the handler deadline, and Close joins the handler either way.
func TestRequestsAreBounded(t *testing.T) {
	prev := handlerTimeout
	handlerTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handlerTimeout = prev })
	dir := sockDir(t, false)
	s := listen(t, dir, runA)

	r := rawRequest(t, dir, runA, strings.Repeat("x", maxFrame+10))
	if r.OK || !strings.Contains(r.Error, "exceeds") {
		t.Fatalf("oversized reply = %+v", r)
	}
	silent, err := dial(ctx(t), dir, runA)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join a handler stuck on a silent peer")
	}
	if _, err := ReadStatus(ctx(t), dir, runA); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("after Close: %v, want ErrNoOwner", err)
	}
}

// A control goes to the owner's Submit under the dispatch lock and comes
// back as recorded; one addressed to another owner generation, or one
// the owner refuses, is a definite RefusedError.
func TestSendControl(t *testing.T) {
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	var got []runcontrol.Request
	s.Submit = func(r runcontrol.Request) (runcontrol.Request, error) {
		if r.Kind == runcontrol.KindFinish {
			return runcontrol.Request{}, errors.New("run is held")
		}
		got = append(got, r)
		r.State = runcontrol.StateAccepted
		return r, nil
	}
	gen := s.Snapshot().Generation
	r, st, err := SendControl(ctx(t), dir, runA, gen, runcontrol.Request{ID: "p1", Kind: runcontrol.KindPark})
	if err != nil || r.ID != "p1" || r.State != runcontrol.StateAccepted || st.Generation != gen || len(got) != 1 {
		t.Fatalf("SendControl = %+v, %+v, %v", r, st, err)
	}
	var refused *RefusedError
	if _, _, err := SendControl(ctx(t), dir, runA, "someone-else", runcontrol.Request{ID: "p2", Kind: runcontrol.KindPark}); !errors.As(err, &refused) {
		t.Fatalf("stale generation: %v, want RefusedError", err)
	}
	if _, _, err := SendControl(ctx(t), dir, runA, "", runcontrol.Request{ID: "f1", Kind: runcontrol.KindFinish}); !errors.As(err, &refused) || !strings.Contains(err.Error(), "held") {
		t.Fatalf("refused submit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("refused requests reached Submit: %+v", got)
	}
}

// Checkpoint publishes the admitted transition before a request queued
// behind it can be acknowledged.
func TestCheckpointSerialisesWithSubmit(t *testing.T) {
	dir := sockDir(t, false)
	s := listen(t, dir, runA)
	inCheckpoint := make(chan struct{})
	release := make(chan struct{})
	var phaseAtSubmit string
	s.Submit = func(r runcontrol.Request) (runcontrol.Request, error) {
		phaseAtSubmit = s.Snapshot().Phase
		return r, nil
	}
	go s.Checkpoint(func() bool {
		close(inCheckpoint)
		<-release
		return true
	}, func(st *Status) { st.Phase = PhaseShipping })
	<-inCheckpoint
	sent := make(chan error, 1)
	go func() {
		_, _, err := SendControl(ctx(t), dir, runA, "", runcontrol.Request{ID: "p", Kind: runcontrol.KindPause})
		sent <- err
	}()
	time.Sleep(50 * time.Millisecond) // give the submit a chance to overtake, which it must not
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if phaseAtSubmit != PhaseShipping {
		t.Fatalf("submit saw phase %q, want the admitted %q", phaseAtSubmit, PhaseShipping)
	}
}
