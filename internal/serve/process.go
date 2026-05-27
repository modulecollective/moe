package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/serve/pty"
)

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

// tailBytes is the per-child ring-buffer cap on PTY stdout retained
// for the `opened run …` rename watcher. The line lands within the
// first few KiB of moe's startup output, so the buffer just needs to
// be big enough to survive normal terminal noise (banner, prompts)
// before the watcher matches.
const tailBytes = 8 * 1024

// shutdownIntrGap is the pause between the two \x03 (Ctrl-C) bytes
// written to each child's PTY during shutdown. Two Ctrl-Cs is the
// same byte path a human at the terminal would take: in raw mode
// claude treats them as two interrupts and exits cleanly; in cooked
// mode (moe at a chain prompt) the kernel converts each to SIGINT
// on the foreground process group, which `readLineWithSignal` turns
// into a clean exit. 100ms is enough for the first byte to be
// consumed as a discrete event before the second arrives.
const shutdownIntrGap = 100 * time.Millisecond

// shutdownSoftGrace is how long the four-phase shutdown waits after
// the two Ctrl-Cs for a child to exit naturally. Moe at the chain
// prompt is the common end-state once the agent exits — this is the
// budget for the agent to flush, moe to run session.Close +
// sync.AutoPush, and moe to print the chain prompt.
//
// Variable rather than const so tests can shorten the wait; not
// part of the package's surface and not safe to mutate concurrently
// with a running ListenAndServe.
var shutdownSoftGrace = 10 * time.Second

