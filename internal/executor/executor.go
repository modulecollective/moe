// Package executor runs one stage turn against the local `claude`
// binary: assembles the CLI args, wires stdio to the operator's
// terminal, and mirrors the session's on-disk JSONL into the
// document's thread.jsonl when the turn ends.
package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

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
	// Metadata is the run's on-disk state. Read-only. Optional —
	// run-less sessions (e.g. wiki lint) pass nil, which skips
	// transcript mirroring at turn end since there is no document
	// thread.jsonl to copy into.
	Metadata *run.Metadata
	// DocID is which document on the run this turn is for. Ignored
	// when Metadata is nil.
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
	// SessionCwd is the document-only fallback cwd: a stable per-document
	// path whose only purpose is to keep claude's encoded-cwd project dir
	// constant across turns so `--resume <sid>` finds its JSONL. Empty
	// for code stages (ClonePath already gives them a stable cwd).
	SessionCwd string
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
	switch {
	case r.ClonePath != "":
		cmd.Dir = r.ClonePath
	case r.SessionCwd != "":
		cmd.Dir = r.SessionCwd
	default:
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
	// Run-less sessions (Metadata nil) skip the copy entirely — there
	// is no per-document thread.jsonl to mirror into.
	if r.Metadata != nil {
		threadPath := filepath.Join(r.Root, run.ThreadPath(r.Metadata.Project, r.Metadata.ID, r.DocID))
		if _, err := claude.CopyTranscript(r.SessionID, threadPath); err != nil && r.Stderr != nil {
			fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
		}
	}
	return runErr
}

// OneShotRequest drives a single non-interactive `claude -p` turn whose
// stdout streams to the operator's terminal as it lands — the
// non-interactive twin of Execute. Same prompt-assembly contract
// (system prompt + add-dirs + sandbox settings + positional user
// prompt), but no session id, no REPL, no transcript mirroring: the
// agent gets one turn, produces its work directly on disk, and exits.
// Used by `moe sdlc new --one-shot` to chain stage turns
// without putting the operator on stdin.
type OneShotRequest struct {
	// Root is the bureaucracy repo root. Passed as --add-dir so the
	// canvas stays reachable when cwd is the sandbox clone.
	Root string
	// Prompt is the assembled --append-system-prompt payload (same
	// shape as Request.Prompt — soul + stage fragment + canvas hint +
	// any one-shot addendum).
	Prompt string
	// UserPrompt is the positional `claude -p <prompt>` argument — the
	// single user turn for this stage.
	UserPrompt string
	// ClonePath, when non-empty, is cwd for the claude subprocess —
	// the per-run sandbox clone for code stages. Empty for
	// document-only stages (cwd falls back to Root).
	ClonePath string
	// Stdout streams claude's output to the operator's terminal. nil
	// falls back to os.Stdout — the runner wants the operator to watch
	// progress so they can Ctrl-C if it goes off the rails.
	Stdout io.Writer
	// Stderr captures claude's diagnostic output. nil falls back to
	// os.Stderr.
	Stderr io.Writer
}

