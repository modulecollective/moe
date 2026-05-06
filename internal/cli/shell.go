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
// project workspace. Two shapes share one verb so the operator can use
// the same name regardless of whether they're poking at a specific
// run's tree or at a long-lived workspace they're using to keep a dev
// server warm:
//
//   - moe sdlc shell <project> <run>
//     The run's working tree — per-run sandbox or named workspace,
//     whichever it was opened with. Refuses if `moe sdlc code` was
//     never run (no clone on disk).
//
//   - moe sdlc shell <project> --workspace <name>
//     A named workspace directly, no run involved. Lazily creates the
//     workspace on first use (same clonefile mechanic as `moe sdlc
//     code`'s first attach), then drops into the shell without
//     touching branches or claims. Whatever's currently checked out
//     is what the operator sees — exactly the warm-state property the
//     named-workspace mode exists to provide.
//
// The shell is whatever $SHELL says, defaulting to /bin/sh. Stdin /
// stdout / stderr are wired straight through, so the operator gets
// their normal prompt, history, completion, and signals; the verb
// exits with the shell's exit code.
func runShell(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspaceName := fs.String("workspace", "", "drop into the named workspace at .moe/named/<project>/<name>/ instead of a specific run's clone")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc shell <project> <run>")
		moePrintln(stderr, "       moe sdlc shell <project> --workspace <name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Drops into a shell rooted at a project workspace. The first form")
		moePrintln(stderr, "uses the run's working tree (per-run sandbox or named workspace,")
		moePrintln(stderr, "whichever it was opened with). The second form uses a named")
		moePrintln(stderr, "workspace directly, lazily creating it — useful for warming a dev")
		moePrintln(stderr, "server in advance and reusing it across runs.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	switch {
	case *workspaceName != "":
		if fs.NArg() != 1 {
			fs.Usage()
			return 2
		}
		return shellNamedWorkspace(root, fs.Arg(0), *workspaceName, stdout, stderr)
	default:
		if fs.NArg() != 2 {
			fs.Usage()
			return 2
		}
		return shellRunWorkspace(root, fs.Arg(0), fs.Arg(1), stdout, stderr)
	}
}

// shellRunWorkspace resolves the run's workspace path and execs a
// shell there. Refuses missing or terminal runs at the boundary so
// a stale shell call against a closed run doesn't silently land on
// a directory that's about to be removed.
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
	moePrintf(stdout, "shell in %s (run %s/%s)\n", wp, md.Project, md.ID)
	return execShell(wp, stderr)
}

// shellNamedWorkspace drops into a named workspace without a run. The
// workspace is created lazily on first use; subsequent calls are a
// stat. Doesn't switch branches and doesn't take a claim — the
// workspace is just a directory at this point, and a run that later
// attaches with `--workspace <name>` is what flips it to "owned."
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
	switch {
	case holder != nil:
		moePrintf(stdout, "shell in %s (workspace %q, currently held by run %s)\n", wp, name, holder.Run)
	default:
		moePrintf(stdout, "shell in %s (workspace %q, unclaimed)\n", wp, name)
	}
	return execShell(wp, stderr)
}

// execShell runs the operator's interactive shell with cwd set to
// dir. $SHELL wins (so the operator gets their configured zsh / bash /
// fish), with /bin/sh as the universal fallback. We pass stdin /
// stdout / stderr straight through so the operator sees their normal
// prompt, completion, and signals; the verb exits with the shell's
// exit code. Returning 1 instead of the actual code on launch failure
// is intentional: any nonzero from the shell process itself is
// preserved.
func execShell(dir string, stderr io.Writer) int {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
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
