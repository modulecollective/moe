// Package runsocktest is an in-memory stand-in for the Unix socket
// namespace runsock binds in: a listener per path, connections as
// net.Pipe pairs. Agent sandboxes may refuse AF_UNIX sockets outright,
// so tests that exercise an owner's endpoint run over this instead.
//
// Listen still behaves like a filesystem socket where it matters: it
// leaves a placeholder file at the path (what a dead owner leaves
// behind), refuses a path that already has a file, and Close removes
// it. Dialling a path with a file but no listener is refused; a path
// with nothing there does not exist.
package runsocktest

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
)

// Net is one in-memory socket namespace.
type Net struct {
	mu sync.Mutex
	ln map[string]*listener
}

// New returns an empty namespace.
func New() *Net { return &Net{ln: map[string]*listener{}} }

type listener struct {
	net  *Net
	path string
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.net.mu.Lock()
		if l.net.ln[l.path] == l {
			delete(l.net.ln, l.path)
		}
		l.net.mu.Unlock()
		os.Remove(l.path)
	})
	return nil
}

func (l *listener) Addr() net.Addr { return &net.UnixAddr{Name: l.path, Net: "unix"} }

// Listen binds path.
func (n *Net) Listen(path string) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return nil, &net.OpError{Op: "listen", Net: "unix", Err: syscall.EADDRINUSE}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return nil, err
	}
	l := &listener{net: n, path: path, ch: make(chan net.Conn), done: make(chan struct{})}
	n.ln[path] = l
	return l, nil
}

// Dial connects to path's listener.
func (n *Net) Dial(ctx context.Context, path string) (net.Conn, error) {
	n.mu.Lock()
	l := n.ln[path]
	n.mu.Unlock()
	if l == nil {
		if _, err := os.Stat(path); err == nil {
			return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
		}
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: fmt.Errorf("%w", os.ErrNotExist)}
	}
	client, server := net.Pipe()
	select {
	case l.ch <- server:
		return client, nil
	case <-l.done:
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
