package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/serve/pty"
)

// chainPromptRegex matches the moe chain-prompt line and captures the
// option set between the square brackets. Two phrasings to match,
// both ending in `run now? [...]`:
//
//	next: <hint> — run now? [Y/n/x/b/!]   (stage_next.go:359, :529)
//	<stage> sealed — close run now? [Y/n/x] (stage_next.go:444)
//
// We capture just the bracket contents so the button renderer can
// enumerate the keys without re-parsing the legend that follows.
var chainPromptRegex = regexp.MustCompile(`run now\? \[([A-Za-z!/]+)\]`)

// openedRunRegex matches the `opened run <project>/<slug>` stdout
// line `moe sdlc new` prints once the open commit lands
// (internal/cli/new.go:226). The promote flow spawns the child
// under a `:promoting` placeholder id and renames the registry
// entry on first match.
var openedRunRegex = regexp.MustCompile(`opened run ([a-z0-9._-]+)/([a-z0-9-]+)`)

// promotingSuffix tags a child's registry id while we wait for the
// `opened run …` line to land and tell us the real slug. The colon
// is illegal in slugs, so a placeholder id can't collide with a
// real run.
const promotingSuffix = ":promoting"

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

// endAgentEotGap is the pause between the two \x04 (EOT) bytes used to
// politely signal "end the agent". Claude's input loop needs at least
// two EOTs to exit and seems to ignore a back-to-back pair; 100ms is
// enough for the first to be consumed as a discrete event. Codex
// exits on the first and treats the second as a no-op.
const endAgentEotGap = 100 * time.Millisecond

// shutdownSoftGrace is how long the four-phase shutdown waits after
// the soft EOFs for a child to exit naturally. Moe at the chain
// prompt is the common end-state once the agent exits — this is the
// budget for the agent to flush, moe to run session.Close +
// sync.AutoPush, and moe to print the chain prompt.
//
// Variable rather than const so tests can shorten the wait; not
// part of the package's surface and not safe to mutate concurrently
// with a running ListenAndServe.
var shutdownSoftGrace = 10 * time.Second

// shutdownHangupGrace is the second wait after hanging up the PTY
// for stragglers that didn't exit on EOT. Total shutdown budget is
// roughly endAgentEotGap + shutdownSoftGrace + shutdownHangupGrace.
var shutdownHangupGrace = 10 * time.Second

// Prompt is the chain-prompt state currently detected at the tail
// of the child's PTY output. Active is true when a prompt line sits
// in the last `promptWindow` bytes of the ring buffer.
type Prompt struct {
	Options string // single characters from the bracket set, e.g. "Ynxb!"
	Active  bool
}

// child is one PTY-backed moe run the server is parenting.
type child struct {
	cmd     *exec.Cmd
	pty     *pty.Pty
	started time.Time

	mu sync.Mutex
	// id is "<project>/<slug>", possibly with the `:promoting`
	// placeholder suffix while a promote spawn waits for its
	// `opened run …` line. Mutated by children.rename under
	// cs.mu + c.mu so the watcher and the snapshot path see a
	// consistent value.
	id            string
	tail          []byte // ring-buffer of recent PTY stdout, capped at tailBytes
	renamePending bool   // true while a promote spawn is still under :promoting

	done     chan struct{}
	exitErr  error
	exitedAt time.Time // set before close(done); only read after <-done
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
		id:            id,
		cmd:           cmd,
		pty:           p,
		started:       time.Now(),
		done:          make(chan struct{}),
		renamePending: strings.HasSuffix(id, promotingSuffix),
	}
	notify := cs.notify
	cs.all[id] = c
	cs.mu.Unlock()

	go c.read(cs, logger, notify)
	return c, nil
}

// rename swaps c's registry key from old to new under cs.mu. Refuses
// if new is already live (a second promote of the same idea, or a
// rename racing against an unrelated spawn — both cases should land
// loudly). The child's own id field is updated so log lines and
// notifier payloads carry the post-rename value.
//
// Used by the promote flow: a child spawned under `<p>/<s>:promoting`
// gets its slug from the `opened run …` line `moe sdlc new` prints
// after the open commit lands.
func (cs *children) rename(old, new string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.all[old]
	if !ok {
		return fmt.Errorf("serve: rename %s: not in registry", old)
	}
	if existing, dup := cs.all[new]; dup {
		select {
		case <-existing.done:
			delete(cs.all, new)
		default:
			return fmt.Errorf("serve: rename %s -> %s: target already live", old, new)
		}
	}
	delete(cs.all, old)
	cs.all[new] = c
	c.mu.Lock()
	c.id = new
	c.renamePending = false
	c.mu.Unlock()
	return nil
}

func (cs *children) get(id string) (*child, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c, ok := cs.all[id]
	return c, ok
}