// shutdownHangupGrace is the second wait after hanging up the PTY
// for stragglers that didn't exit on Ctrl-C. Total shutdown budget
// is roughly shutdownIntrGap + shutdownSoftGrace + shutdownHangupGrace.
var shutdownHangupGrace = 10 * time.Second

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

	go c.read(cs, root, logger, notify)
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
//  1. Write \x03\x03 (two Ctrl-Cs, ~100ms apart) to every live
//     child's PTY master. The tty mode does the routing: raw mode
//     (claude active) sees two interrupts and exits; cooked mode
//     (moe at a chain prompt) converts each to SIGINT on the fg
//     pgrp and `readLineWithSignal` returns interrupted → moe
//     exits 0. In the transition gap the byte sits in the tty input
//     buffer until something reads it.
//  2. Wait up to shutdownSoftGrace for natural exit. The common
//     end-state: agent flushes and exits → moe runs session.Close
//     + AutoPush → moe prints the chain prompt → the second \x03
//     (now in cooked mode) interrupts the prompt → moe exits.
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
	shutLogf(logger, "shutdown: sending Ctrl-C to %d children", len(live))

	// Phase 1: two Ctrl-Cs to every live child.
	for _, c := range live {
		_ = c.writeRaw([]byte{0x03})
	}
	select {
	case <-time.After(shutdownIntrGap):
	case <-ctx.Done():
		return
	}
	for _, c := range live {
		_ = c.writeRaw([]byte{0x03})
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
//
// root is the bureaucracy root, used by the on-exit transcript
// snag to find both source (~/.claude/projects/<encoded
// worktree>/) and destination (projects/<p>/runs/<s>/documents/
// design/transcripts/). Empty root skips snagging.
func (c *child) read(cs *children, root string, logger io.Writer, notify func(string, error)) {
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
	if root != "" {
		c.snagTranscripts(root, logger)
	}
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

// writeRaw writes b verbatim to the child's PTY — no newline, no
// other framing. Used for control characters (Ctrl-C / 0x03) sent
// during shutdown.
func (c *child) writeRaw(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := c.pty.File().Write(b)
	return err
}

// snapshot returns a copy of the current tail and the child's exit
// state. Safe to call from request handlers; doesn't block the
// reader. exitedAt is the zero value until exited == true.
func (c *child) snapshot() (tail []byte, exited bool, exitErr error, exitedAt time.Time) {
	c.mu.Lock()
	tail = append([]byte(nil), c.tail...)
	c.mu.Unlock()
	select {
	case <-c.done:
		exited = true
		exitErr = c.exitErr
		exitedAt = c.exitedAt
	default:
	}
	return
}

// claudeProjectsDir returns ~/.claude/projects/<encoded-cwd> per
// claude code's per-session project-directory encoding: replace `/`
// with `-`, replace `.` with `-`, ensure a leading `-`. Returns the
// empty string if $HOME isn't resolvable.
//
//	/home/dev/work/bureaucracy
//	  → ~/.claude/projects/-home-dev-work-bureaucracy
//	/home/dev/work/bureaucracy/.moe/worktrees/<uuid>
//	  → ~/.claude/projects/-home-dev-work-bureaucracy--moe-worktrees-<uuid>
func claudeProjectsDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	enc := strings.ReplaceAll(cwd, "/", "-")
	enc = strings.ReplaceAll(enc, ".", "-")
	if !strings.HasPrefix(enc, "-") {
		enc = "-" + enc
	}
	return filepath.Join(home, ".claude", "projects", enc)
}

// snagTranscripts copies any claude-code per-session JSONL written
// during this child's lifetime into the run's
// `documents/design/transcripts/` tree, so the transcript survives
// worktree cleanup and is reachable when the operator resumes from
// another machine.
//
// Source: every `~/.claude/projects/<encoded>/*.jsonl` whose
// directory name has the encoded `<root>/.moe/worktrees/` prefix
// (the encoding for the per-session worktrees moe opens under this
// bureaucracy) and whose mtime is ≥ child.started. Copy, not move —
// the original is harmless and any concurrent reader sees no
// surprise.
//
// Destination: `<root>/projects/<p>/runs/<s>/documents/design/
// transcripts/<session-uuid>.jsonl` (the JSONL filename is
// preserved so the file stays drop-in compatible with anything that
// reads claude code's session format).
//
// Skips silently when the id still carries the `:promoting`
// placeholder (no destination run dir exists) or when reading
// `~/.claude/projects/` fails (no claude code on this host, or no
// sessions yet — both fine).
func (c *child) snagTranscripts(root string, logger io.Writer) {
	c.mu.Lock()
	id := c.id
	started := c.started
	c.mu.Unlock()
	if strings.HasSuffix(id, promotingSuffix) {
		return
	}
	project, slug, ok := strings.Cut(id, "/")
	if !ok {
		return
	}

	projectsRoot := claudeProjectsDir(filepath.Join(root, ".moe", "worktrees"))
	if projectsRoot == "" {
		return
	}
	// projectsRoot is .../projects/<encoded-worktrees-dir>; we want
	// the *parent* (.../projects) to list, and the basename as the
	// per-worktree prefix (encoded basename + `-` for the separator
	// before the UUID).
	parent := filepath.Dir(projectsRoot)
	prefix := filepath.Base(projectsRoot) + "-"

	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	destDir := filepath.Join(root, "projects", project, "runs", slug,
		"documents", "design", "transcripts")

	var copied int
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		srcDir := filepath.Join(parent, entry.Name())
		files, err := os.ReadDir(srcDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil || info.ModTime().Before(started) {
				continue
			}
			if err := copyFile(filepath.Join(srcDir, f.Name()),
				filepath.Join(destDir, f.Name())); err != nil {
				if logger != nil {
					fmt.Fprintf(logger, "serve: snag %s/%s: %v\n",
						entry.Name(), f.Name(), err)
				}
				continue
			}
			copied++
		}
	}
	if copied > 0 && logger != nil {
		fmt.Fprintf(logger, "serve: snagged %d transcript(s) for %s\n", copied, id)
	}
}

// copyFile copies src to dst, creating dst's parent dir as needed.
// Overwrites dst if it already exists (a re-run of the same session
// snags the same file; last-write-wins is fine because the source
// is append-only).
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
