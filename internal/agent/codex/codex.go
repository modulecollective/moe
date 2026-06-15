// Package codex is the agent.Agent implementation for the OpenAI
// Codex CLI (`codex`). It owns two subprocess shapes — interactive
// (`codex`) and one-shot streaming (`codex exec --json`) — plus the
// per-session rollout file glob that mirrors codex's transcripts into
// the document directory.
//
// Two structural differences from claude shape the implementation:
//
//   - Codex generates session ids itself and does not (yet) accept a
//     caller-supplied id. The first turn runs without `--resume`;
//     subsequent turns pass `codex resume <sid>` (interactive) or
//     `codex exec resume <sid>` (one-shot). The first turn reads the
//     id back from the `thread.started` JSON event (one-shot, verified
//     in 0.130.0) or from the rollout file's name suffix (interactive
//     — TUI has no stdout stream to read). Callers persist the returned
//     id when it differs from what they passed in.
//
//   - Codex has no `--ask-for-approval` on `codex exec`. We pin the
//     one-shot approval policy explicitly with `-c approval_policy=never`
//     so a non-`never` policy in `~/.codex/config.toml` can't abort the
//     turn at the approval gate (the symptom: "patch rejected:
//     writing outside of the project; rejected by user approval
//     settings"). The sandbox stays on — `workspace-write` plus the
//     bureaucracy-root `--add-dir` is still what bounds writes; this
//     change only removes the human-in-the-loop expectation that the
//     one-shot path can't satisfy. On interactive `codex` and on `codex
//     resume`, we pass `--ask-for-approval never` so MoE-managed
//     interactive Codex has the same approval posture while keeping the
//     same sandbox boundary.
//
// System prompt injection uses `-c developer_instructions="""<prompt>"""`.
// The triple-quoted TOML multi-line form sidesteps the
// "prose-accidentally-parses-as-TOML" risk the design names: the parser
// always takes the string path. Escape `"""` (if present) and any
// backslashes so the TOML reader returns the prompt verbatim.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

func init() {
	agent.Register("codex", Agent{})
}

// scrubbedKeys names the env vars stripped from every codex
// subprocess's environment — both the inherited os.Environ() and the
// dev-env-injected extra. OPENAI_API_KEY is auto-read by codex at
// startup and overrides the ChatGPT-plan OAuth token an operator signed
// in with, silently switching the agent to per-token API billing. A
// project whose dev-env emits the key must not be able to flip billing
// either, so the scrub spans extra as well as inheritance.
var scrubbedKeys = []string{
	"OPENAI_API_KEY",
}

// noEditorEnv pins git's editors to `true` (the no-op shell builtin
// that exits 0) for every codex subprocess. Without it, git falls back
// to vim for the rare editor-spawning operation — `git rebase
// --continue` finalizing a rebase, `git commit` with no `-m`, an
// interactive-rebase todo list — and vim hangs with no TTY in a
// headless turn, leaving the clone wedged mid-rebase with the
// resolution staged-but-uncommitted (run codex-rebase-weirdness). With
// `true`, git proceeds non-interactively using the preserved/default
// message, so the agent's bare `git rebase --continue` finalizes the
// step. Applied to interactive and headless turns alike: the operator
// decision is that MoE-managed codex never pops an editor.
var noEditorEnv = []string{
	"GIT_EDITOR=true",
	"GIT_SEQUENCE_EDITOR=true",
}

// filteredEnv returns os.Environ() with scrubbedKeys removed, then
// appends the no-editor pins and extra — extra also scrubbed. Same
// shape as the claude backend's helper; kept per-package so the drop
// list lives next to the backend that owns it. noEditorEnv is appended
// after the inherited environment so it overrides any inherited
// GIT_EDITOR. extra (the caller's ExtraEnv) stays last so dev-env vars
// win — except the scrubbed keys, which a project's dev-env must not be
// able to re-introduce to flip the agent's billing to metered API.
func filteredEnv(extra []string) []string {
	out := dropScrubbed(os.Environ())
	out = append(out, noEditorEnv...)
	return append(out, dropScrubbed(extra)...)
}

