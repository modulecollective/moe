// Package agent is the seam between the CLI's stage orchestration
// and the agent binary that actually drives a stage turn (claude or
// codex today). The two executor entry points (Execute,
// ExecuteOneShot) plus the transcript mirror sit behind Agent so
// stage.go doesn't need to know which binary is on the other side.
// Implementations register themselves via Register in init();
// callers look one up with Get.
//
// Request and OneShotRequest are kept here (not on the
// implementation side) so a future third backend can be added by
// dropping a sibling package next to claude/ and codex/ without
// touching any call site.
package agent

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// Agent runs one stage turn against a backend binary and mirrors its
// per-session transcript into the document directory. The two modes
// — interactive and one-shot streaming — exist because the stage and
// curation call sites have different requirements for stdio, session
// resume, and output handling.
//
// The interactive and one-shot methods return (sessionID, err): the
// claude implementation echoes the UUID it was passed; the codex
// implementation reads its session id back from the agent's
// per-session rollout file after the turn (codex generates the id
// itself; an upstream feature request to allow callers to pre-mint
// it is open). Callers persist the returned id when it differs from
// what they passed in.
//
// All methods are safe to call from one goroutine at a time;
// they do not assume concurrent invocation.
type Agent interface {
	// Execute is the interactive stage turn: stdio wired to the
	// operator's terminal, session resumes via the agent's native
	// resume mechanism. Returns the session id the agent used (echoed
	// for claude, discovered from the rollout file for codex).
	Execute(Request) (string, error)
	// ExecuteOneShot is the headless streaming path: no operator on
	// stdin, output translated to one-line-per-tool progress on the
	// caller's stdout. Returns the session id the same way Execute
	// does.
	ExecuteOneShot(OneShotRequest) (string, error)
	// CopyTranscript copies the agent's per-session JSONL into dest.
	// Returns (found, err): found=false means the agent has no
	// transcript for sessionID yet (a legitimate no-op state, e.g.
	// the operator aborted before the first turn wrote anything).
	CopyTranscript(sessionID, dest string) (bool, error)
	// TranscriptExists is the pre-flight probe stage.go uses before
	// passing --resume to a returning session: returns whether the
	// agent's per-session transcript for sessionID is present at the
	// path the agent will look for it under cwd. False with nil err
	// is the "transcript not found, re-mint and warn" branch.
	//
	// The cwd argument matters for claude (encoded into the project
	// dir under ~/.claude/projects/) and is ignored by codex (rollout
	// files live under ~/.codex/sessions/<date>/, keyed only by
	// session id).
	TranscriptExists(sessionID, cwd string) (bool, error)
	// RestoreTranscript is the recovery hook stage.go calls after a
	// TranscriptExists miss but before re-minting the session id. The
	// agent looks for the transcript anywhere it might still live
	// (other encoded-cwd buckets, the bureaucracy-side mirror) and
	// stages it where `--resume sessionID` will find it from cwd.
	// Returns a RestoreOutcome describing which path was taken so
	// stage.go can emit the operator-facing stderr line. The mirrorPath
	// is the bureaucracy-relative thread-<agent>.jsonl the agent should
	// fall back to when no agent-cached copy survives; empty disables
	// the mirror branch (run-less sessions).
	//
	// Codex's rollout layout is date-sharded, not cwd-keyed, so
	// TranscriptExists is authoritative; codex returns RestoreMissing
	// here as a no-op. Claude's impl does the real work.
	RestoreTranscript(sessionID, cwd, mirrorPath string) (RestoreOutcome, error)
}

// RestoreResult names which recovery branch RestoreTranscript took.
// Stage.go uses it to pick the appropriate operator-facing stderr line
// and to decide whether to re-mint the session id.
type RestoreResult int

const (
	// RestoreNotNeeded means the transcript is already at the canonical
	// path TranscriptExists would have found — defensive return when
	// the caller skipped the pre-flight. No log line; resume normally.
	RestoreNotNeeded RestoreResult = iota
	// RestoreFromCache means the agent found the transcript in a
	// non-canonical bucket under its on-disk cache (claude's case: a
	// pre-Option-B encoded-cwd dir from an old worktree path) and
	// copied it into place. The original is left for `moe claude-cache
	// gc` to reap. Source carries the old bucket's dirname.
	RestoreFromCache
	// RestoreFromMirror means the agent restored the transcript from
	// the bureaucracy-side mirror (thread-<agent>.jsonl) — the
	// cross-machine / cache-wipe path. Source carries the mirror file
	// path (already operator-friendly).
	RestoreFromMirror
	// RestoreMissing means no transcript exists anywhere the agent
	// knows to look. Stage.go re-mints the session id and proceeds as
	// a true fresh start.
	RestoreMissing
)

// RestoreOutcome is the result of a RestoreTranscript call. Result
// names the branch; Source is the operator-readable identifier for
// the path that was used (old encoded-dir for cache restores, mirror
// path for mirror restores, empty otherwise). Stage.go reads both to
// compose the stderr line.
type RestoreOutcome struct {
	Result RestoreResult
	Source string
}

