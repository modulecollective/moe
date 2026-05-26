package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/serve/pty"
)

// tailBytes is the per-child ring-buffer cap on PTY stdout retained
// for the activity log. Big enough for the chain-prompt context and
// a few claude turns; small enough not to grow without bound.
const tailBytes = 64 * 1024

// child is one PTY-backed moe run the server is parenting.
type child struct {
	id      string // "<project>/<slug>"
	cmd     *exec.Cmd
	pty     *pty.Pty
	started time.Time

	mu   sync.Mutex
	tail []byte // ring-buffer of recent PTY stdout, capped at tailBytes

	done    chan struct{}
	exitErr error
}

// children is the live PTY-child registry, keyed by id.
type children struct {
	mu  sync.RWMutex
	all map[string]*child
}

func newChildren() *children {
	return &children{all: map[string]*child{}}
}

// spawn starts a moe run as a PTY child and records it under id.
// The caller has already validated id and constructed args. Returns
// an error if a live child already holds id.
func (cs *children) spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs.mu.Lock()
	if existing, dup := cs.all[id]; dup {
		// If the prior run already exited, replace it. Otherwise refuse.
		select {
		case <-existing.done:
			delete(cs.all, id)
		default:
			cs.mu.Unlock()
			return nil, fmt.Errorf("serve: run %s already live", id)
		}
	}

	cmd := exec.Command(moeBin, args...)
	cmd.Dir = root
	// Inherit env, then force a recognized TERM so claude/codex
	// render. The PTY is real; we just need to tell the child what
	// to assume about it.
	env := append([]string{}, os.Environ()...)
	env = append(env, "TERM=xterm-256color")
	cmd.Env = env

	p, err := pty.Start(cmd)
	if err != nil {
		cs.mu.Unlock()
		return nil, err
	}

	c := &child{
		id:      id,
		cmd:     cmd,
		pty:     p,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	cs.all[id] = c
	cs.mu.Unlock()

	go c.read(logger)
	return c, nil
}

func (cs *children) get(id string) (*child, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c, ok := cs.all[id]
	return c, ok
}

// shutdown closes every child's PTY (kernel SIGHUPs the chain) and
// waits for the readers to drain. Called from Server.ListenAndServe
// on context cancellation.
func (cs *children) shutdown(ctx context.Context) {
	cs.mu.Lock()
	live := make([]*child, 0, len(cs.all))
	for _, c := range cs.all {
		_ = c.pty.Close()
		live = append(live, c)
	}
	cs.mu.Unlock()

	for _, c := range live {
		select {
		case <-c.done:
		case <-ctx.Done():
			return
		}
	}
}

// read copies master PTY output into the ring buffer until EIO,
// then reaps the child and closes done.
func (c *child) read(logger io.Writer) {
	buf := make([]byte, 4096)
	for {
		n, err := c.pty.File().Read(buf)
		if n > 0 {
			c.appendTail(buf[:n])
		}
		if err != nil {
			break
		}
	}
	c.exitErr = c.cmd.Wait()
	close(c.done)
	if logger != nil {
		fmt.Fprintf(logger, "serve: child %s exited: %v\n", c.id, c.exitErr)
	}
}

func (c *child) appendTail(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tail = append(c.tail, data...)
	if len(c.tail) > tailBytes {
		c.tail = c.tail[len(c.tail)-tailBytes:]
	}
}

// writeKey writes a single byte to the child's stdin via the master.
// Chain-prompt buttons send "Y", "!", "n", etc. one byte at a time.
func (c *child) writeKey(b byte) error {
	_, err := c.pty.File().Write([]byte{b})
	return err
}

// snapshot returns a copy of the current tail plus the child's exit
// state. Safe to call from request handlers; doesn't block the reader.
func (c *child) snapshot() (tail []byte, exited bool, exitErr error) {
	c.mu.Lock()
	tail = append([]byte(nil), c.tail...)
	c.mu.Unlock()
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
	default:
	}
	return
}