// dropScrubbed returns in with any entry whose key is in scrubbedKeys
// removed, preserving order.
func dropScrubbed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, kv := range in {
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
	return out
}

// Agent is the codex implementation of agent.Agent. Stateless — same
// shape as claude.Agent.
type Agent struct{}

// Execute drives an interactive `codex` (or `codex resume <sid>`) turn
// with stdio wired to the operator's terminal. Returns the session id
// the agent used: when r.NewSession is true, this is discovered from
// the rollout file written during the turn (codex's id, not the one
// passed in); otherwise it's r.SessionID echoed back.
//
// The interactive TUI has no JSON stdout to read, so first-turn id
// readback uses the rollout-file glob: list
// `~/.codex/sessions/<today>/rollout-*.jsonl` for files mtime'd
// during the turn window, pick the newest, parse the `<uuid>` suffix
// out of its filename.
func (Agent) Execute(r agent.Request) (string, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return r.SessionID, fmt.Errorf("codex: CLI not found on PATH: %w", err)
	}

	cmdArgs := executeArgs(r)

	cmd := exec.Command(bin, cmdArgs...)
	// cwd-inversion shape: codex always runs cwd = r.Root (the
	// bureaucracy session worktree). Code-bearing stages reach the
	// project clone via --add-dir; document-only stages have no clone
	// and write the canvas directly under root. Claude diverges and
	// runs cwd = r.SessionCwd because it encodes cwd into its on-disk
	// session bucket and needs a stable cwd for `--resume`; codex
	// keeps cwd=root because apply_patch enforces a project-scope
	// check and rollouts are date-sharded so resume doesn't depend
	// on cwd.
	cmd.Dir = r.Root
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

	turnStart := time.Now()
	// Route through agent.StartCommand so an operator Ctrl-C while
	// codex is running becomes a non-nil runErr (ErrInterrupted) rather
	// than a clean-looking exit; non-zero codex exits keep their
	// *exec.ExitError shape.
	var runErr error
	ac, startErr := agent.StartCommand(cmd)
	if startErr != nil {
		runErr = startErr
	} else {
		runErr = ac.Wait()
	}

	// First-turn id discovery: glob rollout files created since
	// turnStart, take the newest, parse its <uuid> suffix. On miss
	// (codex was killed before writing turn 1) keep r.SessionID so
	// stage.go's pre-flight will catch the absent transcript and
	// re-mint on the next turn.
	sid := r.SessionID
	if r.NewSession {
		if found := discoverSessionID(turnStart); found != "" {
			sid = found
		}
	}

	// Transcript copy is best-effort, same shape as claude's path.
	// Codex always writes to its own agent-tagged file.
	if r.Metadata != nil && sid != "" {
		threadPath := filepath.Join(r.Root, run.ThreadPathFor("codex", r.Metadata.Project, r.Metadata.ID, r.DocID))
		if _, err := CopyTranscript(sid, threadPath); err != nil && r.Stderr != nil {
			fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
		}
	}
	return sid, runErr
}

