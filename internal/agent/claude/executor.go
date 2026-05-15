// Package claude is the agent.Agent implementation for the local
// `claude` binary: assembles the CLI args, wires stdio to the
// operator's terminal, and mirrors the session's on-disk JSONL into
// the document's per-agent thread file when the turn ends.
package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

func init() {
	agent.Register("claude", Agent{})
}

// Agent is the claude implementation of agent.Agent. Stateless — the
// per-turn data lives on the Request structs; this type is the
// dispatch hook the registry hands back.
type Agent struct{}

// sandboxSettingsJSON is the `--settings` payload that pins the
// claude subprocess into the built-in sandbox regardless of the
// operator's personal configuration. Array fields (filesystem /
// network allowlists) merge across settings scopes, so this enables
// the sandbox without narrowing the operator's allowlists.
//
// No gitdir widening: under the plain-clone primitive the clone's
// gitdir lives inside the clone at `<clonePath>/.git/`, which is
// already in the writable scope via `--add-dir <clonePath>`. The
// bureaucracy session worktree's gitdir at
// `<root>/.git/worktrees/<uuid>/` is reachable via the
// `--add-dir <root>` claude executor also passes. Both index-mutating
// paths work without per-payload widening.
const sandboxSettingsJSON = `{"sandbox":{"enabled":true}}`

// executeArgs builds the interactive `claude` flag set. Kept separate
// from Execute so the argument shape can be exercised in tests without
// shelling out to the binary.
//
// Ordering rule: `--add-dir` is variadic (<directories...>), so any
// `--add-dir <path>` pair must sit before `--settings` /
// `--append-system-prompt` and the optional positional prompt;
// otherwise claude eats the next flag's value as another directory
// and the session launches with nothing to send.
func executeArgs(r agent.Request) []string {
	// Claude Code uses --session-id to create and --resume to continue.
	// NewSession was set upstream by EnsureDocument exactly when the
	// UUID was minted this turn.
	sessionFlag := "--resume"
	if r.NewSession {
		sessionFlag = "--session-id"
	}
	// --add-dir <root> grants access to the bureaucracy repo. Code
	// stages now run cwd = root (bureaucracy session worktree) and
	// reach the project clone via --add-dir <clone>; document-only
	// stages run cwd = sessionCwd and reach root via --add-dir. Either
	// way the explicit add-dir keeps the canvas and upstream documents
	// reachable. Stage-provided AddDirs (dev-env MOE_HOME /
	// MOE_DEV_TMPDIR) ride alongside so the test-stage `moe`
	// subprocess can write to its isolated bureaucracy.
	args := []string{sessionFlag, r.SessionID, "--add-dir", r.Root}
	if r.ClonePath != "" {
		args = append(args, "--add-dir", r.ClonePath)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	args = append(args,
		"--settings", sandboxSettingsJSON,
		"--append-system-prompt", r.Prompt,
	)
	// A positional prompt launches claude interactively but auto-sends
	// it as the first user message, so the operator lands in a session
	// that's already in motion.
	if r.InitialPrompt != "" {
		args = append(args, r.InitialPrompt)
	}
	return args
}

// executeOneShotArgs builds the `claude -p` flag set. Same ordering
// rule as executeArgs: --add-dir pairs sit before --settings /
// --append-system-prompt / positional prompt.
//
// --output-format stream-json (with mandatory --verbose) makes claude
// emit one JSON event per tool call / message instead of buffering a
// final text response, so the translator can surface progress as it
// happens. --include-partial-messages adds fine-grained delta events;
// we don't render them today, but the flag set matches the design's
// recommendation so future progress vocabulary (token counts,
// thinking) can layer on without re-plumbing claude's output mode.
//
// --permission-mode bypassPermissions: one-shot has no operator on
// stdin to approve per-call write/edit/bash prompts, so the default
// "default" mode silently denies them and the agent's edits never
// land. Bypass mode skips the per-call prompt; safety still comes
// from --settings enabling the built-in sandbox plus --add-dir
// scoping filesystem reach to the worktree/clone.
func executeOneShotArgs(r agent.OneShotRequest) []string {
	args := []string{"-p"}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--add-dir", r.Root,
	)
	if r.ClonePath != "" {
		args = append(args, "--add-dir", r.ClonePath)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	args = append(args,
		"--settings", sandboxSettingsJSON,
		"--append-system-prompt", r.Prompt,
		r.UserPrompt,
	)
	return args
}

// Execute shells out to `claude`, wires stdio to the operator's
// terminal, and mirrors the session's on-disk JSONL into the
// document's thread file when the turn ends. The returned string is
// the session id the agent reports back — for claude that's always
// the SessionID we passed in. A non-nil error means claude itself
// exited non-zero; callers still commit whatever document edits
// landed on disk, because the operator may have bailed mid-edit
// intentionally.
func (Agent) Execute(r agent.Request) (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return r.SessionID, fmt.Errorf("claude: CLI not found on PATH: %w", err)
	}

	args := executeArgs(r)
	cmd := exec.Command(bin, args...)
	// cwd-inversion shape: code-bearing stages (r.ClonePath set) run
	// cwd = r.Root (the bureaucracy session worktree) and reach the
	// project clone via --add-dir. Document-only stages run cwd =
	// r.SessionCwd so claude's encoded-cwd project dir stays stable
	// across turns (--resume <sid> finds its JSONL).
	switch {
	case r.ClonePath != "":
		cmd.Dir = r.Root
	case r.SessionCwd != "":
		cmd.Dir = r.SessionCwd
	default:
		cmd.Dir = r.Root
	}
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
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
	// is no per-document thread file to mirror into.
	if r.Metadata != nil {
		threadPath := filepath.Join(r.Root, run.ThreadPathFor("claude", r.Metadata.Project, r.Metadata.ID, r.DocID))
		if _, err := CopyTranscript(r.SessionID, threadPath); err != nil && r.Stderr != nil {
			fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
		}
	}
	return r.SessionID, runErr
}

