package git

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// blackHoleProxy is an https proxy that completes the TCP handshake and
// then says nothing — the shape a wedged push actually has, where the
// socket is up and no bytes ever come back. It returns the listener's
// address and a channel carrying the first connection it accepted.
func blackHoleProxy(t *testing.T) (addr string, accepted <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return ln.Addr().String(), conns
}

// TestCombinedContext_TimeoutReapsTransportChild is the whole point of
// the context primitive, and it pins the process-group decision along
// with the deadline.
//
// The deadline assertion alone doesn't separate the two: with WaitDelay
// set, a leader-only kill still returns — just late, and with
// git-remote-https left alive on the socket, holding the captured
// output pipe it inherited. The EOF read off the proxy connection is
// the discriminator. Measured: drop the group kill and this test fails
// on that read, not on the clock.
func TestCombinedContext_TimeoutReapsTransportChild(t *testing.T) {
	gittest.RequireHTTPSTransport(t)
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "seed")
	proxy, accepted := blackHoleProxy(t)

	const budget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	out, err := CombinedContext(ctx, dir,
		"-c", "http.proxy=http://"+proxy,
		"push", "https://example.invalid/x.git", "HEAD:refs/heads/main")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("CombinedContext against a black hole returned nil error (out=%q)", out)
	}
	// Generous slack: the assertion is "bounded", not "prompt". A
	// leader-only kill blows this by ~2 minutes, not by 2 seconds.
	if elapsed > budget+5*time.Second {
		t.Errorf("returned after %s, want the %s budget plus slack", elapsed, budget)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to unwrap to context.DeadlineExceeded", err)
	}
	if got := err.Error(); !strings.Contains(got, "timed out after 500ms") {
		t.Errorf("error = %q, want it to name the budget it blew", got)
	}

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("git never reached the proxy; the black hole proved nothing")
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("read from the proxy side: %v — want EOF, meaning git-remote-https died with the group", err)
	}
}

// TestCombinedContext_SucceedsUnderALiveContext: the cancellable path
// is the same git, not a special one. A push that beats its deadline
// lands exactly as Combined's would.
func TestCombinedContext_SucceedsUnderALiveContext(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "seed")
	origin := gittest.InitBare(t)
	gittest.Run(t, dir, "remote", "add", "origin", origin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := CombinedContext(ctx, dir, "push", origin, "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("CombinedContext push: %v", err)
	}
	if got, want := gittest.HeadSHA(t, origin), gittest.HeadSHA(t, dir); got != want {
		t.Fatalf("origin main = %s, want %s", got, want)
	}
}

// TestCombinedContext_CancelIsNotReportedAsATimeout: an operator
// stopping serve is not the remote failing. A cancelled context has no
// budget to name, so the error stays context.Canceled rather than
// claiming a deadline nobody set.
func TestCombinedContext_CancelIsNotReportedAsATimeout(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "seed")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CombinedContext(ctx, dir, "status")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want no timeout claim on a plain cancel", err)
	}
}