// ExecuteOneShot runs `codex exec --json` non-interactively, translates
// the JSON event stream into one-line-per-tool progress on r.Stdout,
// and returns the session id read from the `thread.started` event.
// `codex exec` has no `--ask-for-approval` flag, so the one-shot
// approval policy is pinned with `-c approval_policy=never` (see the
// package doc).
func (Agent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("codex: CLI not found on PATH: %w", err)
	}

	cmdArgs := executeOneShotArgs(r)

	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	// Same cwd rule as Execute: always r.Root. See the comment there.
	cmd.Dir = r.Root
	cmd.Env = filteredEnv(r.ExtraEnv)
	// Codex blocks on stdin when invoked non-interactively with a
	// non-tty stdin; explicitly close it.
	cmd.Stdin = nil
	stdout := r.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	cmd.Stderr = r.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("codex: exec stdout pipe: %w", err)
	}
	// agent.StartCommand wraps cmd.Start so an operator Ctrl-C during
	// the headless turn surfaces as a non-nil waitErr (ErrInterrupted)
	// instead of a clean exit. Context timeout-kills still win on
	// deadline because that returns a non-nil process error, which
	// StartCommand preserves verbatim.
	ac, err := agent.StartCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("codex: exec start: %w", err)
	}
	sidCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeExecProgress(pipe, stdout, r.Root, sidCh)
	}()
	waitErr := ac.Wait()
	<-done
	close(sidCh)
	// Shared post-Wait tail: drain the sid, mirror the transcript, map
	// the exit to a timeout or the raw waitErr. Routed through the agent
	// package so codex matches claude on the timeout path — it used to
	// return "" with no mirror on a deadline kill, losing the auto-tail
	// render and the resumable session.
	timedOut := waitErr != nil && r.Timeout > 0 && ctx.Err() == context.DeadlineExceeded
	return agent.FinishOneShot(sidCh, r, timedOut, waitErr, "codex: exec", CopyTranscript)
}

// CopyTranscript globs `~/.codex/sessions/*/*/*/rollout-*-<sid>.jsonl`
// and copies the first match (UUIDs are unique, so there's at most
// one) to dest. Returns (found, err): false found means "no rollout
// yet" — the legitimate no-op state.
func (Agent) CopyTranscript(sessionID, dest string) (bool, error) {
	return CopyTranscript(sessionID, dest)
}

// TranscriptExists reports whether a rollout file with the suffix
// `-<sessionID>.jsonl` exists anywhere under ~/.codex/sessions/. cwd
// is ignored: codex's rollout layout is date-sharded, not cwd-keyed,
// so the same id resolves identically regardless of where the agent
// ran from.
func (Agent) TranscriptExists(sessionID, _ string) (bool, error) {
	path, err := rolloutPath(sessionID)
	if err != nil {
		return false, err
	}
	return path != "", nil
}

// RestoreTranscript is a no-op for codex: TranscriptExists already
// globs every date shard for the rollout file, so a miss there means
// the rollout truly isn't on disk and there's nowhere else for codex
// to look. The mirror-restore path that claude needs doesn't apply —
// codex won't accept a hand-staged rollout without a matching session
// id of its own choosing. Stage.go treats RestoreMissing the same as
// the pre-Option-A re-mint behaviour.
func (Agent) RestoreTranscript(_, _, _ string) (agent.RestoreOutcome, error) {
	return agent.RestoreOutcome{Result: agent.RestoreMissing}, nil
}

// CopyTranscript is the package-level form used both by Agent's
// CopyTranscript method and by stage.go's per-agent thread mirror.
func CopyTranscript(sessionID, dest string) (bool, error) {
	src, err := rolloutPath(sessionID)
	if err != nil {
		return false, err
	}
	if src == "" {
		return false, nil
	}
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("codex: open rollout: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, fmt.Errorf("codex: mkdir thread dir: %w", err)
	}
	out, err := os.Create(dest)
	if err != nil {
		return false, fmt.Errorf("codex: create thread file: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, fmt.Errorf("codex: copy rollout: %w", err)
	}
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("codex: close thread file: %w", err)
	}
	return true, nil
}

