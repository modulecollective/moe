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
	"strings"

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

// pickCwd picks the cwd claude runs under. sessionCwd is the
// per-document stable path stage.go owns; claude buckets transcripts
// by EncodeCwd(cwd), so resume needs cwd to be the same value on every
// turn. Root is the fallback for run-less callers (rebase_resolve)
// that have no session to resume.
func pickCwd(sessionCwd, root string) string {
	if sessionCwd != "" {
		return sessionCwd
	}
	return root
}

// scrubbedKeys names the env vars stripped from every claude
// subprocess's inherited environment. Both override Anthropic's OAuth
// path silently: ANTHROPIC_API_KEY is documented to take precedence
// over an active Claude subscription, and ANTHROPIC_AUTH_TOKEN is sent
// as the Authorization bearer header with the same precedence. An
// operator who set either for some other tool would otherwise have MoE
// billing per-token API rates without warning.
var scrubbedKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
}

// filteredEnv returns os.Environ() with scrubbedKeys removed, then
// appends extra. Filtering (not setting to "") matters: an empty value
// can read as "set but blank" to some clients and yield a 401 instead
// of the OAuth fallback we want.
func filteredEnv(extra []string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+len(extra))
	for _, kv := range src {
		drop := false
		for _, k := range scrubbedKeys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

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
	cmd.Dir = pickCwd(r.SessionCwd, r.Root)
	cmd.Env = filteredEnv(r.ExtraEnv)
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
	// Route through agent.StartCommand so an operator Ctrl-C while
	// claude is running becomes a non-nil runErr (ErrInterrupted) rather
	// than a clean-looking exit; non-zero claude exits keep their
	// *exec.ExitError shape.
	var runErr error
	ac, startErr := agent.StartCommand(cmd)
	if startErr != nil {
		runErr = startErr
	} else {
		runErr = ac.Wait()
	}

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
// its work. When r.ThreadPath is set, the per-session JSONL is
// mirrored into it after the turn returns so the operator (and the
// post-headless auto-tail) can read what happened — claude's session
// id is plucked off the first `system / init` event in the stream and
// used to find the right rollout.
//
// Returns the session id captured from the stream (empty when no init
// event fired before the turn died) and a non-nil error on subprocess
// failure; callers still commit whatever the agent landed on disk
// because partial work is salvage.
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
	cmd.Dir = pickCwd(r.SessionCwd, r.Root)
	cmd.Env = filteredEnv(r.ExtraEnv)
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
	// agent.StartCommand wraps cmd.Start so an operator Ctrl-C during
	// the headless turn surfaces as a non-nil waitErr (ErrInterrupted)
	// instead of a clean exit. The context's timeout-kill still wins on
	// deadline because that returns a non-nil process error, which
	// StartCommand preserves verbatim.
	ac, err := agent.StartCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("claude: -p start: %w", err)
	}
	sidCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeOneShotProgress(pipe, stdout, r.Root, sidCh)
	}()
	waitErr := ac.Wait()
	<-done
	close(sidCh)
	// Shared post-Wait tail: drain the sid, mirror the transcript, map
	// the exit to a timeout or the raw waitErr. Routed through the agent
	// package so claude and codex can't diverge on the timeout path.
	timedOut := waitErr != nil && r.Timeout > 0 && ctx.Err() == context.DeadlineExceeded
	return agent.FinishOneShot(sidCh, r, timedOut, waitErr, "claude: -p", CopyTranscript)
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
	canonical := canonicalTranscriptPath(cwd, sessionID)
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

// RestoreTranscript runs after a TranscriptExists miss. Two fallbacks
// in order: glob every <CLAUDE_CONFIG_DIR>/projects/*/<sessionID>.jsonl
// for a stray copy under an old encoded-cwd bucket (typical when the
// session was opened before the stable-cwd switch landed and its JSONL
// lives under a worktree-UUID dir); if that misses, fall back to the
// bureaucracy-side mirror at mirrorPath. Both branches copy with a
// top-level `cwd` rewrite so claude --resume can read the file from
// the new canonical path without complaining about mismatched cwd.
// Source files are left in place (copy, don't move) so a crash between
// copy and resume can't lose history.
func (Agent) RestoreTranscript(sessionID, cwd, mirrorPath string) (agent.RestoreOutcome, error) {
	// Cache glob first — the original is the most fidelity-preserving
	// recovery and avoids the mirror's once-per-turn snapshot lag.
	src, err := transcriptPath(sessionID)
	if err != nil {
		return agent.RestoreOutcome{}, err
	}
	if src != "" {
		// Skip rewrite when the cached copy is already the canonical
		// path (defensive — TranscriptExists should have caught it,
		// but a race between probe and restore could land here).
		if canonical := canonicalTranscriptPath(cwd, sessionID); canonical != "" && src == canonical {
			return agent.RestoreOutcome{Result: agent.RestoreNotNeeded}, nil
		}
		oldDir, err := RestoreFromCache(src, cwd, sessionID)
		if err != nil {
			return agent.RestoreOutcome{}, err
		}
		return agent.RestoreOutcome{Result: agent.RestoreFromCache, Source: oldDir}, nil
	}
	// No cache hit — try the bureaucracy-side mirror.
	restored, err := RestoreFromMirror(mirrorPath, cwd, sessionID)
	if err != nil {
		return agent.RestoreOutcome{}, err
	}
	if restored {
		return agent.RestoreOutcome{Result: agent.RestoreFromMirror, Source: mirrorPath}, nil
	}
	return agent.RestoreOutcome{Result: agent.RestoreMissing}, nil
}

// Compile-time check that Agent satisfies the interface.
var _ agent.Agent = Agent{}
