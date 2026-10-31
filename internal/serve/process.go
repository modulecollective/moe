package serve

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Keep only a few screens of output, in memory; tails never enter the state file.
const childTailBytes = 8 << 10

// child carries outcomes only; each host owns its process resources.
type child struct {
	id         string
	started    time.Time
	dispatched bool
	done       chan struct{} // a known exit, available to read-only snapshots
	settled    chan struct{} // callbacks and immediate host retirement have finished
	exitErr    error
	tailMu     sync.Mutex
	tailBuf    []byte
}

// jobHost is private to serve: scheduling never inspects process ownership.
type jobHost interface {
	reconcile() error
	spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error)
	shutdown(context.Context, io.Writer)
}
type children struct {
	mu           sync.RWMutex
	spawnMu      sync.Mutex
	all          map[string]*child
	host         jobHost
	stopping     bool
	notify       func(string, error)
	beforeLaunch func(string)
	onStart      func(*child)
	onComplete   func(*child)
	onSpawn      func(string, time.Time, string)
	onExit       func(string, time.Time, error, string)
}

func newChildren() *children {
	cs := &children{all: map[string]*child{}}
	cs.host = newLocalHost(cs)
	return cs
}
func (c *child) isSettled() bool {
	select {
	case <-c.settled:
		return true
	default:
		return false
	}
}

// register publishes start state synchronously, before an observer can finish.
func (cs *children) register(c *child, detail string) {
	cs.mu.Lock()
	cs.all[c.id] = c
	cs.mu.Unlock()
	if cs.onStart != nil {
		cs.onStart(c)
	}
	if cs.onSpawn != nil {
		cs.onSpawn(c.id, c.started, detail)
	}
}
func (cs *children) spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs.spawnMu.Lock()
	defer cs.spawnMu.Unlock()
	cs.mu.RLock()
	stopping := cs.stopping
	prev := cs.all[id]
	cs.mu.RUnlock()
	if stopping {
		return nil, fmt.Errorf("serve: stopping")
	}
	if prev != nil && !prev.isSettled() {
		return nil, fmt.Errorf("serve: run %s still completing", id)
	}
	return cs.host.spawn(id, moeBin, args, root, logger)
}

// exitNotifier suppresses notifications during operator-requested shutdown.
func (cs *children) exitNotifier() func(id string, exitErr error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.stopping {
		return nil
	}
	return cs.notify
}

func (cs *children) get(id string) (*child, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c, ok := cs.all[id]
	return c, ok
}

// remove drops id from the registry. Idempotent — a no-op when id isn't
// present. Called after a successful sdlc close so a lingering exited
// child stops marking the now-gone run as parented on the dash and the
// per-run page.
func (cs *children) remove(id string) {
	cs.mu.Lock()
	delete(cs.all, id)
	cs.mu.Unlock()
}

// shutdown suppresses notifications before invoking the host's stop policy.
func (cs *children) shutdown(ctx context.Context, logger io.Writer) {
	cs.mu.Lock()
	cs.stopping = true
	cs.mu.Unlock()
	cs.host.shutdown(ctx, logger)
}

func shutLogf(logger io.Writer, format string, a ...any) {
	if logger == nil {
		return
	}
	fmt.Fprintf(logger, format+"\n", a...)
}

// finish is the exit path every host shares: record the status, release
// anyone waiting on done, then the notify and activity hooks. Rex's
// watcher reaches it with an exit status read from the block rather than
// from Wait.
func (c *child) finish(cs *children, exitErr error, logger io.Writer) {
	c.exitErr = exitErr
	close(c.done)
	if logger != nil {
		fmt.Fprintf(logger, "serve: child %s exited: %v\n", c.id, c.exitErr)
	}
	if notify := cs.exitNotifier(); notify != nil {
		notify(c.id, c.exitErr)
	}
	if cs.onExit != nil {
		cs.onExit(c.id, time.Now(), c.exitErr, c.tail())
	}
	if cs.onComplete != nil {
		cs.onComplete(c)
	}
}

// appendTail folds one PTY read into the bounded tail, dropping the
// oldest bytes once it is full.
func (c *child) appendTail(p []byte) {
	c.tailMu.Lock()
	defer c.tailMu.Unlock()
	c.tailBuf = append(c.tailBuf, p...)
	if len(c.tailBuf) > childTailBytes {
		c.tailBuf = c.tailBuf[len(c.tailBuf)-childTailBytes:]
	}
}

// tail returns the kept output as a string. Safe to call while the
// reader is still running.
func (c *child) tail() string {
	c.tailMu.Lock()
	defer c.tailMu.Unlock()
	return string(c.tailBuf)
}

// snapshot reports the child's exit state. Safe to call from request
// handlers without blocking the reader.
func (c *child) snapshot() (exited bool, exitErr error) {
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
	default:
	}
	return
}