// ExecuteOneShot runs `claude -p` non-interactively and surfaces a
// one-line-per-tool-call progress stream to the operator's terminal so
// the long agent turn doesn't look hung. The agent gets one turn to do
// its work; transcript mirroring is intentionally skipped (the canvas
// + per-turn commit are the durable artifacts — one-shot runs don't
// carry a thread.jsonl). A non-nil error means the subprocess exited
// non-zero or the binary can't be found; callers still commit whatever
// the agent landed on disk because partial work is salvage, not
// contamination.
//
// Implementation: claude is invoked with `--output-format stream-json
// --verbose --include-partial-messages` so its stdout is a JSON event
// stream rather than buffered final text. A reader goroutine maps each
// tool_use event to a short progress line (`> reading <path>`,
// `> bash: <cmd>`, etc.) on r.Stdout; the raw JSON is never shown.
func ExecuteOneShot(r OneShotRequest) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("executor: claude CLI not found on PATH: %w", err)
	}
	// --add-dir is variadic, so --settings/--append-system-prompt
	// must sit between it and the positional user prompt — same
	// ordering as Execute and ExecuteHeadless. Otherwise claude eats
	// the prompt as another directory and the turn launches with
	// nothing to do.
	//
	// --output-format stream-json (with mandatory --verbose) makes
	// claude emit one JSON event per tool call / message instead of
	// buffering a final text response, so the translator below can
	// surface progress as it happens. --include-partial-messages adds
	// fine-grained delta events; we don't render them today, but the
	// flag set matches the design's recommendation so future progress
	// vocabulary (token counts, thinking) can layer on without
	// re-plumbing claude's output mode.
	//
	// --permission-mode bypassPermissions: one-shot has no operator on
	// stdin to approve per-call write/edit/bash prompts, so the default
	// "default" mode silently denies them and the agent's edits never
	// land. Bypass mode skips the per-call prompt; safety still comes
	// from --settings enabling the built-in sandbox plus --add-dir
	// scoping filesystem reach to the worktree/clone.
	args := []string{
		"-p",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--add-dir", r.Root,
		"--settings", sandboxSettings,
		"--append-system-prompt", r.Prompt,
		r.UserPrompt,
	}
	cmd := exec.Command(bin, args...)
	if r.ClonePath != "" {
		cmd.Dir = r.ClonePath
	} else {
		cmd.Dir = r.Root
	}
	// No stdin: -p mode reads only flags + positional prompt. Wiring
	// stdin would let claude block on a tty that nobody's typing into.
	cmd.Stdin = nil
	stdout := r.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	cmd.Stderr = r.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}

	// StdoutPipe + Start/Wait (not Run) — the docs say it's incorrect
	// to call Run when using StdoutPipe, because Wait closes the pipe
	// after the process exits and Run does both internally.
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("executor: claude -p stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("executor: claude -p start: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeOneShotProgress(pipe, stdout, r.Root)
	}()
	waitErr := cmd.Wait()
	<-done
	return waitErr
}

// HeadlessRequest drives a one-shot, non-interactive `claude -p` call —
// no REPL, no --session-id, no transcript mirroring. The agent reads
// the prompts, produces its response on stdout, and exits. Suited to
// callers whose agent job is a bounded question-and-answer rather than
// a conversation (e.g. a future wiki finalize that asks the model to
// summarise a diff into one log entry).
type HeadlessRequest struct {
	// WorkDir is the cwd for the claude subprocess. Typically the
	// bureaucracy root so any incidental Read goes through the
	// canonical paths.
	WorkDir string
	// Model, if non-empty, is passed as --model. Empty string defers to
	// the operator's configured default. Shelve passes "sonnet".
	Model string
	// AllowedTools is the comma-joined --allowed-tools list, e.g.
	// "Read". Empty means Claude's default set — callers that want a
	// locked-down tool surface must set this explicitly.
	AllowedTools string
	// SystemPrompt is appended to Claude's system prompt via
	// --append-system-prompt, same as the interactive path.
	SystemPrompt string
	// UserPrompt is the `claude -p <prompt>` positional argument — the
	// single "here is your task" turn for a headless run.
	UserPrompt string
	// AddDirs are passed as repeated --add-dir flags for any paths the
	// agent needs to read outside WorkDir.
	AddDirs []string
	// Timeout bounds the whole invocation. Zero means no timeout, which
	// no caller should actually choose — headless calls that hang are
	// the worst kind of silent failure.
	Timeout time.Duration
	// Stderr, if non-nil, streams the subprocess's stderr so the
	// operator can see progress/errors in real time. Stdout is captured
	// and returned rather than streamed — callers parse it (JSON, a
	// short answer, etc.) and decide what to show the operator.
	Stderr io.Writer
}

// ExecuteHeadless runs a single non-interactive `claude -p` call under
// a timeout and returns the subprocess's stdout as bytes. A non-nil
// error means claude exited non-zero, the timeout fired, or the binary
// can't be found — the stdout bytes collected up to that point are
// still returned so the caller can log them for debugging. Callers
// treat failures as "this turn produced no commit; operator can retry"
// — there is no state to unwind.
func ExecuteHeadless(r HeadlessRequest) ([]byte, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("executor: claude CLI not found on PATH: %w", err)
	}

	args := []string{"-p"}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.AllowedTools != "" {
		args = append(args, "--allowed-tools", r.AllowedTools)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	args = append(args, "--settings", sandboxSettings)
	if r.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", r.SystemPrompt)
	}
	// Positional prompt is the single user turn. --add-dir is variadic,
	// so the --settings/--append-system-prompt flags must sit between
	// --add-dir and the positional prompt to avoid claude eating it as
	// another directory.
	args = append(args, r.UserPrompt)

	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = r.WorkDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = r.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out.Bytes(), fmt.Errorf("executor: claude -p timed out after %s", r.Timeout)
		}
		return out.Bytes(), fmt.Errorf("executor: claude -p: %w", err)
	}
	return out.Bytes(), nil
}
