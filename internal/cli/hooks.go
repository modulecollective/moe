package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// hookEvent is the named gate point a hook fires at. Declaring the
// type keeps the call sites explicit instead of stringly-typed; today
// only pre-push is wired up.
type hookEvent string

const hookEventPrePush hookEvent = "pre-push"

// hookFailure is the dispatcher's "a hook said no" error. Project
// scripts emit this on non-zero exit; the chain-back uses event +
// script + output to build the kickoff prompt.
type hookFailure struct {
	event  hookEvent
	script string
	output string
}

func (e *hookFailure) Error() string {
	return fmt.Sprintf("hooks: %s/%s failed", e.event, e.script)
}

// hookEnv carries the run context exposed to a hook. The dispatcher
// exports the MOE_* vars to project scripts and passes the struct
// directly to built-ins.
type hookEnv struct {
	Project      string
	Run          string
	Document     string
	Workflow     string
	Sandbox      string
	Bureaucracy  string
	TargetBranch string
}

func (e hookEnv) envVars() []string {
	return []string{
		"MOE_PROJECT=" + e.Project,
		"MOE_RUN=" + e.Run,
		"MOE_DOCUMENT=" + e.Document,
		"MOE_WORKFLOW=" + e.Workflow,
		"MOE_SANDBOX=" + e.Sandbox,
		"MOE_BUREAUCRACY=" + e.Bureaucracy,
		"MOE_TARGET_BRANCH=" + e.TargetBranch,
	}
}

// builtinHook is a Go-side gate registered against a hook event.
// Built-ins run after project scripts. Returning a non-nil error
// halts the chain; richer error types (e.g. *rebaseConflictError)
// flow through unchanged so the caller can errors.As on them and
// drive a custom chain-back.
type builtinHook struct {
	Name string
	Run  func(env hookEnv, stdout, stderr io.Writer) error
}

var builtinHooks = map[hookEvent][]builtinHook{}

// registerBuiltinHook appends a built-in to the per-event chain.
// Called from init() in whichever file owns the built-in.
func registerBuiltinHook(event hookEvent, h builtinHook) {
	builtinHooks[event] = append(builtinHooks[event], h)
}

// runHooks dispatches the project drop-in directory for event, then
// the registered built-ins. Returns nil on success, *hookFailure when
// a project script exits non-zero, or whatever error type a built-in
// chooses to return. A missing or empty hooks dir is a no-op.
func runHooks(root string, event hookEvent, env hookEnv, stdout, stderr io.Writer) error {
	if err := runProjectHookScripts(root, event, env, stdout, stderr); err != nil {
		return err
	}
	for _, b := range builtinHooks[event] {
		if err := b.Run(env, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// runProjectHookScripts walks projects/<p>/hooks/<event>.d in lex
// order, executes each executable script with CWD = sandbox clone
// and the MOE_* env vars exported, and returns *hookFailure on the
// first non-zero exit.
func runProjectHookScripts(root string, event hookEvent, env hookEnv, stdout, stderr io.Writer) error {
	dir := filepath.Join(root, project.Dir(env.Project), "hooks", string(event)+".d")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hooks: read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Dotfiles (.gitkeep, editor backups, macOS ._foo metadata)
		// are convention-skipped, matching run-parts.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("hooks: stat %s: %w", path, err)
		}
		// Non-executable files are silently skipped: a script the
		// operator hasn't `chmod +x`'d yet is a half-installed hook,
		// not a hard error. run-parts behaves the same way.
		if info.Mode()&0o111 == 0 {
			continue
		}

		rel := filepath.Join(project.Dir(env.Project), "hooks", string(event)+".d", name)
		moePrintf(stdout, "running %s hook %s...\n", event, rel)

		var captured strings.Builder
		cmd := exec.Command(path)
		cmd.Dir = env.Sandbox
		cmd.Env = append(os.Environ(), env.envVars()...)
		cmd.Stdout = io.MultiWriter(&captured, stdout)
		cmd.Stderr = io.MultiWriter(&captured, stderr)
		if err := cmd.Run(); err != nil {
			return &hookFailure{
				event:  event,
				script: rel,
				output: captured.String(),
			}
		}
	}
	return nil
}

// openCodeSessionForHookFailure is the generic chain-back for project
// hook failures: spawn a fresh code session against the same run with
// a kickoff that names the failing event + script and dumps the
// captured output verbatim, then exit non-zero so the operator knows
// to re-run push after the agent commits the fix. Built-ins with
// richer semantics (the rebase check) keep their own chain-back —
// see openCodeSessionForRebaseConflict.
//
// Overridable in tests; the default invokes runStageSession with
// docID="code", same as `moe wf <wf> code` would.
var openCodeSessionForHookFailure = func(md *run.Metadata, fail *hookFailure, stdout, stderr io.Writer) int {
	moePrintln(stderr, "       opening a fresh code session — fix the hook failure, commit, then re-run push")
	kickoff := buildHookFailureKickoff(fail)
	_ = runStageSession(md.Project, md.ID, "code", stageSessionOpts{
		NeedsSandbox:  true,
		InitialPrompt: kickoff,
		// SkipNextStage so the post-turn prompt doesn't offer to chain
		// straight into push — the operator re-runs push by hand once
		// the fix is committed.
		SkipNextStage: true,
	}, stdout, stderr)
	return 1
}

// buildHookFailureKickoff is the agent-facing kickoff for a generic
// hook failure. Names the event and script, dumps the captured output
// verbatim, and tells the agent what "done" looks like — fix, commit,
// exit so the operator can re-run push.
func buildHookFailureKickoff(f *hookFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe workflow ... push` ran the %s hook `%s` and it exited non-zero.\n\n", f.event, f.script)
	if strings.TrimSpace(f.output) != "" {
		b.WriteString("Output:\n\n")
		b.WriteString(f.output)
		if !strings.HasSuffix(f.output, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString("(the hook produced no output)\n\n")
	}
	b.WriteString("This needs to be fixed before we can continue. Investigate the failure, ")
	b.WriteString("apply the fix in the sandbox, commit, and exit the session. Then tell the ")
	b.WriteString("operator to re-run `moe wf <wf> push` to ship.\n")
	return b.String()
}
