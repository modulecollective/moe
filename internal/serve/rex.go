package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Rex as the host for serve's background jobs.
//
// When a local Rex server answers at startup, each heartbeat sweep runs
// as a terminal block in one Rex session labelled "moe" instead of on a
// PTY serve owns. Rex then owns the screen and the process lifetime: the
// operator can open the block, watch it, Ctrl-C it, and the job outlives
// a serve restart or crash. serve keeps the registry half of the child —
// id, start, exit, tail — and drives the installed `rex` CLI with --json
// to fill it in. No wire protocol, no SDK.
//
// The block label is the child id ("heartbeat:<project>"), and that is
// the whole of the persistence: a restarted serve lists the session's
// blocks and adopts the labelled ones (adoptRex). The only file is
// .moe/rex-session, which names the session.

// rexBin is the Rex CLI serve looks for on PATH. A var so the package's
// tests can point discovery at a name that never resolves — an armed
// test serve on a box with a real Rex server would otherwise launch real
// blocks.
var rexBin = "rex"

// rexTimeout bounds every non-streaming Rex request. `rex wait` streams,
// and the flag does not limit its event delivery.
const rexTimeout = "10s"

// rexMinAPIRevision is the oldest Rex API whose JSON shapes this file
// was written against (0.1.0-preview.12 reports 8).
const rexMinAPIRevision = 8

// rexSessionLabel is the one Rex session every serve job lives in.
const rexSessionLabel = "moe"

// rexRetryDelay paces the watcher when Rex can't say whether a block's
// program has exited. Variable so tests can shorten it.
var rexRetryDelay = 5 * time.Second

// rexHost is the Rex half of the child registry: the CLI it drives and
// the session its blocks live in.
type rexHost struct {
	bin    string
	root   string
	logger io.Writer

	// ctx is cancelled by shutdown. It stops local observation, including
	// the watchers' `rex wait` processes: the blocks keep running.
	ctx    context.Context
	cancel context.CancelFunc

	// workers covers watchers through their final RPC and heartbeat completion.
	// Wait only after all launch/adoption calls have returned. Production
	// shutdown cancels without joining: post-exit RPCs can outlast its budget.
	workers sync.WaitGroup

	mu      sync.Mutex
	session string // Rex session id; "" until the first launch creates one
}

func newRexHost(bin, root string, logger io.Writer) *rexHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &rexHost{bin: bin, root: root, logger: logger, ctx: ctx, cancel: cancel}
}

// rexExit is a Rex-hosted program's non-zero exit, spelled the way
// exec.ExitError spells a local one so logs and /serve read the same.
type rexExit int

func (e rexExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// errRexBlockGone is a block that vanished before reporting an exit —
// closed by hand, or lost with its server.
var errRexBlockGone = errors.New("rex block closed before its program reported an exit")

// discoverRex picks the host for this process lifetime. (nil, nil) means
// no Rex here — not on PATH, or no server answering — and selects the
// local PTY host. An error means a Rex server answered but this code
// can't drive it; the caller must not fall back to local, because a
// previous serve may have left live Rex jobs a local launch would
// duplicate.
func discoverRex(root string, logger io.Writer) (*rexHost, error) {
	bin, err := exec.LookPath(rexBin)
	if err != nil {
		return nil, nil
	}
	probe := &rexHost{bin: bin}
	out, err := probe.call(context.Background(), "whoami", "--json")
	if err != nil {
		return nil, nil
	}
	var who struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(out, &who); err != nil || who.InstanceID == "" {
		return nil, fmt.Errorf("rex whoami: unrecognised reply %q", firstLine(string(out)))
	}
	// The server's whoami carries no API revision; the CLI's version
	// does, and the CLI's JSON is the contract this file reads.
	out, err = probe.call(context.Background(), "version", "--json")
	if err != nil {
		return nil, fmt.Errorf("rex version: %w", err)
	}
	var ver struct {
		APIRevision *int `json:"api_revision"`
	}
	if err := json.Unmarshal(out, &ver); err != nil || ver.APIRevision == nil {
		return nil, fmt.Errorf("rex version: unrecognised reply %q", firstLine(string(out)))
	}
	if *ver.APIRevision < rexMinAPIRevision {
		return nil, fmt.Errorf("rex API revision %d is older than %d", *ver.APIRevision, rexMinAPIRevision)
	}
	return newRexHost(bin, root, logger), nil
}

// call runs one rex command and returns its stdout. Every call passes
// --autostart=false: serve must never start a Rex server of its own.
func (h *rexHost) call(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"--autostart=false", "--timeout", rexTimeout}, args...)
	cmd := exec.CommandContext(ctx, h.bin, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("rex %s: %w: %s", args[0], err, firstLine(msg))
		}
		return stdout.Bytes(), fmt.Errorf("rex %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

func (h *rexHost) logf(format string, a ...any) {
	if h.logger != nil {
		fmt.Fprintf(h.logger, "serve: "+format+"\n", a...)
	}
}

// rexSessionPath is where serve remembers its Rex session. Under .moe/,
// which carries a `*` gitignore.
func rexSessionPath(root string) string {
	return filepath.Join(root, ".moe", "rex-session")
}

func (h *rexHost) sessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session
}

