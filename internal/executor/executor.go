// Package executor abstracts the backend that actually runs a stage
// turn, so moe can host more than one: the interactive Claude Code CLI
// (today's only implementation) and, later, Anthropic's Managed Agents
// API for fire-and-forget async runs.
//
// The interface is small on purpose. A Request carries everything an
// executor needs to run one turn against one document; the executor
// owns the details of how the agent runs, what filesystem it sees, and
// where the turn's transcript ends up. The stage driver stays the same
// shape regardless of which executor it picks.
package executor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/request"
)

// Request is the inputs for one turn on one document.
type Request struct {
	// Root is the bureaucracy repo root. Executors that drop transcripts
	// or other artifacts back into the bureaucracy use it to compute
	// canonical in-repo paths.
	Root string
	// Metadata is the request's on-disk state. Read-only for executors.
	Metadata *request.Metadata
	// DocID is which document on the request this turn is for.
	DocID string
	// SessionID is the canonical UUID that identifies this document's
	// conversation. Executors use it to create or resume their own
	// session keyed to the same identity.
	SessionID string
	// NewSession is true when SessionID was just minted this turn
	// (first ever call for this document) and false when it already
	// has a server-side session that should be resumed.
	NewSession bool
	// Prompt is the assembled system prompt from buildSystemPrompt.
	Prompt string
	// ClonePath is the private per-request sandbox clone of the target
	// project's submodule, or "" for document-only requests. When set,
	// executors should run the agent with this as its working directory.
	ClonePath string
	// InitialPrompt, if non-empty, is auto-sent as the first user message
	// of the turn so the operator doesn't have to type anything to kick
	// the session off. Stage handlers use it for things like a cue to the
	// user in design or "implement the design" in code.
	InitialPrompt string
	// Stdin / Stdout / Stderr let executors wire interactive agents to
	// the operator's terminal or capture output in tests.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Executor runs one turn. A non-nil error means the agent itself exited
// non-zero; callers still commit whatever document edits landed on disk,
// because the operator may have bailed mid-edit intentionally.
type Executor interface {
	Execute(r Request) error
}

// ClaudeCLI runs a turn against the local `claude` binary. It is the
// default executor stage sessions use; behavior is unchanged from the
// pre-interface implementation.
type ClaudeCLI struct{}

// sandboxSettings is layered on top of the operator's settings.json via
// `--settings` to pin the claude subprocess into the built-in sandbox
// regardless of the operator's personal configuration. Array fields
// (filesystem/network allowlists) merge with the operator's settings,
// so this only forces the toggle on without narrowing their allowlists.
const sandboxSettings = `{"sandbox":{"enabled":true}}`

// Execute shells out to `claude`, wires stdio to the operator's
// terminal, and mirrors the session's on-disk JSONL into the document's
// thread.jsonl when the turn ends.
func (ClaudeCLI) Execute(r Request) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("executor: claude CLI not found on PATH: %w", err)
	}

	// Claude Code uses --session-id to create and --resume to continue.
	// NewSession was set upstream by EnsureDocument exactly when the
	// UUID was minted this turn.
	sessionFlag := "--resume"
	if r.NewSession {
		sessionFlag = "--session-id"
	}
	// --add-dir <root> grants access to the bureaucracy repo even when
	// cwd is the sandbox clone, so the canvas and upstream documents
	// stay reachable without per-call permission prompts. It's variadic
	// (<directories...>), so it must not be the last flag before the
	// positional prompt — otherwise claude parses the prompt as a second
	// directory and the session launches with nothing to send.
	args := []string{
		sessionFlag, r.SessionID,
		"--add-dir", r.Root,
		"--settings", sandboxSettings,
		"--append-system-prompt", r.Prompt,
	}
	// A positional prompt launches claude interactively but auto-sends
	// it as the first user message, so the operator lands in a session
	// that's already in motion.
	if r.InitialPrompt != "" {
		args = append(args, r.InitialPrompt)
	}
	cmd := exec.Command(bin, args...)
	if r.ClonePath != "" {
		cmd.Dir = r.ClonePath
	} else {
		cmd.Dir = r.Root
	}
	cmd.Stdin = r.Stdin
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	runErr := cmd.Run()

	// Transcript copy is best-effort: a missing file is legal (operator
	// aborted before claude wrote anything, or ran on another machine),
	// and other I/O errors don't block the caller's post-run commit.
	threadPath := filepath.Join(r.Root, request.ThreadPath(r.Metadata.Project, r.Metadata.ID, r.DocID))
	if _, err := claude.CopyTranscript(r.SessionID, threadPath); err != nil && r.Stderr != nil {
		fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
	}
	return runErr
}