// shutdown winds children down in four phases so the agent (and
// moe-the-parent of the agent) gets a real chance to commit, push,
// and exit cleanly before the kernel reaps anything:
//
//  1. Send \x04\x04 to every live child (writeRaw — soft EOFs to
//     the agent process, EOF on moe's stdinSharedReader).
//  2. Wait up to shutdownSoftGrace for natural exit. The common
//     end-state: agent flushes and exits → moe runs session.Close
//     + AutoPush → moe prints the chain prompt → moe's
//     readLineWithSignal sees EOF and returns → moe exits.
//  3. For stragglers, pty.Close() → kernel SIGHUP via the
//     controlling terminal. Same blunt instrument as the old
//     behavior, just gated on the agent having declined to leave
//     politely.
//  4. Wait up to shutdownHangupGrace for the hung-up stragglers to
//     drain. Anything still alive after that is left for the
//     kernel to reap on os.Exit; logged so the operator knows.
//
// logger is the serve logger (the per-server io.Writer); nil means
// quiet shutdown. ctx caps the whole operation — useful if the
// operator hits Ctrl-C twice (the second SIGINT collapses through
// the Go runtime's default handler).
func (cs *children) shutdown(ctx context.Context, logger io.Writer) {
	cs.mu.Lock()
	live := make([]*child, 0, len(cs.all))
	for _, c := range cs.all {
		select {
		case <-c.done:
			// Already exited; nothing to do.
		default:
			live = append(live, c)
		}
	}
	cs.mu.Unlock()

	if len(live) == 0 {
		return
	}
	shutLogf(logger, "shutdown: sending EOF to %d children", len(live))

	// Phase 1: two soft EOFs to every live child.
	for _, c := range live {
		_ = c.writeRaw([]byte{0x04})
	}
	select {
	case <-time.After(endAgentEotGap):
	case <-ctx.Done():
		return
	}
	for _, c := range live {
		_ = c.writeRaw([]byte{0x04})
	}

	// Phase 2: wait for natural exit.
	stillLive := waitForExit(live, shutdownSoftGrace, ctx)
	if len(stillLive) == 0 {
		shutLogf(logger, "shutdown: %d/%d children exited cleanly", len(live), len(live))
		return
	}
	shutLogf(logger, "shutdown: %d/%d still live after grace, hanging up PTY", len(stillLive), len(live))

	// Phase 3: hang up the master fd for stragglers.
	for _, c := range stillLive {
		_ = c.pty.Close()
	}

	// Phase 4: bounded wait for the hung-up stragglers.
	final := waitForExit(stillLive, shutdownHangupGrace, ctx)
	if len(final) > 0 {
		shutLogf(logger, "shutdown: %d still live after hangup, leaving for kernel reap", len(final))
	}
}

// waitForExit waits up to grace (or ctx cancellation) for every
// child in cs to close c.done. Returns the children that didn't
// exit in time.
func waitForExit(cs []*child, grace time.Duration, ctx context.Context) []*child {
	deadline := time.Now().Add(grace)
	var stillLive []*child
	for _, c := range cs {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		timer := time.NewTimer(remaining)
		select {
		case <-c.done:
			timer.Stop()
		case <-timer.C:
			stillLive = append(stillLive, c)
		case <-ctx.Done():
			timer.Stop()
			// Treat remaining children as still-live; caller
			// decides what to do.
			stillLive = append(stillLive, c)
		}
	}
	return stillLive
}

func shutLogf(logger io.Writer, format string, a ...any) {
	if logger == nil {
		return
	}
	fmt.Fprintf(logger, format+"\n", a...)
}

// read copies master PTY output into the ring buffer until EIO,
// then reaps the child and closes done. Calls notify (if non-nil)
// once the exit status is known. cs is held so the promote-rename
// watcher can mutate the registry when the `opened run …` line
// shows up; nil is permitted for tests that drive a child directly.
func (c *child) read(cs *children, logger io.Writer, notify func(string, error)) {
	buf := make([]byte, 4096)
	for {
		n, err := c.pty.File().Read(buf)
		if n > 0 {
			c.appendTail(buf[:n])
			if cs != nil {
				c.maybeRename(cs, logger)
			}
		}
		if err != nil {
			break
		}
	}
	c.exitErr = c.cmd.Wait()
	c.exitedAt = time.Now()
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

// maybeRename greps the tail for the `opened run …` line and, on
// first match, atomically renames the child's registry id from its
// `:promoting` placeholder to `<project>/<slug>`. No-op once the
// rename has happened (renamePending stays false thereafter), and
// no-op for any child not spawned under a `:promoting` id.
func (c *child) maybeRename(cs *children, logger io.Writer) {
	c.mu.Lock()
	if !c.renamePending {
		c.mu.Unlock()
		return
	}
	tail := c.tail
	oldID := c.id
	c.mu.Unlock()
	m := openedRunRegex.FindSubmatch(tail)
	if m == nil {
		return
	}
	newID := string(m[1]) + "/" + string(m[2])
	if err := cs.rename(oldID, newID); err != nil {
		if logger != nil {
			fmt.Fprintf(logger, "serve: rename %s -> %s: %v\n", oldID, newID, err)
		}
		// Clear the pending flag so we don't churn on every read —
		// the placeholder id stays in the registry, the operator
		// sees the failure via the child's own error tail.
		c.mu.Lock()
		c.renamePending = false
		c.mu.Unlock()
		return
	}
	if logger != nil {
		fmt.Fprintf(logger, "serve: promoted child %s -> %s\n", oldID, newID)
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

// writeRaw writes b verbatim to the child's PTY — no newline, no
// other framing. Used for control characters (Ctrl-D / EOT) that
// would be corrupted by writeKeys' line-buffered append.
func (c *child) writeRaw(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := c.pty.File().Write(b)
	return err
}

// snapshot returns a copy of the current tail, the active chain-prompt
// (if any), and the child's exit state. Safe to call from request
// handlers; doesn't block the reader. exitedAt is the zero value
// until exited == true.
func (c *child) snapshot() (tail []byte, prompt Prompt, exited bool, exitErr error, exitedAt time.Time) {
	c.mu.Lock()
	tail = append([]byte(nil), c.tail...)
	c.mu.Unlock()
	prompt = detectPrompt(tail)
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
		exitedAt = c.exitedAt
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