// sessionPresent requires a valid listing: loss of contact never proves
// that a session (and the jobs it owns) can safely be replaced.
func (h *rexHost) sessionPresent(session string) (bool, error) {
	out, err := h.call(context.Background(), "ls", "--json")
	if err != nil {
		return false, err
	}
	var ls struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &ls); err != nil || ls.Sessions == nil {
		return false, fmt.Errorf("rex ls: unrecognised reply %q", firstLine(string(out)))
	}
	present := false
	for _, row := range ls.Sessions {
		if row.ID == "" {
			return false, errors.New("rex ls: session without an ID")
		}
		present = present || row.ID == session
	}
	return present, nil
}

// rexBlock is one row of `rex block ls --json`.
type rexBlock struct {
	ID    string `json:"block_id"`
	Label string `json:"label"`
}

// blocks lists the session's blocks.
func (h *rexHost) blocks(session string) ([]rexBlock, error) {
	out, err := h.call(context.Background(), "block", "ls", "--json", "-s", session)
	if err != nil {
		return nil, err
	}
	var ls struct {
		Blocks []rexBlock `json:"blocks"`
	}
	if err := json.Unmarshal(out, &ls); err != nil {
		return nil, fmt.Errorf("rex block ls: %w", err)
	}
	if ls.Blocks == nil {
		return nil, errors.New("rex block ls: missing or null blocks array")
	}
	for _, b := range ls.Blocks {
		if b.ID == "" {
			return nil, errors.New("rex block ls: block without an ID")
		}
	}
	return ls.Blocks, nil
}

// findLabel returns the id of the session's block labelled label, or ""
// when there is none (or no session to look in).
func (h *rexHost) findLabel(label string) (string, error) {
	session := h.sessionID()
	if session == "" {
		return "", nil
	}
	bs, err := h.blocks(session)
	if err != nil {
		return "", err
	}
	id := ""
	for _, b := range bs {
		if b.Label == label {
			id = b.ID
		}
	}
	return id, nil
}

