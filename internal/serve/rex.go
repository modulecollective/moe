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
// An active block carries the child id ("heartbeat:<project>"). Once
// reported, it is renamed "done:heartbeat:<project>" so restart can
// distinguish a retained screen from an unprocessed exit. The only file
// is .moe/rex-session, which names the session.

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
// program has exited or acknowledge its completion. Variable so tests
// can shorten it.
var rexRetryDelay = 5 * time.Second

// rexHost owns Rex resources and retryable initialization. The ticker alone
// mutates readiness and candidates; observers read the published session under mu.
type rexHost struct {
	children         *children
	moeBin           string
	jobs             map[*child]string // resource handles, guarded by children.mu
	ready            bool
	discoveryPending bool
	identityPending  bool
	permanentErr     error
	candidate        string
	bootstrap        string
	bin              string
	root             string
	logger           io.Writer

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
	return &rexHost{bin: bin, root: root, logger: logger, ctx: ctx, cancel: cancel, jobs: map[*child]string{}}
}

// rexExit is a Rex-hosted program's non-zero exit, spelled the way
// exec.ExitError spells a local one so logs and /serve read the same.
type rexExit int

func (e rexExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// errRexBlockGone is a block that vanished before reporting an exit —
// closed by hand, or lost with its server.
var errRexReply = errors.New("invalid Rex reply")

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
	out, err := h.call(h.ctx, "ls", "--json")
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
	out, err := h.call(h.ctx, "block", "ls", "--json", "-s", session)
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
	if err := validateOwnedBlocks(bs); err != nil {
		return "", err
	}
	id := ""
	for _, b := range bs {
		if b.Label == label {
			if id != "" {
				return "", fmt.Errorf("rex duplicate active label %q", label)
			}
			id = b.ID
		}
	}
	return id, nil
}

// launch only starts work in a durably recorded session. Session establishment
// runs a harmless bootstrap, so a lost creation reply can never orphan an agent.
func (h *rexHost) launch(label string, argv []string) (string, error) {
	if h.candidate != "" {
		if err := h.establish(); err != nil {
			return "", err
		}
	}
	session := h.sessionID()
	if session == "" {
		if err := h.establish(); err != nil {
			return "", err
		}
		session = h.sessionID()
	}
	run := func() (string, error) {
		args := append([]string{"run", "--json", "--focus=false", "--keep-open", "-s", session, "--label", label, "--cwd", h.root, "--"}, argv...)
		out, err := h.call(context.Background(), args...)
		if err != nil {
			return "", err
		}
		var ran struct {
			BlockIDs []string `json:"block_ids"`
		}
		if err := json.Unmarshal(out, &ran); err != nil || len(ran.BlockIDs) != 1 || ran.BlockIDs[0] == "" {
			return "", fmt.Errorf("rex run: %w %q", errRexReply, firstLine(string(out)))
		}
		return ran.BlockIDs[0], nil
	}
	block, err := run()
	if err == nil {
		return block, nil
	}
	// A malformed successful reply is uncertainty too. Only a command failure
	// followed by valid absence permits replacement; don't reuse stale evidence.
	// Readback by label is handled by the caller for lost run replies.
	if errors.Is(err, errRexReply) {
		return "", err
	}
	present, probeErr := h.sessionPresent(session)
	if probeErr != nil {
		return "", fmt.Errorf("%w (check session: %v)", err, probeErr)
	}
	if present {
		return "", err
	}
	h.logf("rex: session %s is gone — creating a replacement", session)
	if err := h.establish(); err != nil {
		return "", err
	}
	session = h.sessionID()
	return run()
}

func (h *rexHost) establish() error {
	if h.candidate == "" {
		out, err := h.call(context.Background(), "new", rexSessionLabel, "--json", "--keep-open", "--cwd", h.root, "--window", "bootstrap", "--", "/usr/bin/true")
		if err != nil {
			return err
		}
		var created struct {
			SessionID string `json:"session_id"`
			Windows   []struct {
				BlockIDs []string `json:"block_ids"`
			} `json:"initial_windows"`
		}
		if err := json.Unmarshal(out, &created); err != nil || created.SessionID == "" || len(created.Windows) != 1 || len(created.Windows[0].BlockIDs) != 1 || created.Windows[0].BlockIDs[0] == "" {
			return fmt.Errorf("rex new: unrecognised reply %q", firstLine(string(out)))
		}
		h.candidate = created.SessionID
		h.bootstrap = created.Windows[0].BlockIDs[0]
	}
	path := rexSessionPath(h.root)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("rex record session: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "rex-session-*")
	if err != nil {
		return fmt.Errorf("rex record session: %w", err)
	}
	defer os.Remove(f.Name())
	_, writeErr := f.WriteString(h.candidate + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("rex record session: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("rex record session: %w", closeErr)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("rex record session: %w", err)
	}
	h.mu.Lock()
	h.session = h.candidate
	h.mu.Unlock()
	h.candidate = ""
	if err := h.closeBlock(h.bootstrap); err != nil {
		h.logf("rex: close bootstrap %s: %v", h.bootstrap, err)
	}
	h.bootstrap = ""
	return nil
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
		LastExit json.RawMessage `json:"last_exit"`
	}
	if err := json.Unmarshal(out, &proc); err != nil || len(proc.LastExit) == 0 {
		return 0, false, fmt.Errorf("rex block call process: invalid reply %q", firstLine(string(out)))
	}
	if bytes.Equal(bytes.TrimSpace(proc.LastExit), []byte("null")) {
		return 0, false, nil
	}
	var exit struct {
		Code *int `json:"exit_code"`
	}
	if err := json.Unmarshal(proc.LastExit, &exit); err != nil || exit.Code == nil {
		return 0, false, fmt.Errorf("rex block call process: invalid exit %q", firstLine(string(out)))
	}
	return *exit.Code, true, nil
}

// lookupBlock returns the block's current metadata, or a zero value once
// a valid listing proves the block (or its session) absent.
func (h *rexHost) lookupBlock(block string) (rexBlock, error) {
	session := h.sessionID()
	if session == "" {
		return rexBlock{}, errors.New("no rex session")
	}
	bs, err := h.blocks(session)
	if err != nil {
		if present, probeErr := h.sessionPresent(session); probeErr == nil && !present {
			return rexBlock{}, nil
		}
		return rexBlock{}, err
	}
	for _, b := range bs {
		if b.ID == block {
			return b, nil
		}
	}
	return rexBlock{}, nil
}

// findBlock reports whether block is still in the session: its id, or
// "" when it is gone.
func (h *rexHost) findBlock(block string) (string, error) {
	b, err := h.lookupBlock(block)
	return b.ID, err
}

// acknowledge marks an already reported exit without discarding its
// screen. Readback resolves a lost rename reply; uncertainty retries
// without replaying the completion hooks. False means confirmed absence.
func (h *rexHost) acknowledge(block, id string) (bool, error) {
	label := "done:" + id
	for {
		if h.ctx.Err() != nil {
			return false, h.ctx.Err()
		}
		_, err := h.call(h.ctx, "block", "rename", "-s", h.sessionID(), block, label)
		if err == nil {
			return true, nil
		}
		if h.ctx.Err() != nil {
			return false, h.ctx.Err()
		}
		h.logf("rex: acknowledge block %s: %v — checking label", block, err)
		if b, readErr := h.lookupBlock(block); readErr == nil {
			if b.ID == "" {
				return false, nil
			}
			if b.Label == label {
				return true, nil
			}
		}
		select {
		case <-h.ctx.Done():
			return false, h.ctx.Err()
		case <-time.After(rexRetryDelay):
		}
	}
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
