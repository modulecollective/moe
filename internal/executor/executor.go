// Package executor abstracts the backend that actually runs a `moe work`
// turn, so moe can host more than one: the interactive Claude Code CLI
// (today's only implementation) and, later, Anthropic's Managed Agents
// API for fire-and-forget async runs.
//
// The interface is small on purpose. A Request carries everything an
// executor needs to run one turn against one document; the executor
// owns the details of how the agent runs, what filesystem it sees, and
// where the turn's transcript ends up. `moe work` stays the same shape
// regardless of which executor it picks.
package executor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/termout"
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
// executor `moe work` used before the interface existed; behavior is
// unchanged.
type ClaudeCLI struct{}

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
	name := bin
	args := []string{
		sessionFlag, r.SessionID,
		"--add-dir", r.Root,
		"--append-system-prompt", r.Prompt,
	}
	// A positional prompt launches claude interactively but auto-sends
	// it as the first user message, so the operator lands in a session
	// that's already in motion.
	if r.InitialPrompt != "" {
		args = append(args, r.InitialPrompt)
	}

	// Wrap the claude subprocess with srt (Anthropic Sandbox Runtime)
	// for OS-level isolation — sandbox-exec on macOS, bubblewrap on
	// Linux, plus a domain-filtering proxy. srt inherits cwd and stdio.
	//
	// srt's default mode joins argv with a bare space and runs the
	// result through `sh -c` with no escaping, which mangles any arg
	// containing whitespace, newlines, or shell metacharacters (moe's
	// system prompt is multi-line). We pre-quote each arg and hand srt
	// a ready-made command string via its `-c` mode instead.
	srtBin, cfg, err := resolveSRT(r.Root, os.Getenv("MOE_SANDBOX"))
	if err != nil {
		return err
	}
	if srtBin != "" {
		name = srtBin
		args = []string{"--settings", cfg, "-c", shellJoin(append([]string{bin}, args...))}
		if r.Stderr != nil {
			termout.Printf(r.Stderr, "moe: sandboxing claude via srt (%s)\n", cfg)
		}
	}

	cmd := exec.Command(name, args...)
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

// resolveSRT decides whether to wrap claude with srt, based on the
// MOE_SANDBOX env var and whether srt is on PATH. A non-empty srtBin
// means "wrap"; an empty srtBin means "run claude direct." The decision
// table (from the design):
//
//	MOE_SANDBOX   srt installed   behaviour
//	unset         yes             wrap
//	unset         no              direct
//	off           either          direct
//	on / 1        yes             wrap
//	on / 1        no              error
//
// Any other value for MOE_SANDBOX is an error so typos like
// MOE_SANDBOX=false don't silently fall back to the default.
func resolveSRT(root, envVal string) (srtBin, cfg string, err error) {
	switch envVal {
	case "", "on", "1", "off":
	default:
		return "", "", fmt.Errorf("executor: invalid MOE_SANDBOX=%q (want \"on\", \"1\", \"off\", or unset)", envVal)
	}
	if envVal == "off" {
		return "", "", nil
	}
	bin, lookErr := exec.LookPath("srt")
	if lookErr != nil {
		if envVal == "on" || envVal == "1" {
			return "", "", fmt.Errorf("executor: MOE_SANDBOX=%s but srt not on PATH: %w", envVal, lookErr)
		}
		return "", "", nil
	}
	cfg, err = ensureSRTSettings(root)
	if err != nil {
		return "", "", err
	}
	return bin, cfg, nil
}

// ensureSRTSettings lazily writes <root>/.moe/srt-settings.json the
// first time sandboxing kicks in. The template is literal except for
// the <root> token, which is replaced with the absolute bureaucracy
// path so srt's allowWrite covers clones and transcripts. Mirrors
// ensureGitignore in internal/sandbox/sandbox.go.
func ensureSRTSettings(root string) (string, error) {
	dir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("executor: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, "srt-settings.json")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("executor: stat %s: %w", p, err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("executor: resolve %s: %w", root, err)
	}
	body := strings.ReplaceAll(srtSettingsTemplate, "<root>", abs)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("executor: write %s: %w", p, err)
	}
	return p, nil
}

// srtSettingsTemplate is the starter settings file written on first
// sandboxed run. The allowWrite and denyRead lists are the minimum that
// lets Claude do useful work on this repo (build Go, run tests, write
// the sandbox clone, persist its own session JSONL under ~/.claude)
// without handing it access to ~/.ssh, cloud creds, or browser
// profiles. allowedDomains is the minimum network set Claude needs for
// normal moe work — the operator is expected to extend it as real
// projects demand more. denyWrite and deniedDomains are required keys
// in srt's config schema; we ship them empty because allowWrite and
// allowedDomains are already restrictive.
const srtSettingsTemplate = `{
  "filesystem": {
    "allowWrite": [
      "<root>",
      "/tmp",
      "~/go/pkg/mod",
      "~/.cache/go-build",
      "~/Library/Caches/go-build",
      "~/.claude"
    ],
    "denyWrite": [],
    "denyRead": [
      "~/.ssh",
      "~/.aws",
      "~/.config/gh",
      "~/.gnupg",
      "~/Library/Application Support/Google/Chrome",
      "~/Library/Application Support/Firefox"
    ]
  },
  "network": {
    "allowedDomains": [
      "api.anthropic.com",
      "statsig.anthropic.com",
      "*.anthropic.com",
      "github.com",
      "*.github.com",
      "proxy.golang.org",
      "sum.golang.org",
      "registry.npmjs.org"
    ],
    "deniedDomains": []
  }
}
`

// shellJoin POSIX-quotes each arg and joins them with spaces, producing
// a command string safe to pass to `sh -c`. Used to work around srt's
// default-mode arg handling, which joins argv without escaping.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

// shellQuote returns s wrapped in single quotes with any embedded single
// quote escaped as '\''. Single-quote wrapping disables all shell
// interpretation (whitespace, $, backticks, \, newlines) so the arg
// survives `sh -c` unchanged.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '/', r == '.', r == '=', r == ':', r == ',', r == '@', r == '+':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