// launch starts argv in a new block labelled label and returns its id.
// The first launch with no session creates it — at startup that is the
// dash (openDash), and a heartbeat only when the dash failed to open.
// The first window takes the label; `rex new` takes none for its block,
// so the block is renamed afterwards. A failed rename leaves the job
// running, unlabelled, and is only logged — the cost is that a restart
// can't adopt it.
func (h *rexHost) launch(label string, argv []string) (string, error) {
	session := h.sessionID()
	if session != "" {
		args := append([]string{"run", "--json", "--focus=false", "--keep-open",
			"-s", session, "--label", label, "--cwd", h.root, "--"}, argv...)
		out, err := h.call(context.Background(), args...)
		if err == nil {
			var ran struct {
				BlockIDs []string `json:"block_ids"`
			}
			if err := json.Unmarshal(out, &ran); err != nil || len(ran.BlockIDs) == 0 {
				return "", fmt.Errorf("rex run: unrecognised reply %q", firstLine(string(out)))
			}
			return ran.BlockIDs[0], nil
		}
		present, probeErr := h.sessionPresent(session)
		if probeErr != nil {
			return "", fmt.Errorf("%w (check session: %v)", err, probeErr)
		}
		if present {
			return "", err
		}
		h.logf("rex: session %s is gone — creating a replacement", session)
		// Keep the old identity until creation succeeds. If new fails,
		// other retained blocks and watchers still need it to confirm
		// absence, so any eligible project can reach the next launch.
	}
	args := append([]string{"new", rexSessionLabel, "--json", "--keep-open", "--cwd", h.root,
		"--window", label, "--"}, argv...)
	out, err := h.call(context.Background(), args...)
	if err != nil {
		return "", err
	}
	var created struct {
		SessionID string `json:"session_id"`
		Windows   []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.SessionID == "" ||
		len(created.Windows) == 0 || len(created.Windows[0].BlockIDs) == 0 {
		return "", fmt.Errorf("rex new: unrecognised reply %q", firstLine(string(out)))
	}
	block := created.Windows[0].BlockIDs[0]
	h.mu.Lock()
	h.session = created.SessionID
	h.mu.Unlock()
	err = os.MkdirAll(filepath.Dir(rexSessionPath(h.root)), 0o755)
	if err == nil {
		err = os.WriteFile(rexSessionPath(h.root), []byte(created.SessionID+"\n"), 0o644)
	}
	if err != nil {
		h.logf("rex: record session %s: %v", created.SessionID, err)
	}
	if _, err := h.call(context.Background(), "block", "rename", "-s", created.SessionID, block, label); err != nil {
		h.logf("rex: label block %s as %s: %v", block, label, err)
	}
	return block, nil
}

// waitExit blocks until the block's program exits and returns its exit
// code, or returns ctx's error once shutdown cancels the watch.
//
// `rex wait --json` writes an exit payload and mirrors its code in its
// own status, so the payload is what's read. No payload is a transport
// failure, not an exit: the block's process record is read instead
// (last_exit is null while the program runs), and a block that has gone
// from the session entirely is errRexBlockGone.
func (h *rexHost) waitExit(block string) (int, error) {
	for {
		out, _ := h.call(h.ctx, "wait", "--json", block)
		if h.ctx.Err() != nil {
			return 0, h.ctx.Err()
		}
		var payload struct {
			ExitCode *int `json:"exit_code"`
		}
		if json.Unmarshal(firstJSONLine(out), &payload) == nil && payload.ExitCode != nil {
			return *payload.ExitCode, nil
		}
		code, exited, err := h.lastExit(block)
		switch {
		case err == nil && exited:
			return code, nil
		case err == nil:
			// Still running; the wait itself dropped. Wait again.
		default:
			if id, lerr := h.findBlock(block); lerr == nil && id == "" {
				return 0, errRexBlockGone
			}
			h.logf("rex: can't read block %s: %v — retrying", block, err)
		}
		select {
		case <-h.ctx.Done():
			return 0, h.ctx.Err()
		case <-time.After(rexRetryDelay):
		}
	}
}

// lastExit reads the block's process record.
func (h *rexHost) lastExit(block string) (code int, exited bool, err error) {
	out, err := h.call(h.ctx, "block", "call", "-b", block, "process")
	if err != nil {
		return 0, false, err
	}
	var proc struct {
		LastExit *struct {
			ExitCode int `json:"exit_code"`
		} `json:"last_exit"`
	}
	if err := json.Unmarshal(out, &proc); err != nil {
		return 0, false, fmt.Errorf("rex block call process: %w", err)
	}
	if proc.LastExit == nil {
		return 0, false, nil
	}
	return proc.LastExit.ExitCode, true, nil
}

// findBlock reports whether block is still in the session: its id, or
// "" when it is gone.
func (h *rexHost) findBlock(block string) (string, error) {
	session := h.sessionID()
	if session == "" {
		return "", errors.New("no rex session")
	}
	bs, err := h.blocks(session)
	if err != nil {
		if present, probeErr := h.sessionPresent(session); probeErr == nil && !present {
			return "", nil
		}
		return "", err
	}
	for _, b := range bs {
		if b.ID == block {
			return b.ID, nil
		}
	}
	return "", nil
}

func (h *rexHost) capture(block string) (string, error) {
	out, err := h.call(context.Background(), "capture", "-b", block, "--trim")
	return string(out), err
}

func (h *rexHost) closeBlock(block string) error {
	_, err := h.call(context.Background(), "block", "close", block)
	return err
}

// firstJSONLine returns the first line of out: `rex wait --json` writes
// its payload as one line, and an error message may follow it.
func firstJSONLine(out []byte) []byte {
	line, _, _ := bytes.Cut(out, []byte("\n"))
	return line
}

// rexJobArgv is the argv a Rex block runs for moe: absolute binaries, no
// shell text. The block starts in the account's login environment, which
// is what serve itself runs in; the variables serve adds ride an env
// prefix. TERM is Rex's own.
func rexJobArgv(moeBin string, env, args []string) ([]string, error) {
	bin, err := exec.LookPath(moeBin)
	if err != nil {
		return nil, err
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	argv := append([]string{"/usr/bin/env"}, env...)
	argv = append(argv, bin)
	return append(argv, args...), nil
}

// spawnRex launches a job as a Rex block and registers it under id.
//
// Retention happens here: a previous run of this id that failed kept its
// block so the operator could read the screen, and this launch is when
// it goes. If that close fails the launch is skipped — the next tick
// tries again — so a stale block never shares a label with a live one.
//
// A launch that errors is unresolved, not failed: Rex may have created
// the block before the reply was lost. The session is searched for the
// label, and a match is this launch. No match means nothing is launched
// this tick, and never locally.
//
// Not safe for concurrent calls on one id. The heartbeat ticker is the
// only spawner and runs one goroutine.
func (cs *children) spawnRex(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs.mu.Lock()
	prev, had := cs.all[id]
	if had {
		select {
		case <-prev.done:
		default:
			cs.mu.Unlock()
			return nil, fmt.Errorf("serve: run %s already live", id)
		}
	}
	cs.mu.Unlock()

	if had && prev.block != "" && prev.exitErr != nil {
		// A kept block the operator closed by hand, or that went with
		// its server, fails the close with nothing left to close. Only a
		// block still in the session holds the launch back.
		if err := cs.rex.closeBlock(prev.block); err != nil {
			if id, ferr := cs.rex.findBlock(prev.block); ferr != nil || id != "" {
				return nil, fmt.Errorf("close kept rex block %s: %w", prev.block, err)
			}
		}
		cs.mu.Lock()
		prev.block = ""
		cs.mu.Unlock()
	}

	// The same two variables serve adds for a local child.
	argv, err := rexJobArgv(moeBin, []string{"MOE_HOME=" + root, "MOE_SERVE_AGENT=1"}, args)
	if err != nil {
		return nil, err
	}
	block, err := cs.rex.launch(id, argv)
	if err != nil {
		found, ferr := cs.rex.findLabel(id)
		if ferr != nil || found == "" {
			return nil, fmt.Errorf("rex launch unresolved, nothing launched: %w", err)
		}
		cs.rex.logf("rex: launch of %s reported %v, but block %s carries its label — taking it", id, err, found)
		block = found
	}
	return cs.registerRex(id, block, "started", logger), nil
}

// registerRex records a Rex-hosted child and starts its watcher. how is
// the activity ring's word for it: "started" or "adopted".
func (cs *children) registerRex(id, block, how string, logger io.Writer) *child {
	c := &child{id: id, block: block, started: time.Now(), done: make(chan struct{})}
	cs.mu.Lock()
	onSpawn, onExit := cs.onSpawn, cs.onExit
	cs.all[id] = c
	cs.mu.Unlock()
	if onSpawn != nil {
		onSpawn(id, c.started, how+" in rex block "+block)
	}
	cs.rex.workers.Go(func() { cs.watchRex(c, logger, onExit) })
	return c
}

// watchRex is the Rex host's reader: wait for the exit, keep the same
// bounded tail the local host keeps, then the shared exit path. A clean
// result is consumed by closing its block; a failed one keeps it for
// the operator until the next launch of the same id.
//
// A shutdown returns without finishing: the job is Rex's, and a
// restarted serve adopts it.
func (cs *children) watchRex(c *child, logger io.Writer, onExit func(string, time.Time, error, string)) {
	code, err := cs.rex.waitExit(c.block)
	if errors.Is(err, context.Canceled) {
		return
	}
	var exitErr error
	switch {
	case err != nil:
		exitErr = err
	case code != 0:
		exitErr = rexExit(code)
	}
	if err == nil {
		// A failed capture costs the tail, never the exit.
		if screen, cerr := cs.rex.capture(c.block); cerr != nil {
			cs.rex.logf("rex: capture block %s: %v", c.block, cerr)
		} else {
			c.appendTail([]byte(screen))
		}
	}
	c.finish(cs, exitErr, logger, onExit)
	if exitErr == nil {
		if err := cs.rex.closeBlock(c.block); err != nil {
			cs.rex.logf("rex: close block %s: %v", c.block, err)
		}
	}
}

// selectHost picks where this armed serve runs jobs, and adopts what a
// previous serve left running in Rex. Called once, before the first
// tick; no flag and no config key — a Rex server that answers is used.
func (s *Server) selectHost() {
	h, err := discoverRex(s.opts.Root, s.opts.Logger)
	switch {
	case err != nil:
		s.hostErr = err
		s.activity.setHost("rex unusable", err.Error())
		s.logf("host: a Rex server answered but can't be driven (%v) — the heartbeat will dispatch nothing until this is fixed and serve restarts", err)
	case h == nil:
		s.activity.setHost("local pty", "")
		s.logf("host: local pty — sweeps stop when serve stops")
	default:
		s.children.rex = h
		s.activity.setHost("rex", "")
		s.logf("host: rex session %q — sweeps run as Rex blocks and finish even if serve stops", rexSessionLabel)
		// Load identity once. Adoption retries use the host's current ID,
		// which launch may replace only after proving the old session gone.
		if body, err := os.ReadFile(rexSessionPath(s.opts.Root)); err == nil {
			h.mu.Lock()
			h.session = strings.TrimSpace(string(body))
			h.mu.Unlock()
		}
		s.rexAdoptErr = s.adoptRex(h)
		if s.rexAdoptErr != nil {
			s.logf("host: %v — dispatch waits for a heartbeat adoption retry", s.rexAdoptErr)
		} else {
			s.openDash(h)
		}
	}
	s.saveActivity()
}

// rexDashLabel labels the block serve keeps `moe dash --watch` in.
const rexDashLabel = "dash"

// openDash puts `moe dash --watch` in the session's "dash" block, so the
// session exists the moment serve starts and the dash reports to the Rex
// picker (OSC 7501) without the operator starting a watch.
//
// A dash block a previous serve left is closed, not adopted: the update
// watcher restarts serve on every reinstall, and an adopted dash would
// go on running the old binary. If that close fails nothing is launched,
// so the session never holds two. Every failure here is only logged —
// the dash is the operator's eye, and must not hold the heartbeat.
func (s *Server) openDash(h *rexHost) {
	argv, err := rexJobArgv(s.opts.MoeBin, []string{"MOE_HOME=" + s.opts.Root}, []string{"dash", "--watch"})
	if err != nil {
		s.logf("host: rex dash not opened: %v", err)
		return
	}
	if session := h.sessionID(); session != "" {
		bs, err := h.blocks(session)
		if err != nil {
			present, probeErr := h.sessionPresent(session)
			if probeErr != nil || present {
				s.logf("host: rex dash not opened: %v (session present: %t, check: %v)", err, present, probeErr)
				return
			}
		}
		for _, b := range bs {
			if b.Label != rexDashLabel {
				continue
			}
			if err := h.closeBlock(b.ID); err != nil {
				s.logf("host: rex dash not opened: close the previous one: %v", err)
				return
			}
		}
	}
	block, err := h.launch(rexDashLabel, argv)
	if err != nil {
		s.logf("host: rex dash not opened: %v", err)
		return
	}
	s.logf("host: rex dash open in block %s", block)
}

// adoptRex picks up the jobs a previous serve launched. The session's
// blocks labelled "heartbeat:<project>" are registered under that id and
// watched: a live one holds its project's single-flight until it exits,
// and an exited one is consumed at once. Either way the exit runs the
// ordinary hooks — emit file, activity, log line, webhook — and the
// backoff ledger, but not the gate's Swept, because this serve never saw
// the dispatch. The emit file is left for the exit to read, since a live
// job may still write it.
//
// An unreadable listing holds dispatch until a retry reconciles the jobs
// or a valid session listing proves absence. Even then the old identity
// remains installed until launch successfully creates its replacement.
func (s *Server) adoptRex(h *rexHost) error {
	session := h.sessionID()
	if session == "" {
		return nil
	}
	bs, err := h.blocks(session)
	if err != nil {
		present, probeErr := h.sessionPresent(session)
		if probeErr != nil {
			return fmt.Errorf("rex session %s unreadable: %w (check session: %v)", session, err, probeErr)
		}
		if present {
			return fmt.Errorf("rex session %s unreadable: %w", session, err)
		}
		return nil
	}
	seen := map[string]bool{}
	for _, b := range bs {
		project := heartbeatProject(b.Label)
		if project == "" {
			continue
		}
		if seen[b.Label] {
			s.logf("host: rex block %s also carries %s — leaving it alone", b.ID, b.Label)
			continue
		}
		seen[b.Label] = true
		c := s.children.registerRex(b.Label, b.ID, "adopted", s.opts.Logger)
		s.logf("heartbeat: adopted %s from rex block %s", project, b.ID)
		s.activity.recordSweepStart(project, c.started)
		s.startHeartbeatWaiter(project, c, false)
	}
	return nil
}
