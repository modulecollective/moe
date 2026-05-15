// Package codex is the agent.Agent implementation for the OpenAI
// Codex CLI (`codex`). It owns three subprocess shapes — interactive
// (`codex`), one-shot streaming (`codex exec --json`), and headless
// captured (`codex exec`) — plus the per-session rollout file glob
// that mirrors codex's transcripts into the document directory.
//
// Two structural differences from claude shape the implementation:
//
//   - Codex generates session ids itself and does not (yet) accept a
//     caller-supplied id. The first turn runs without `--resume`;
//     subsequent turns pass `codex resume <sid>` (interactive) or
//     `codex exec resume <sid>` (one-shot/headless). The first turn
//     reads the id back from the `thread.started` JSON event (one-shot,
//     verified in 0.130.0) or from the rollout file's name suffix
//     (interactive — TUI has no stdout stream to read). Callers persist
//     the returned id when it differs from what they passed in.
//
//   - Codex has no `--ask-for-approval` on `codex exec`. We pin the
//     headless approval policy explicitly with `-c approval_policy=never`
//     so a non-`never` policy in `~/.codex/config.toml` can't abort a
//     headless turn at the approval gate (the symptom: "patch rejected:
//     writing outside of the project; rejected by user approval
//     settings"). The sandbox stays on — `workspace-write` plus the
//     bureaucracy-root `--add-dir` is still what bounds writes; this
//     change only removes the human-in-the-loop expectation that
//     headless can't satisfy. On interactive `codex` and on `codex
//     resume`, we pass `--ask-for-approval on-request` (the codex
//     equivalent of claude's default permission flow).
//
// System prompt injection uses `-c developer_instructions="""<prompt>"""`.
// The triple-quoted TOML multi-line form sidesteps the
// "prose-accidentally-parses-as-TOML" risk the design names: the parser
// always takes the string path. Escape `"""` (if present) and any
// backslashes so the TOML reader returns the prompt verbatim.
package codex

import (
	"bufio"
	"bytes"
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

	cmdArgs, err := executeArgs(r)
	if err != nil {
		return r.SessionID, err
	}

	cmd := exec.Command(bin, cmdArgs...)
	cmd.Dir = resolveCwd(r.ClonePath, r.SessionCwd, r.Root)
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

	turnStart := time.Now()
	runErr := cmd.Run()

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
	// Codex doesn't get a legacy-rename pass: any pre-existing
	// `thread.jsonl` next to the document is claude's history, so it
	// belongs to claude's slot (the claude agent's first turn after
	// this commit will rename it). Codex always writes to its own
	// agent-tagged file.
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
// `codex exec` has no `--ask-for-approval` flag, so the headless
// approval policy is pinned with `-c approval_policy=never` (see the
// package doc).
func (Agent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("codex: CLI not found on PATH: %w", err)
	}

	cmdArgs, err := executeOneShotArgs(r)
	if err != nil {
		return "", err
	}

	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = resolveCwd(r.ClonePath, "", r.Root)
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
	}
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
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("codex: exec start: %w", err)
	}
	sidCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeExecProgress(pipe, stdout, r.Root, sidCh)
	}()
	waitErr := cmd.Wait()
	<-done
	close(sidCh)
	if waitErr != nil && r.Timeout > 0 && ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("codex: exec timed out after %s", r.Timeout)
	}
	// Drain whatever the translator captured. sidCh is buffered so the
	// translator non-blocks on send; we take the first id (codex emits
	// exactly one thread.started per session).
	var sid string
	select {
	case sid = <-sidCh:
	default:
	}
	return sid, waitErr
}

// ExecuteHeadless runs a one-shot `codex exec` and returns stdout as
// bytes. No session id is tracked (the curation calls this serves
// don't have one). Stderr streams through to the caller's
// r.Stderr if set.
func (Agent) ExecuteHeadless(r agent.HeadlessRequest) ([]byte, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex: CLI not found on PATH: %w", err)
	}

	args, err := commonArgs(r.WorkDir, r.WorkDir, r.SystemPrompt)
	if err != nil {
		return nil, err
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// Pin approval to "never": `codex exec` has no flag for it, so a
	// non-`never` `approval_policy` in `~/.codex/config.toml` would
	// abort headless turns at the approval gate. The sandbox still
	// enforces add-dirs.
	args = append(args, "-c", "approval_policy=never")

	cmdArgs := append([]string{"exec", "--skip-git-repo-check"}, args...)
	cmdArgs = append(cmdArgs, r.UserPrompt)

	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = r.WorkDir
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = r.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out.Bytes(), fmt.Errorf("codex: exec timed out after %s", r.Timeout)
		}
		return out.Bytes(), fmt.Errorf("codex: exec: %w", err)
	}
	return out.Bytes(), nil
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