// Request is the inputs for one interactive turn on one document.
// All path / id fields are populated by stage.go from the resolved
// run metadata.
type Request struct {
	// Root is the bureaucracy repo root. Passed to the agent as a
	// writable add-dir so the canvas stays reachable when cwd is the
	// sandbox clone.
	Root string
	// Metadata is the run's on-disk state. Read-only. Optional —
	// run-less sessions (e.g. wiki lint) pass nil, which skips
	// transcript mirroring at turn end since there is no document
	// thread file to copy into.
	Metadata *run.Metadata
	// DocID is which document on the run this turn is for. Ignored
	// when Metadata is nil.
	DocID string
	// SessionID is the canonical UUID that identifies this document's
	// conversation. Claude uses it directly (--session-id /
	// --resume); codex generates its own and the agent reads it back.
	SessionID string
	// NewSession is true when SessionID was just minted this turn
	// (first ever call for this document) and false when it already
	// has a server-side session that should be resumed.
	NewSession bool
	// Prompt is the assembled system prompt from buildSystemPrompt.
	Prompt string
	// ClonePath is the private per-run sandbox clone of the target
	// project's submodule, or "" for document-only runs. When set,
	// the agent runs with this as its working directory.
	ClonePath string
	// SessionCwd is the stable per-document cwd for claude turns: a path
	// under <root>/.moe/sessions/<p>/<r>/<d> that's identical across
	// turns, so claude's encoded-cwd project dir under
	// ~/.claude/projects/ doesn't churn and `--resume <sid>` finds the
	// JSONL it wrote on turn 1. Used by every claude stage now —
	// code-bearing stages reach the sandbox clone via --add-dir rather
	// than via cwd. Empty for run-less callers (rebase_resolve) which
	// have no session to resume; the executor falls back to Root.
	// Codex ignores this field — its rollout files are date-sharded,
	// not cwd-indexed.
	SessionCwd string
	// InitialPrompt, if non-empty, is auto-sent as the first user message
	// of the turn so the operator doesn't have to type anything to kick
	// the session off.
	InitialPrompt string
	// Stdin / Stdout / Stderr wire the interactive agent to the
	// operator's terminal or capture output in tests.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// ExtraEnv is appended to os.Environ() before the agent subprocess
	// is spawned. Stage callers use it to inject the dev-env hooks'
	// parsed output (DATABASE_URL, MOE_HOME, etc.) so the agent's
	// shell tool calls run against the project's isolated runtime.
	ExtraEnv []string
	// AddDirs are extra read/write paths the agent backend should
	// expose alongside the bureaucracy root and (when set) the
	// sandbox clone. Stage callers populate this from
	// `devEnvWritableDirs(devEnv)` so the test-stage `moe` subprocess
	// can write to the isolated MOE_HOME / MOE_DEV_TMPDIR the dev-env
	// hook emitted. Empty for stages that don't open a working tree.
	AddDirs []string
}

// OneShotRequest drives a single non-interactive streaming turn: the
// agent's stdout is translated to one-line-per-tool progress and
// surfaced on the caller's stdout while the turn runs. No session
// resume, no operator on stdin.
type OneShotRequest struct {
	// Root is the bureaucracy repo root. Passed as a writable
	// add-dir so the canvas stays reachable when cwd is the sandbox
	// clone.
	Root string
	// Prompt is the assembled system prompt for this turn.
	Prompt string
	// UserPrompt is the single user turn for the headless run.
	UserPrompt string
	// ClonePath, when non-empty, is the per-run sandbox clone for
	// code-bearing stages. Reached via --add-dir; not used as cwd. Empty
	// for document-only stages.
	ClonePath string
	// SessionCwd is the stable per-document cwd for claude headless
	// turns — same shape and rationale as Request.SessionCwd. Empty for
	// run-less callers (rebase_resolve); the executor falls back to
	// Root. Codex ignores this field.
	SessionCwd string
	// Model, if non-empty, names the model to use. Empty defers to
	// the agent's configured default. Bounded curation tasks (push
	// synthesis) may pass a model override; full stage turns leave it
	// empty.
	Model string
	// Stdout streams agent output (progress translation) to the
	// operator's terminal.
	Stdout io.Writer
	// Stderr captures the agent's diagnostic output.
	Stderr io.Writer
	// Timeout, when > 0, hard-caps the whole invocation. Zero leaves
	// the call open-ended.
	Timeout time.Duration
	// ExtraEnv is appended to os.Environ() before the agent
	// subprocess is spawned — same shape as Request.ExtraEnv.
	ExtraEnv []string
	// AddDirs are extra read/write paths the agent backend should
	// expose. Same shape and contract as Request.AddDirs.
	AddDirs []string
	// ThreadPath, when non-empty, is the bureaucracy-relative
	// destination the agent should copy its per-session JSONL to
	// after the turn returns. The post-headless auto-tail in stage.go
	// reads it for the "what just happened?" render the operator sees
	// when a one-shot bails. Empty means skip the mirror (e.g. the
	// rebase-resolve fallback that has no run document to mirror
	// into). Best-effort: a copy failure surfaces on r.Stderr but
	// doesn't override the turn's exit status.
	ThreadPath string
}

var (
	mu       sync.RWMutex
	registry = map[string]Agent{}
)

// Register associates name with an Agent implementation. Called from
// the implementation package's init(). Panics on duplicate names —
// agent names are a tiny closed set today, so a collision is a
// programming error.
func Register(name string, a Agent) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic("agent: duplicate registration for " + name)
	}
	registry[name] = a
}

// Get returns the Agent registered under name. Unknown names return
// an error rather than silently falling back to a default; the
// caller (a CLI flag or env var) should surface the failure to the
// operator instead of running the wrong backend.
func Get(name string) (Agent, error) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("agent: unknown backend %q (registered: %v)", name, names())
	}
	return a, nil
}

// names returns the sorted list of registered agent names. Used for
// error-message context. Caller holds the read lock.
func names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
