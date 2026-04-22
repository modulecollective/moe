// Package executor runs one stage turn against the local `claude`
// binary: assembles the CLI args, wires stdio to the operator's
// terminal, and mirrors the session's on-disk JSONL into the
// document's thread.jsonl when the turn ends.
package executor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/run"
)

// Request is the inputs for one turn on one document.
type Request struct {
	// Root is the bureaucracy repo root. Used to compute canonical
	// in-repo paths (e.g. where to write the mirrored transcript) and
	// passed to claude as --add-dir so the canvas stays reachable when
	// the agent's cwd is the sandbox clone.
	Root string
	// Metadata is the run's on-disk state. Read-only.
	Metadata *run.Metadata
	// DocID is which document on the run this turn is for.
	DocID string
	// SessionID is the canonical UUID that identifies this document's
	// conversation. Claude Code uses it to create or resume its own
	// session keyed to the same identity.
	SessionID string
	// NewSession is true when SessionID was just minted this turn
	// (first ever call for this document) and false when it already
	// has a server-side session that should be resumed.
	NewSession bool
	// Prompt is the assembled system prompt from buildSystemPrompt.
	Prompt string
	// ClonePath is the private per-run sandbox clone of the target
	// project's submodule, or "" for document-only runs. When set,
	// claude runs with this as its working directory.
	ClonePath string
	// InitialPrompt, if non-empty, is auto-sent as the first user message
	// of the turn so the operator doesn't have to type anything to kick
	// the session off. Stage handlers use it to have the agent greet the
	// operator and ask what they'd like to work on.
	InitialPrompt string
	// Stdin / Stdout / Stderr wire the interactive agent to the
	// operator's terminal or capture output in tests.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// sandboxSettings is layered on top of the operator's settings.json via
// `--settings` to pin the claude subprocess into the built-in sandbox
// regardless of the operator's personal configuration. Array fields
// (filesystem/network allowlists) merge with the operator's settings,
// so this only forces the toggle on without narrowing their allowlists.
const sandboxSettings = `{"sandbox":{"enabled":true}}`

// Execute shells out to `claude`, wires stdio to the operator's
// terminal, and mirrors the session's on-disk JSONL into the document's
// thread.jsonl when the turn ends. A non-nil error means claude itself
// exited non-zero; callers still commit whatever document edits landed
// on disk, because the operator may have bailed mid-edit intentionally.
func Execute(r Request) error {
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
	threadPath := filepath.Join(r.Root, run.ThreadPath(r.Metadata.Project, r.Metadata.ID, r.DocID))
	if _, err := claude.CopyTranscript(r.SessionID, threadPath); err != nil && r.Stderr != nil {
		fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
	}
	return runErr
}