// ExecuteOneShot runs `claude -p` non-interactively and surfaces a
// one-line-per-tool-call progress stream to the operator's terminal so
// the long agent turn doesn't look hung. The agent gets one turn to do
// its work; transcript mirroring is intentionally skipped (the canvas
// + per-turn commit are the durable artifacts — one-shot runs don't
// carry a thread file). Returns the empty session id and a non-nil
// error on subprocess failure; callers still commit whatever the
// agent landed on disk because partial work is salvage.
//
// Implementation: claude is invoked with `--output-format stream-json
// --verbose --include-partial-messages` so its stdout is a JSON event
// stream rather than buffered final text. A reader goroutine maps each
// tool_use event to a short progress line (`> reading <path>`,
// `> bash: <cmd>`, etc.) on r.Stdout; the raw JSON is never shown.
func (Agent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude: CLI not found on PATH: %w", err)
	}
	args := executeOneShotArgs(r)
	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// cwd-inversion shape: r.Root is the canonical cwd (bureaucracy
	// session worktree for code stages, bureaucracy root for
	// document-only). The project clone is reached via --add-dir.
	cmd.Dir = r.Root
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
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
		return "", fmt.Errorf("claude: -p stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude: -p start: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeOneShotProgress(pipe, stdout, r.Root)
	}()
	waitErr := cmd.Wait()
	<-done
	if waitErr != nil && r.Timeout > 0 && ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("claude: -p timed out after %s", r.Timeout)
	}
	return "", waitErr
}

// CopyTranscript is the Agent method form of the package-level
// CopyTranscript func. Defined as a method so the registry returns
// something that satisfies agent.Agent.CopyTranscript.
func (Agent) CopyTranscript(sessionID, dest string) (bool, error) {
	return CopyTranscript(sessionID, dest)
}

// TranscriptExists reports whether claude's per-session JSONL is at
// the canonical path it would read for `--resume sessionID` from cwd.
// True with nil err means "safe to --resume"; false with nil err is
// the re-mint-and-warn branch the stage pre-flight uses.
func (Agent) TranscriptExists(sessionID, cwd string) (bool, error) {
	canonical := CanonicalTranscriptPath(cwd, sessionID)
	if canonical == "" {
		return false, nil
	}
	switch _, err := os.Stat(canonical); {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// Compile-time check that Agent satisfies the interface.
var _ agent.Agent = Agent{}