// SessionsDir returns the effective codex sessions root —
// $CODEX_HOME/sessions when set, else ~/.codex/sessions. Empty when
// neither is available.
func SessionsDir() string {
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
	root := SessionsDir()
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
func executeArgs(r agent.Request) ([]string, error) {
	args, err := commonArgs(r.Root, r.ClonePath, r.Prompt)
	if err != nil {
		return nil, err
	}
	// Stage-provided AddDirs (dev-env MOE_HOME / MOE_DEV_TMPDIR) widen
	// the writable scope alongside the bureaucracy root commonArgs
	// passes. Loop shape mirrors ExecuteHeadless's so the three call
	// sites stay structurally identical.
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// Interactive mode: on-request approval is the codex equivalent of
	// claude's default permission flow. The operator can confirm or
	// deny each agent-proposed write/bash from the TUI.
	args = append(args, "--ask-for-approval", "on-request")
	if !r.NewSession {
		// Subsequent turn: `codex resume <sid> [prompt]`. The session
		// id was stored on r.SessionID after the first turn returned
		// what codex generated.
		args = append([]string{"resume", r.SessionID}, args...)
	}
	if r.InitialPrompt != "" {
		args = append(args, r.InitialPrompt)
	}
	return args, nil
}

// executeOneShotArgs builds the full codex argv for the non-interactive
// streaming path (`codex exec --json …`). Same testability rationale
// as executeArgs.
func executeOneShotArgs(r agent.OneShotRequest) ([]string, error) {
	args, err := commonArgs(r.Root, r.ClonePath, r.Prompt)
	if err != nil {
		return nil, err
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	// Pin approval to "never" — see ExecuteHeadless for the rationale.
	args = append(args, "-c", "approval_policy=never")
	cmdArgs := append([]string{"exec", "--json", "--skip-git-repo-check"}, args...)
	cmdArgs = append(cmdArgs, r.UserPrompt)
	return cmdArgs, nil
}

// commonArgs builds the codex flag set shared across exec / interactive
// / resume: developer-instructions injection, sandbox mode, add-dirs,
// and the cwd hint. Returns the flag slice ready to be prefixed with
// the subcommand (or used bare for interactive).
func commonArgs(root, clonePath, systemPrompt string) ([]string, error) {
	args := []string{
		"-c", "developer_instructions=" + tomlMultilineBasic(systemPrompt),
	}
	// Sandbox: workspace-write covers both code stages (cwd = sandbox
	// clone, which must be writable) and document-only stages (cwd =
	// bureaucracy worktree, also writable for canvas edits). read-only
	// would block the canvas write that doc stages need, so we keep
	// scoping to the cwd + add-dirs.
	args = append(args, "--sandbox", "workspace-write")
	// `--add-dir <bureaucracy-root>` keeps the canvas reachable from
	// the sandbox clone (code stages) or pins the bureaucracy
	// worktree as the writable scope (doc stages — clonePath empty).
	if root != "" {
		args = append(args, "--add-dir", root)
	}
	// Future: project-level AGENTS.md discovery walks from cwd up to
	// the git root, so the target submodule's AGENTS.md (if any)
	// loads automatically on code stages. No flag needed.
	_ = clonePath
	return args, nil
}

// resolveCwd mirrors the executor's cwd precedence: clone if a
// sandbox is attached, else session worktree for doc-only stages,
// else the bureaucracy root.
func resolveCwd(clonePath, sessionCwd, root string) string {
	switch {
	case clonePath != "":
		return clonePath
	case sessionCwd != "":
		return sessionCwd
	default:
		return root
	}
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
	root := SessionsDir()
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
