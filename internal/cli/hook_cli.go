package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// `moe hook` is the cheap-fire iteration loop for project hooks. One
// verb today: `fire`, which mints a transient sandbox and exercises
// one event's `<event>.d/*` scripts end-to-end. No run.json, no journal
// trailers, no dash row — pair with the `moe hooks` workflow
// (hooks_workflow.go) when an edit should actually land in history.

func init() {
	g := NewCommandGroup("hook", "exercise one project hook event in a transient sandbox; pair with `moe hooks` for journaled edits")
	g.Register(&Command{
		Name:    "fire",
		Summary: "run projects/<project>/hooks/<event>.d/* once in a fresh transient sandbox",
		Run:     runHookFire,
	})
	RegisterGroup(g)
}

// hookFirePrefix is the run-id stem for transient hook-fire sandboxes.
// Strict prefix match: cleanPriorHookFireSandboxes never touches a
// real run's sandbox.
const hookFirePrefix = "hook-fire-"

// hookFireWorkflow is the synthetic workflow name written to the
// transient run.Metadata. It never reaches disk (the metadata is
// in-memory only) but flows into MOE_WORKFLOW so scripts that branch
// on workflow see a stable marker for "this is a fire, not a real run."
const hookFireWorkflow = "hook-fire"

// hookFireEvents is the single source of truth for which events
// `moe hook fire` will dispatch. Used by the pre-mint gate in
// runHookFire and the defensive default in dispatchHookFire so the
// two surfaces can't drift on which names are accepted.
var hookFireEvents = []string{"dev-env", "dev-env-teardown", "pre-push"}

func isHookFireEvent(s string) bool {
	for _, e := range hookFireEvents {
		if e == s {
			return true
		}
	}
	return false
}

func unknownHookFireEventMsg(event string) string {
	return fmt.Sprintf("hook fire: unknown event %q (known: %s)",
		event, strings.Join(hookFireEvents, ", "))
}

func runHookFire(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook fire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe hook fire <project> <event>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Events:")
		moePrintln(stderr, "  dev-env           run dev-env.d/* and dump the merged env on stdout")
		moePrintln(stderr, "  dev-env-teardown  run dev-env.d/* in memory, then dev-env-teardown.d/*")
		moePrintln(stderr, "  pre-push          run pre-push.d/* against the transient sandbox")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, event := fs.Arg(0), fs.Arg(1)

	// Validate the event before any side effects. cleanPriorHookFireSandboxes
	// and mintHookFireSandbox both touch disk; a typo'd event name shouldn't
	// look like "something ran" to the operator. Placement pre-findRoot is
	// deliberate — operator typos surface fast regardless of CWD.
	if !isHookFireEvent(event) {
		moePrintln(stderr, unknownHookFireEventMsg(event))
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	pj, err := project.Load(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if err := cleanPriorHookFireSandboxes(root, projectID, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	runID := fmt.Sprintf("%s%d", hookFirePrefix, time.Now().Unix())
	sandboxPath, err := mintHookFireSandbox(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Same shape as production: per-run branch under moe/<id>. Fire
	// branches are throwaway — they live and die with the sandbox dir —
	// but the pre-push built-in (rebase-onto-default) and any project
	// script that introspects the branch see the expected shape.
	if err := sandbox.CheckoutBranch(sandboxPath, branchPrefix+runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	md := &run.Metadata{
		ID:       runID,
		Project:  projectID,
		Workflow: hookFireWorkflow,
	}

	exit := dispatchHookFire(root, sandboxPath, pj, md, event, stdout, stderr)
	moePrintf(stdout, "sandbox: %s\n", sandboxPath)
	return exit
}

// mintHookFireSandbox calls sandbox.EnsureAt against a per-fire path
// under .moe/clones/<project>/hook-fire-<unix-ts>/. A var so tests
// can swap in a tempdir without needing a real submodule on disk.
var mintHookFireSandbox = func(root, projectID, runID string) (string, error) {
	dst := filepath.Join(root, ".moe", "clones", projectID, runID)
	return sandbox.EnsureAt(root, projectID, dst)
}

// dispatchHookFire is the per-event dispatch. Split out from runHookFire
// so tests can exercise the routing against a stub sandbox path,
// without standing up a real submodule worktree.
//
// pre-push uses runProjectHookScripts (project scripts only) rather
// than runHooks (built-ins + project scripts). The built-in is the
// rebase-onto-default check; on a transient sandbox there's nothing
// to rebase, and the operator running `moe hook fire pre-push` is
// here to exercise their own scripts. They can test rebase mechanics
// through a real run.
func dispatchHookFire(root, sandboxPath string, pj *project.Metadata, md *run.Metadata, event string, stdout, stderr io.Writer) int {
	switch event {
	case "dev-env":
		env, err := runDevEnvSetup(root, sandboxPath, md, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		dumpEnvSorted(stdout, env)
		return 0
	case "dev-env-teardown":
		env, err := runDevEnvSetup(root, sandboxPath, md, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if err := runDevEnvScripts(root, devEnvTeardownDirRel, sandboxPath, md, env, stdout, stderr); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	case string(hookEventPrePush):
		targetBranch := ""
		if pj != nil {
			targetBranch = pj.DefaultBranch
		}
		he := hookEnv{
			Project:      md.Project,
			Run:          md.ID,
			Document:     "hook-fire",
			Workflow:     md.Workflow,
			Sandbox:      sandboxPath,
			Bureaucracy:  root,
			TargetBranch: targetBranch,
		}
		if err := runProjectHookScripts(root, hookEventPrePush, he, stdout, stderr); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	default:
		// Defensive belt-and-suspenders: runHookFire gates on the same
		// allowlist before we ever reach here, but new callers of
		// dispatchHookFire shouldn't have to know that.
		moePrintln(stderr, unknownHookFireEventMsg(event))
		return 2
	}
}

// dumpEnvSorted writes env as sorted KEY=VALUE lines — same shape the
// cached dev-env.env file uses, so the operator's eye can match what
// they see on stdout against what a real run would cache.
func dumpEnvSorted(w io.Writer, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		moePrintf(w, "%s=%s\n", k, env[k])
	}
}

// cleanPriorHookFireSandboxes removes every prior
// .moe/clones/<project>/hook-fire-* worktree — keep-only-latest gives
// a disk-bounded story without a separate `moe hook clean` verb. The
// prefix match is strict: a real run's sandbox (slug never starts with
// hook-fire-) is never touched. Failures are reported and skipped — a
// stuck older sandbox shouldn't block today's fire.
func cleanPriorHookFireSandboxes(root, projectID string, stderr io.Writer) error {
	clonesDir := filepath.Join(root, ".moe", "clones", projectID)
	entries, err := os.ReadDir(clonesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hook fire: read %s: %w", clonesDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), hookFirePrefix) {
			continue
		}
		if err := sandbox.Remove(root, projectID, e.Name()); err != nil {
			moePrintf(stderr, "hook fire: leaving prior sandbox at %s: %v\n", filepath.Join(clonesDir, e.Name()), err)
		}
	}
	return nil
}
