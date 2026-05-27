package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/serve/pty"
)

// chainPromptRegex matches the moe chain-prompt line and captures the
// option set between the square brackets. Format printed at
// internal/cli/stage_next.go:359 is:
//
//	next: <hint> — run now? [Y/n/x/b/!]
//
// We capture just the bracket contents so the button renderer can
// enumerate the keys without re-parsing the legend that follows.
var chainPromptRegex = regexp.MustCompile(`— run now\? \[([A-Za-z!/]+)\]`)

// promptWindow is the byte-window at the tail of the ring buffer
// where a matching prompt is treated as "active". A prompt detected
// deeper in history is stale — moe printed the prompt earlier, the
// operator answered, and progress output has since pushed the
// prompt out of the window.
const promptWindow = 1024

// tailBytes is the per-child ring-buffer cap on PTY stdout retained
// for the activity log. Big enough for the chain-prompt context and
// a few claude turns; small enough not to grow without bound.
const tailBytes = 64 * 1024

// Prompt is the chain-prompt state currently detected at the tail
// of the child's PTY output. Active is true when a prompt line sits
// in the last `promptWindow` bytes of the ring buffer.
type Prompt struct {
	Options string // single characters from the bracket set, e.g. "Ynxb!"
	Active  bool
}

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
	// notify fires once per child on natural exit (not on server
	// shutdown). Empty by default; Server.New wires it when
	// Options.NotifyURL is set.
	notify func(id string, exitErr error)
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
	notify := cs.notify
	cs.all[id] = c
	cs.mu.Unlock()

	go c.read(logger, notify)
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
// then reaps the child and closes done. Calls notify (if non-nil)
// once the exit status is known.
func (c *child) read(logger io.Writer, notify func(string, error)) {
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
	if notify != nil {
		notify(c.id, c.exitErr)
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

// writeKeys writes the given string (typically one or two bytes —
// "Y", "!", "!!") followed by a newline so moe's line-buffered
// readLine consumes it. Returns the underlying Write error.
func (c *child) writeKeys(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("serve: empty key")
	}
	_, err := c.pty.File().Write([]byte(s + "\n"))
	return err
}

// snapshot returns a copy of the current tail, the active chain-prompt
// (if any), and the child's exit state. Safe to call from request
// handlers; doesn't block the reader.
func (c *child) snapshot() (tail []byte, prompt Prompt, exited bool, exitErr error) {
	c.mu.Lock()
	tail = append([]byte(nil), c.tail...)
	c.mu.Unlock()
	prompt = detectPrompt(tail)
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
	default:
	}
	return
}

// detectPrompt scans the tail for a chain-prompt match. Returns
// Active=false unless the match sits in the last promptWindow bytes
// of tail — a deeper match is stale (the operator already answered
// and moe has since printed past it).
func detectPrompt(tail []byte) Prompt {
	if len(tail) == 0 {
		return Prompt{}
	}
	matches := chainPromptRegex.FindAllSubmatchIndex(tail, -1)
	if len(matches) == 0 {
		return Prompt{}
	}
	// FindAll returns matches in order; take the last (most recent).
	last := matches[len(matches)-1]
	matchEnd := last[1]
	if len(tail)-matchEnd > promptWindow {
		// Prompt printed too far back to still be live.
		return Prompt{}
	}
	// Group 1 holds the bracket contents, e.g. "Y/n/x/b/!". Strip
	// slashes; what remains is the option-key alphabet.
	raw := string(tail[last[2]:last[3]])
	var keys []byte
	for i := 0; i < len(raw); i++ {
		if raw[i] != '/' {
			keys = append(keys, raw[i])
		}
	}
	return Prompt{Options: string(keys), Active: true}
}
