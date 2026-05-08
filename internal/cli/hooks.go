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

// hookEventPrePush fires after the built-in rebase-onto-default check
// and before the actual `git push`. "Pre-push, post-rebase": project
// scripts see the tree that's about to be pushed, not the pre-rebase
// tree the operator was editing. That's load-bearing — running gofmt /
// vet / etc. against the pre-rebase tree is exactly how a concurrent
// edit to the same surface slips past local hooks and breaks CI.
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
// Built-ins run before project scripts so the scripts see the tree
// the rebase produced. Returning a non-nil error halts the chain;
// richer error types (e.g. *rebaseConflictError) flow through
// unchanged so the caller can errors.As on them and drive a custom
// chain-back.
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

// runHooks dispatches the registered built-ins for event, then the
// project drop-in directory. Built-ins run first so project scripts
// vet the post-rebase tree (the one about to be pushed) instead of
// the pre-rebase tree the operator was editing — see hookEventPrePush.
// Returns nil on success, *hookFailure when a project script exits
// non-zero, or whatever error type a built-in chooses to return. A
// missing or empty hooks dir is a no-op.
func runHooks(root string, event hookEvent, env hookEnv, stdout, stderr io.Writer) error {
	for _, b := range builtinHooks[event] {
		if err := b.Run(env, stdout, stderr); err != nil {
			return err
		}
	}
	return runProjectHookScripts(root, event, env, stdout, stderr)
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
// captured output verbatim, then propagate that session's exit code so
// a clean fix-and-commit lets the workflow's chain prompt offer push
// next — same shape `moe <wf> code` already produces. Built-ins with
// richer semantics (the rebase check) keep their own chain-back —
// see openCodeSessionForRebaseConflict.
//
// Overridable in tests; the default invokes runStageSession with
// docID="code", same as `moe <wf> code` would.
var openCodeSessionForHookFailure = func(md *run.Metadata, fail *hookFailure, stdout, stderr io.Writer) int {
	moePrintln(stderr, "       opening a fresh code session — fix the hook failure and commit; the chain prompt will offer push next")
	kickoff := buildHookFailureKickoff(md.Workflow, fail)
	return runStageSession(md.Project, md.ID, "code", stageSessionOpts{
		NeedsSandbox:  true,
		InitialPrompt: kickoff,
	}, stdout, stderr)
}

// buildHookFailureKickoff is the agent-facing kickoff for a generic
// hook failure. Names the event and script, dumps the captured output
// verbatim, and tells the agent what "done" looks like — fix, commit,
// exit; the post-turn chain prompt will offer push.
func buildHookFailureKickoff(workflow string, f *hookFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe %s push` ran the %s hook `%s` and it exited non-zero.\n\n", workflow, f.event, f.script)
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
	b.WriteString("apply the fix in the sandbox, commit, and exit the session. The post-turn ")
	fmt.Fprintf(&b, "chain prompt will offer `moe %s push` next.\n", workflow)
	return b.String()
}