// sessionsDir returns the effective codex sessions root —
// $CODEX_HOME/sessions when set, else ~/.codex/sessions. Empty when
// neither is available.
func sessionsDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// rolloutPath returns the rollout file path whose filename suffix
// matches the given session id, or "" with nil err when no match
// exists. The glob is intentionally broad — codex shards rollouts by
// `YYYY/MM/DD` under sessions/; the session id alone disambiguates.
func rolloutPath(sessionID string) (string, error) {
	root := sessionsDir()
	if root == "" {
		return "", nil
	}
	pattern := filepath.Join(root, "*", "*", "*", "rollout-*-"+sessionID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("codex: glob rollouts: %w", err)
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// executeArgs builds the full codex argv for the interactive path —
// `codex …` for a new session, `codex resume <sid> …` for a returning
// one. Kept separate from Execute so the argument shape (and its
// dependence on AddDirs) is unit-testable without shelling out.
func executeArgs(r agent.Request) []string {
	args := commonArgs(r.ClonePath, r.Prompt)
	// Stage-provided AddDirs (dev-env MOE_HOME / MOE_DEV_TMPDIR) widen
	// the writable scope alongside the clone path commonArgs passes.
	// Loop shape mirrors executeOneShotArgs's so the two call sites
	// stay structurally identical.
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// Interactive mode uses the same approval posture as the one-shot
	// path. The sandbox and add-dir set remain the write boundary;
	// failures return to the model/operator instead of asking for
	// approval.
	args = append(args, "--ask-for-approval", "never")
	if !r.NewSession {
		// Subsequent turn: `codex resume <sid> [prompt]`. The session
		// id was stored on r.SessionID after the first turn returned
		// what codex generated.
		args = append([]string{"resume", r.SessionID}, args...)
	}
	if r.InitialPrompt != "" {
		args = append(args, r.InitialPrompt)
	}
	return args
}

// executeOneShotArgs builds the full codex argv for the non-interactive
// streaming path (`codex exec --json …`). Same testability rationale
// as executeArgs.
func executeOneShotArgs(r agent.OneShotRequest) []string {
	args := commonArgs(r.ClonePath, r.Prompt)
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// Pin approval to "never": `codex exec` has no `--ask-for-approval`
	// flag, so a non-`never` `approval_policy` in `~/.codex/config.toml`
	// would abort the turn at the approval gate. The sandbox still
	// enforces add-dirs.
	args = append(args, "-c", "approval_policy=never")
	cmdArgs := append([]string{"exec", "--json", "--skip-git-repo-check"}, args...)
	cmdArgs = append(cmdArgs, r.UserPrompt)
	return cmdArgs
}

// commonArgs builds the codex flag set shared across exec / interactive
// / resume: developer-instructions injection, sandbox mode, and the
// per-stage clone add-dir.
//
// cwd-inversion shape: every stage runs cwd = bureaucracy worktree,
// which `--sandbox workspace-write` makes writable automatically — no
// explicit `--add-dir <root>` needed. Code-bearing stages reach the
// project clone via add-dir while cwd sits on the bureaucracy worktree;
// AGENTS.md discovery walks from cwd up to the git root, so
// project-specific AGENTS.md under the clone is no longer auto-loaded
// and is handled separately (system prompt or symlink) where needed.
// Document-only stages have an empty clonePath; cwd alone gives them
// the canvas's writable surface.
func commonArgs(clonePath, systemPrompt string) []string {
	args := []string{
		"-c", "developer_instructions=" + tomlMultilineBasic(systemPrompt),
	}
	// Sandbox: workspace-write keeps writes scoped to cwd + the
	// explicit add-dir set. read-only would block the canvas write
	// every stage needs.
	args = append(args, "--sandbox", "workspace-write")
	// Interactive codex applies a stricter sandbox than `codex exec`
	// for the project's `.git/` subtree — even with `--add-dir <clone>`,
	// writes to `<clone>/.git/index.lock` fail EROFS during commit
	// (verified across approval form, trust state, and stripped user
	// config — the divergence is internal to codex). Selecting the
	// `workspace-git` permissions profile re-grants `.git` writes
	// inside project roots. The profile lives in the operator's
	// `~/.codex/config.toml`; see docs/reference.md §"Codex Setup".
	// `codex exec` doesn't need the override, but applying it
	// uniformly keeps the operator-side config single-shape.
	args = append(args, "-c", "default_permissions=workspace-git")
	if clonePath != "" {
		args = append(args, "--add-dir", clonePath)
	}
	return args
}

// tomlMultilineBasic encodes s as a TOML multi-line basic string
// (`"""..."""`). Escapes the only sequences TOML treats specially in
// that context: backslashes (which start escape sequences) and
// literal `"""` (which ends the string). All other content — newlines,
// tabs, UTF-8 — passes through verbatim. This sidesteps the
// "prose-accidentally-parses-as-TOML" risk by always taking the
// string-parse path.
func tomlMultilineBasic(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"""`, `\"""`)
	return `"""` + s + `"""`
}

// discoverSessionID picks the newest rollout file modified at or after
// since (the turn-start timestamp), parses its session uuid suffix,
// and returns it. Returns "" when no fresh rollout exists — caller
// keeps its prior id and lets the next turn's pre-flight catch the
// missing transcript.
func discoverSessionID(since time.Time) string {
	root := sessionsDir()
	if root == "" {
		return ""
	}
	// Glob today and yesterday — a turn that straddles UTC midnight
	// would otherwise miss its own rollout. Costlier globs (full
	// month) buy nothing in this two-window case.
	today := time.Now().UTC()
	candidates := []string{
		filepath.Join(root, today.Format("2006"), today.Format("01"), today.Format("02"), "rollout-*.jsonl"),
		filepath.Join(root, today.Add(-24*time.Hour).Format("2006"), today.Add(-24*time.Hour).Format("01"), today.Add(-24*time.Hour).Format("02"), "rollout-*.jsonl"),
	}
	var newestPath string
	var newestMtime time.Time
	for _, pat := range candidates {
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			if info.ModTime().After(newestMtime) {
				newestPath = m
				newestMtime = info.ModTime()
			}
		}
	}
	if newestPath == "" {
		return ""
	}
	return parseSessionIDFromFilename(newestPath)
}

// parseSessionIDFromFilename extracts the trailing UUID from a codex
// rollout filename. Format: `rollout-<ISO-timestamp>-<uuid>.jsonl`.
// The uuid is the last hyphen-delimited segment of the basename
// (minus extension); a UUID contains four hyphens of its own (3+1
// versioning), so we count back from the end rather than split on
// "-".
func parseSessionIDFromFilename(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	parts := strings.Split(base, "-")
	const uuidParts = 5
	if len(parts) < uuidParts {
		return ""
	}
	return strings.Join(parts[len(parts)-uuidParts:], "-")
}

// pipeExecProgress reads codex's `--json` event stream and writes
// one-line-per-tool progress to w, mirroring claude's
// pipeOneShotProgress shape. The session id (from `thread.started`)
// is sent on sidCh exactly once; subsequent thread.started events
// (codex doesn't currently emit more than one per session) are
// dropped on a full channel.
func pipeExecProgress(r io.Reader, w io.Writer, trimRoot string, sidCh chan<- string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				select {
				case sidCh <- ev.ThreadID:
				default:
				}
			}
		case "item.started":
			if ev.Item == nil || ev.Item.Type != "command_execution" {
				continue
			}
			fmt.Fprintf(w, "> %s\n", renderCommandExecution(ev.Item, trimRoot))
		}
	}
}

// codexEvent is the minimum subset of `codex exec --json` we read.
// The full event vocabulary is thread.started / turn.started /
// item.started / item.completed / turn.completed; we render the
// command_execution items and pluck thread_id from thread.started.
type codexEvent struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id,omitempty"`
	Item     *codexItem `json:"item,omitempty"`
}

type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`
}

// renderCommandExecution turns a command_execution item into a short
// progress line. Codex tool calls are nearly always
// `/bin/bash -lc <cmd>`, so we strip that wrapper to keep the line
// readable. Trim absolute paths under trimRoot so the output stays
// short on a deep bureaucracy clone.
func renderCommandExecution(it *codexItem, trimRoot string) string {
	cmd := it.Command
	for _, prefix := range []string{`/bin/bash -lc "`, `/bin/bash -lc `, `bash -lc "`, `bash -lc `} {
		if strings.HasPrefix(cmd, prefix) {
			cmd = strings.TrimPrefix(cmd, prefix)
			cmd = strings.TrimSuffix(cmd, `"`)
			break
		}
	}
	if trimRoot != "" {
		cmd = strings.ReplaceAll(cmd, trimRoot+"/", "")
	}
	if len(cmd) > 80 {
		cmd = cmd[:79] + "…"
	}
	return "bash: " + cmd
}
