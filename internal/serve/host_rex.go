package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// spawn launches a job as a Rex block and registers it under id.
//
// Retention ends here: close the previous child's block and every done
// block for this id, including screens retained across serve restarts.
// Uncertain listing or cleanup holds this launch for the next tick.
//
// A launch that errors is unresolved, not failed: Rex may have created
// the block before the reply was lost. The session is searched for the
// label, and a match is this launch. No match means nothing is launched
// this tick, and never locally.
//
// Not safe for concurrent calls on one id. The heartbeat ticker is the
// only spawner and runs one goroutine.
func (h *rexHost) spawn(id, moeBin string, args []string, root string, logger io.Writer) (*child, error) {
	cs := h.children
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	cs.mu.RLock()
	prev, had := cs.all[id]
	previousBlock := h.jobs[prev]
	cs.mu.RUnlock()

	var active string
	var blocks []string
	seen := map[string]bool{}
	if had && previousBlock != "" {
		blocks = append(blocks, previousBlock)
		seen[previousBlock] = true
	}
	if session := h.sessionID(); session != "" {
		bs, err := h.blocks(session)
		if err != nil {
			if present, probeErr := h.sessionPresent(session); probeErr != nil || present {
				return nil, fmt.Errorf("list kept rex blocks: %w (session present: %t, check: %v)", err, present, probeErr)
			}
		}
		if err := validateOwnedBlocks(bs); err != nil {
			return nil, err
		}
		for _, b := range bs {
			if b.Label == id {
				active = b.ID
			}
			if b.Label == "done:"+id && !seen[b.ID] {
				blocks = append(blocks, b.ID)
				seen[b.ID] = true
			}
		}
	}
	if active != "" {
		return h.register(id, active, false, logger), nil
	}
	for _, block := range blocks {
		// A kept block the operator closed by hand, or that went with
		// its server, fails the close with nothing left to close. Only a
		// block still in the session holds the launch back.
		if err := h.closeBlock(block); err != nil {
			if found, ferr := h.findBlock(block); ferr != nil || found != "" {
				return nil, fmt.Errorf("close kept rex block %s: %w", block, err)
			}
		}
	}
	if had {
		cs.mu.Lock()
		delete(h.jobs, prev)
		cs.mu.Unlock()
	}

	// The same two variables serve adds for a local child.
	argv, err := rexJobArgv(moeBin, []string{"MOE_HOME=" + root, "MOE_SERVE_AGENT=1"}, args)
	if err != nil {
		return nil, err
	}
	if cs.beforeLaunch != nil {
		cs.beforeLaunch(id)
	}
	session := h.sessionID()
	block, err := h.launch(id, argv)
	if err != nil {
		found, ferr := h.findLabel(id)
		if ferr != nil || found == "" {
			return nil, fmt.Errorf("rex launch unresolved, nothing launched: %w", err)
		}
		h.logf("rex: launch of %s reported %v, but block %s carries its label — taking it", id, err, found)
		block = found
	}
	c := h.register(id, block, false, logger)
	if h.sessionID() != session {
		h.openDash()
	}
	return c, nil
}

// register records a Rex-hosted child and starts its watcher. how is
// the activity ring's word for it: "started" or "adopted".
func (h *rexHost) register(id, block string, adopted bool, logger io.Writer) *child {
	cs := h.children
	how := "started"
	if adopted {
		how = "adopted"
	}
	c := &child{id: id, started: time.Now(), done: make(chan struct{}), settled: make(chan struct{}), dispatched: !adopted}
	cs.mu.Lock()
	delete(h.jobs, cs.all[id])
	h.jobs[c] = block
	cs.mu.Unlock()
	cs.register(c, how+" in rex block "+block)
	h.workers.Go(func() { h.watch(c, block, logger) })
	return c
}

// watch is the Rex host's reader: wait for the exit, keep the same
// bounded tail the local host keeps, then the shared exit path. Mark
// the reported result before closing a clean block or retaining a failed
// screen until the next launch of the same id.
//
// A shutdown returns without finishing: the job is Rex's, and a
// restarted serve adopts it.
func (h *rexHost) watch(c *child, block string, logger io.Writer) {
	code, err := h.waitExit(block)
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
		if screen, cerr := h.capture(block); cerr != nil {
			h.logf("rex: capture block %s: %v", block, cerr)
		} else {
			c.appendTail([]byte(screen))
		}
	}
	c.finish(h.children, exitErr, logger)
	defer close(c.settled)
	if errors.Is(err, errRexBlockGone) {
		return
	}
	present, ackErr := h.acknowledge(block, c.id)
	if ackErr != nil || !present {
		return
	}
	if exitErr == nil {
		if err := h.closeBlock(block); err != nil {
			h.logf("rex: close block %s: %v", block, err)
		}
	}
}

