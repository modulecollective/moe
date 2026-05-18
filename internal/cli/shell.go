package cli

import (
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// runShell drops the operator into an interactive shell rooted at a
// run's working tree. The named-workspace shape lives under
// `moe workspace shell <project> <name>` — workspaces aren't
// sdlc-specific, so the verb sits with the rest of the workspace
// admin verbs. Single-operator, no-config-knobs: one verb per shape.
//
// The shell is whatever $SHELL says, defaulting to /bin/sh. Stdin /
// stdout / stderr are wired straight through, so the operator gets
// their normal prompt, history, completion, and signals; the verb
// exits with the shell's exit code.
func runShell(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc shell <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Drops into a shell rooted at the run's working tree (per-run sandbox")
		moePrintln(stderr, "or named workspace, whichever it was opened with). For a shell into")
		moePrintln(stderr, "a named workspace directly, use `moe workspace shell <project> <name>`.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	return shellRunWorkspace(root, fs.Arg(0), fs.Arg(1), stdout, stderr)
}

// shellRunWorkspace resolves the run's workspace path and execs a
// shell there. Refuses missing or terminal runs at the boundary so
// a stale shell call against a closed run doesn't silently land on
// a directory that's about to be removed.
//
// Sources the cached dev-env from <tree>/.moe/dev-env.env if it
// exists so the operator's manual spot-check sees the same
// `DATABASE_URL`, `MOE_HOME`, etc. the agent saw during code/test
// stages. A missing cache means dev-env hooks never ran for this
// tree (no code/test stage yet, or the project ships no dev-env.d/);
// in that case the shell inherits the operator's real env, same as
// before.
func shellRunWorkspace(root, projectID, runID string, stdout, stderr io.Writer) int {
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	wp, err := resolveRunWorkspacePath(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	extraEnv, _, err := devEnvLoadCache(wp)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "shell in %s (run %s %s)\n", wp, md.Project, md.ID)
	return execShell(wp, mapToEnv(extraEnv), stderr)
}

// shellNamedWorkspace drops into a named workspace without a run. The
// workspace is created lazily on first use; subsequent calls are a
// stat. Doesn't switch branches and doesn't take a claim — the
// workspace is just a directory at this point, and a run that later
// attaches with `--workspace <name>` is what flips it to "owned."
//
// Sources the workspace's cached dev-env if it exists. The unclaimed
// shell-only path doesn't run setup itself (there's no run to bind
// to and no design intent for the workspace to imply env), so the
// env shows up only after some run has already attached to this
// workspace and produced a cache.
func shellNamedWorkspace(root, projectID, name string, stdout, stderr io.Writer) int {
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	wp, err := workspace.Ensure(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	holder, err := workspace.ReadClaim(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	extraEnv, _, err := devEnvLoadCache(wp)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	switch {
	case holder != nil:
		moePrintf(stdout, "shell in %s (workspace %q, currently held by run %s)\n", wp, name, holder.Run)
	default:
		moePrintf(stdout, "shell in %s (workspace %q, unclaimed)\n", wp, name)
	}
	return execShell(wp, mapToEnv(extraEnv), stderr)
}

// execShell runs the operator's interactive shell with cwd set to
// dir. $SHELL wins (so the operator gets their configured zsh / bash /
// fish), with /bin/sh as the universal fallback. We pass stdin /
// stdout / stderr straight through so the operator sees their normal
// prompt, completion, and signals; the verb exits with the shell's
// exit code. Returning 1 instead of the actual code on launch failure
// is intentional: any nonzero from the shell process itself is
// preserved.
//
// extraEnv is appended to os.Environ() before the shell launches —
// today's caller threads the cached dev-env vars through so the
// operator's manual spot-check sees the same `DATABASE_URL`,
// `MOE_HOME`, etc. the agent saw. An empty slice falls back to a
// plain inherited env.
func execShell(dir string, extraEnv []string, stderr io.Writer) int {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		moePrintf(stderr, "shell: launch %s: %v\n", shell, err)
		return 1
	}
	return 0
}
