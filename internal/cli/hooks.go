package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/banner"
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
	dirRel := filepath.Join(project.Dir(env.Project), "hooks", string(event)+".d")
	dir := filepath.Join(root, dirRel)
	// listExecutables filters dotfiles + non-executables upfront so the
	// section header can name an accurate count. The walker shares the
	// "missing dir / empty dir → silent no-op" contract pre-push relies
	// on for projects with no hooks installed.
	scripts, err := listExecutables(dir)
	if err != nil {
		return fmt.Errorf("hooks: list %s: %w", dir, err)
	}
	if len(scripts) == 0 {
		return nil
	}
	banner.HookSection(stdout, string(event)+" hooks", len(scripts), dirRel)

	for _, name := range scripts {
		path := filepath.Join(dir, name)
		rel := filepath.Join(dirRel, name)
		banner.HookStart(stdout, name)
		start := time.Now()

		// Capture stdout + stderr verbatim into a buffer for the
		// chain-back kickoff. Operators also see the raw stream live
		// — pre-push scripts emit human status lines on both streams
		// and the failure-time output is the same text they just read.
		var captured strings.Builder
		cmd := exec.Command(path)
		cmd.Dir = env.Sandbox
		cmd.Env = append(os.Environ(), env.envVars()...)
		cmd.Stdout = io.MultiWriter(&captured, stdout)
		cmd.Stderr = io.MultiWriter(&captured, stderr)
		runErr := cmd.Run()
		banner.HookDone(stdout, name, time.Since(start))
		if runErr != nil {
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
// see openCodeSessionForRebaseConflict. The second return is a
// *PushDeferredError marking the deferral so the cascade renders
// "deferred to recovery" instead of mistaking the recovery's clean
// exit for a successful ship.
//
// Overridable in tests; the default invokes runStageSession with
// docID="code", same as `moe <wf> code` would.
var openCodeSessionForHookFailure = func(md *run.Metadata, fail *hookFailure, stdout, stderr io.Writer) (int, error) {
	moePrintln(stderr, "       opening a fresh code session — fix the hook failure and commit; the chain prompt will offer push next")
	kickoff := buildHookFailureKickoff(md.Workflow, fail)
	code := runStageSession(md.Project, md.ID, "code", stageSessionOpts{
		NeedsSandbox:  true,
		InitialPrompt: kickoff,
	}, stdout, stderr)
	return code, &PushDeferredError{
		Recovery: "hook-failure",
		Project:  md.Project,
		Run:      md.ID,
	}
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