// selectHost is the only composition point. Identity is inspected before fallback.
func (s *Server) selectHost() {
	body, readErr := os.ReadFile(rexSessionPath(s.opts.Root))
	recorded := !os.IsNotExist(readErr)
	h, err := discoverRex(s.opts.Root, s.opts.Logger)
	if h == nil && err == nil && !recorded {
		s.activity.setHost("local pty", "")
		s.logf("host: local pty — sweeps stop when serve stops")
		s.saveActivity()
		return
	}
	if h == nil {
		h = newRexHost("", s.opts.Root, s.opts.Logger)
		h.discoveryPending = true
	}
	h.permanentErr = err
	h.children = s.children
	h.moeBin = s.opts.MoeBin
	if readErr == nil && strings.TrimSpace(string(body)) != "" {
		h.session = strings.TrimSpace(string(body))
	} else if recorded {
		h.identityPending = true
	}
	s.children.host = h
	s.activity.setHost("rex", "")
	s.reconcileHost()
}

func (h *rexHost) reconcile() error {
	if h.permanentErr != nil {
		return fmt.Errorf("rex host unusable: %w", h.permanentErr)
	}
	if h.candidate != "" {
		if err := h.establish(); err != nil {
			return err
		}
		h.openDash()
	}
	if h.ready {
		return nil
	}
	if h.identityPending {
		body, err := os.ReadFile(rexSessionPath(h.root))
		if err != nil {
			return fmt.Errorf("rex identity unreadable: %w", err)
		}
		session := strings.TrimSpace(string(body))
		if session == "" {
			return errors.New("rex identity is empty")
		}
		h.session = session
		h.identityPending = false
	}
	if h.discoveryPending {
		discovered, err := discoverRex(h.root, h.logger)
		if err != nil {
			h.permanentErr = err
			return fmt.Errorf("rex host unusable: %w", err)
		}
		if discovered == nil {
			return errors.New("rex discovery unavailable with recorded identity")
		}
		h.bin = discovered.bin
		discovered.cancel()
		h.discoveryPending = false
	}
	if err := h.adopt(); err != nil {
		return fmt.Errorf("rex adoption pending: %w", err)
	}
	h.ready = true
	h.openDash()
	if h.candidate != "" {
		return fmt.Errorf("rex session %s persistence pending", h.candidate)
	}
	return nil
}
func (h *rexHost) shutdown(context.Context, io.Writer) { h.cancel() }

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
func (h *rexHost) openDash() {
	argv, err := rexJobArgv(h.moeBin, []string{"MOE_HOME=" + h.root}, []string{"dash", "--watch"})
	if err != nil {
		h.logf("host: rex dash not opened: %v", err)
		return
	}
	if session := h.sessionID(); session != "" {
		bs, err := h.blocks(session)
		if err != nil {
			present, probeErr := h.sessionPresent(session)
			if probeErr != nil || present {
				h.logf("host: rex dash not opened: %v (session present: %t, check: %v)", err, present, probeErr)
				return
			}
		}
		for _, b := range bs {
			if b.Label != rexDashLabel {
				continue
			}
			if err := h.closeBlock(b.ID); err != nil {
				h.logf("host: rex dash not opened: close the previous one: %v", err)
				return
			}
		}
	}
	block, err := h.launch(rexDashLabel, argv)
	if err != nil {
		h.logf("host: rex dash not opened: %v", err)
		return
	}
	h.logf("host: rex dash open in block %s", block)
}

// adopt picks up the jobs a previous serve launched. The session's
// blocks labelled "heartbeat:<project>" are registered under that id and
// watched: a live one holds its project's single-flight until it exits,
// and an exited one is consumed at once. Either way the exit runs the
// ordinary hooks — emit file, activity, log line, webhook — and the
// backoff ledger, but not the gate's Swept, because this serve never saw
// the dispatch. Done labels are retained screens, never adopted or reported
// again. The emit file is left for an active job's exit to read, since
// that job may still write it.
//
// An unreadable listing holds dispatch until a retry reconciles the jobs
// or a valid session listing proves absence. Even then the old identity
// remains installed until launch successfully creates its replacement.
func (h *rexHost) adopt() error {
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
	if err := validateOwnedBlocks(bs); err != nil {
		return err
	}
	for _, b := range bs {
		project := heartbeatProject(b.Label)
		if project == "" {
			continue
		}
		h.register(b.Label, b.ID, true, h.logger)
		h.logf("heartbeat: adopted %s from rex block %s", project, b.ID)
	}

	return nil
}

// Validate the whole list before any observer or path access can start.
func validateOwnedBlocks(bs []rexBlock) error {
	seen := map[string]bool{}
	for _, b := range bs {
		label := b.Label
		if strings.HasPrefix(label, "done:"+heartbeatChildPrefix) {
			label = strings.TrimPrefix(label, "done:")
		}
		if !strings.HasPrefix(label, heartbeatChildPrefix) {
			continue
		}
		if heartbeatProject(label) == "" {
			return fmt.Errorf("rex malformed owned label %q", b.Label)
		}
		if b.Label != label {
			continue
		}
		if seen[label] {
			return fmt.Errorf("rex duplicate active label %q", label)
		}
		seen[label] = true
	}
	return nil
}
