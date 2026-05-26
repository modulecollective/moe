package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// writeAlignedRows prints rows of equal length as a padded table. The
// first row is treated like every other (the workspace-list caller
// uses it as a header). Columns are padded to the widest cell in each
// column; the last column has no trailing padding so the line doesn't
// carry an awkward run of spaces.
func writeAlignedRows(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	cols := len(rows[0])
	widths := make([]int, cols)
	for _, r := range rows {
		for i, cell := range r {
			if i >= cols {
				break
			}
			if n := len(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, cell := range r {
			b.WriteString(cell)
			if i < cols-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteByte('\n')
		moePrint(w, b.String())
	}
}

// `moe workspace` is the top-level verb group for named-workspace
// administration. The verbs round out the CRUD a long-lived workspace
// dir needs:
//
//   - new       — explicit create (lazy create via sdlc still works)
//   - list      — enumerate, with claim / branch / dirty / dev-env state
//   - remove    — tear down dev-env, then delete the dir
//   - release   — clear a stuck claim
//   - refresh   — rebuild the cached dev-env in place
//   - rebase    — advance workspace branch onto the canonical's main
//
// The verb sits at the top level rather than under `moe sdlc shell`
// because the workspace concept spans workflows — a future workflow
// that uses named workspaces (none today, but the shape is in place)
// would share the same admin surface. Same reason `moe project` sits
// at the top level.

func init() {
	g := NewCommandGroup("workspace", "named-workspace administration: new, list, shell, remove, release, refresh, rebase")
	g.Register(&Command{
		Name:    "new",
		Summary: "create a named workspace for a project (idempotent)",
		Run:     runWorkspaceNew,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "list named workspaces (optionally filtered by project)",
		Run:     runWorkspaceList,
	})
	g.Register(&Command{
		Name:    "shell",
		Summary: "drop into a shell rooted at a named workspace (lazily creates)",
		Run:     runWorkspaceShell,
	})
	g.Register(&Command{
		Name:    "remove",
		Summary: "tear down dev-env and delete a named workspace",
		Run:     runWorkspaceRemove,
	})
	g.Register(&Command{
		Name:    "release",
		Summary: "clear a stuck claim on a named workspace",
		Run:     runWorkspaceRelease,
	})
	g.Register(&Command{
		Name:    "refresh",
		Summary: "rebuild a workspace's cached dev-env in place",
		Run:     runWorkspaceRefresh,
	})
	g.Register(&Command{
		Name:    "rebase",
		Summary: "advance a workspace branch onto the canonical's current main",
		Run:     runWorkspaceRebase,
	})
	RegisterGroup(g)
}

// runWorkspaceShell drops the operator into the named workspace
// directly, no run involved. Thin wrapper over shellNamedWorkspace —
// same lazy create, same cached dev-env sourcing, same claim-aware
// status message as the previous `moe sdlc shell --workspace <name>`
// form. Promoted from a flag form to a top-level verb because
// workspaces aren't sdlc-specific; single-operator, no-config-knobs,
// one verb per shape.
func runWorkspaceShell(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace shell <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Drops into a shell rooted at .moe/named/<project>/<name>/, lazily")
		moePrintln(stderr, "creating the workspace on first use. Doesn't switch branches and")
		moePrintln(stderr, "doesn't take a claim — useful for warming a dev server in advance")
		moePrintln(stderr, "and reusing it across runs.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace shell: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return shellNamedWorkspace(root, projectID, name, stdout, stderr)
}

// runWorkspaceNew materialises a named workspace eagerly. Same
// primitive as the lazy creation path used by `sdlc new --workspace`
// and `moe workspace shell`, exposed so the operator can warm a
// dev server / run `pnpm install` before any run attaches. Idempotent
// — second call on an existing workspace prints a "already exists"
// note and exits 0 without touching the claim or the working tree.
func runWorkspaceNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace new <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Creates the named workspace directory under .moe/named/<project>/<name>/.")
		moePrintln(stderr, "Idempotent — a workspace that already exists prints a note and exits 0.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace new: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if workspace.Exists(root, projectID, name) {
		moePrintf(stdout, "workspace %s/%s already exists at %s\n",
			projectID, name, workspace.Path(root, projectID, name))
		return 0
	}
	wp, err := workspace.Ensure(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "created workspace %s/%s at %s\n", projectID, name, wp)
	return 0
}

// runWorkspaceRelease drops the claim on a named workspace. Thin
// wrapper over workspace.Release with a friendlier message:
// reads the existing claim first so the success line can name what
// was cleared, and refuses if the workspace dir doesn't exist
// (same shape as refresh).
//
// The operator is asserting the holding run is stuck / dead. No
// liveness check, no --force: this is the explicit override.
func runWorkspaceRelease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace release <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Clears claim.json on the named workspace so a new run can attach.")
		moePrintln(stderr, "Use when the holding run is stuck or abandoned — moe does not check.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace release: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if !workspace.Exists(root, projectID, name) {
		moePrintf(stderr, "workspace %q for project %q does not exist on disk\n", name, projectID)
		return 1
	}
	prior, err := workspace.ReadClaim(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.Release(root, projectID, name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if prior == nil {
		moePrintf(stdout, "workspace %s/%s: no claim to release\n", projectID, name)
		return 0
	}
	moePrintf(stdout, "workspace %s/%s: released claim previously held by %s\n",
		projectID, name, prior.Run)
	return 0
}

// runWorkspaceList prints a table of named workspaces. Without
// arguments: every project's workspaces. With a project argument: just
// that project's. Columns: WORKSPACE / BRANCH / CLAIM / DIRTY /
// DEV-ENV, where WORKSPACE is the `<project>/<name>` identifier every
// other workspace verb parses — paste a cell straight into `workspace
// remove` / `shell` / `refresh` / `release`. Slash form holds in the
// filtered case too: output shape stays the same regardless of
// invocation.
//
// Empty result exits 0 with no output — same posture `project list`
// takes for empty state.
func runWorkspaceList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe workspace list [<project>]") }
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	var filter string
	if fs.NArg() == 1 {
		filter = fs.Arg(0)
		if err := requireProject(root, filter); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
	}
	infos, err := workspace.List(root, filter)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if len(infos) == 0 {
		return 0
	}
	rows := make([][]string, 0, len(infos)+1)
	rows = append(rows, []string{"WORKSPACE", "BRANCH", "CLAIM", "DIRTY", "DEV-ENV"})
	for _, info := range infos {
		claim := "unclaimed"
		if info.Claim != "" {
			claim = info.Claim
		}
		dirty := ""
		if info.Dirty {
			dirty = "*"
		}
		devenv := ""
		if info.DevEnvCached {
			devenv = "cached"
		}
		rows = append(rows, []string{info.Project + "/" + info.Name, info.Branch, claim, dirty, devenv})
	}
	writeAlignedRows(stdout, rows)
	return 0
}

// runWorkspaceRemove tears the workspace down. Order matters:
//
//  1. Refuse if claim.json exists; the holding run owns the dir.
//     The operator's recovery path is `moe close` (or `moe workspace
//     release` for a known-stuck run), not a `--force`.
//  2. Run the project's dev-env-teardown.d/* against the cached env so
//     postgres dbs / tmpdirs / etc. die with the workspace.
//  3. os.RemoveAll the directory.
//
// Missing workspace is a no-op (exit 0). Teardown failure halts before
// the delete so the operator can investigate.
func runWorkspaceRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace remove <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs dev-env-teardown.d/* against the cached env, then deletes the")
		moePrintln(stderr, "workspace directory. Refuses while claim.json names a holding run —")
		moePrintln(stderr, "close that run (or use `moe workspace release`) first.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace remove: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if !workspace.Exists(root, projectID, name) {
		moePrintf(stdout, "workspace %s/%s does not exist; nothing to remove\n", projectID, name)
		return 0
	}
	claim, err := workspace.ReadClaim(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if claim != nil {
		moePrintf(stderr, "workspace %s/%s is claimed by run %s — close that run first\n",
			projectID, name, claim.Run)
		return 1
	}
	wp := workspace.Path(root, projectID, name)
	md := &run.Metadata{Project: projectID, Workspace: name}
	if err := devEnvRunTeardown(root, wp, md, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		moePrintln(stderr, "       workspace left in place; resolve the teardown failure and re-run to retry")
		return 1
	}
	if err := workspace.Remove(root, projectID, name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "removed workspace %s/%s\n", projectID, name)
	return 0
}

// runWorkspaceRefresh rebuilds the workspace's cached dev-env in
// place: teardown the old env, clear the cache, then re-run setup
// against the project's current dev-env.d/* scripts. Eager rather than
// lazy so a broken setup script surfaces here, not on the next stage
// open.
//
// Teardown halts on first non-zero exit so a stuck script doesn't
// silently leak resources past the cache delete — the operator sees
// the error and decides how to recover. The cache is left in place
// in that case so a retry can re-run teardown without conjuring a
// fresh dev-env on top of the half-torn-down old one. Setup failure
// after a successful teardown leaves the cache empty, which is just
// the steady state for a fresh workspace; re-running picks up cleanly.
//
// A workspace that exists but has no cached dev-env skips the teardown
// path (nothing to tear down) and runs setup directly. A project with
// no dev-env.d/ directory yields an empty setup map and no cache file
// — same shape as the stage-open path.
func runWorkspaceRefresh(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace refresh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace refresh <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the project's dev-env-teardown.d/* against the cached env, clears")
		moePrintln(stderr, "<workspace>/.moe/dev-env.env, then runs dev-env.d/* to rebuild it now.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace refresh: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if !workspace.Exists(root, projectID, name) {
		moePrintf(stderr, "workspace %q for project %q does not exist on disk; nothing to refresh\n", name, projectID)
		return 1
	}
	wp := workspace.Path(root, projectID, name)

	// The teardown scripts take a run.Metadata for the MOE_* env vars
	// they get exported. The workspace isn't necessarily claimed by a
	// run right now (the operator may be refreshing between runs), so
	// we synthesise a minimal metadata struct keyed off the workspace
	// rather than refusing the verb when the workspace is unclaimed.
	// The synthesised run id stays empty so a teardown script that
	// branches on MOE_RUN sees the unclaimed state honestly.
	holder, err := workspace.ReadClaim(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	syntheticRunID := ""
	if holder != nil {
		syntheticRunID = holder.Run
	}
	md := &run.Metadata{
		Project:   projectID,
		ID:        syntheticRunID,
		Workspace: name,
	}

	if err := devEnvRunTeardown(root, wp, md, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		moePrintln(stderr, "       cache left in place; resolve the teardown failure and re-run to retry")
		return 1
	}
	if err := devEnvClearCache(wp); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, _, err := devEnvSetupEnv(root, wp, md, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "dev-env refreshed for workspace %s/%s\n", projectID, name)
	return 0
}

// runWorkspaceRebase brings the workspace's view of the canonical's
// default branch forward, then rebases the workspace's current branch
// onto it. The verb is operator-driven: the long-lived workspace dir
// holds a branch indefinitely, and `moe push` / `moe sync` keep the
// canonical fresh — but nothing else propagates that movement into a
// claimed workspace until this verb runs.
//
// Two layers of resolution share one entry:
//
//   - Workspace path missing on disk → refuse with a pointer to
//     `moe workspace new`. Same shape as release / refresh.
//   - Dirty working tree or detached HEAD → workspace.Rebase refuses;
//     surface the error verbatim. The operator commits or checks out a
//     branch and retries.
//   - Rebase conflicts → workspace.Rebase returns
//     *workspace.RebaseFailureError. Fire the shared one-shot resolver
//     (same agent the session-close path uses) and retry once. Single-
//     shot: if the second attempt still fails the typed error surfaces
//     and the operator resolves by hand.
func runWorkspaceRebase(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace rebase", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace rebase <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Fetches the canonical's current default branch into the workspace,")
		moePrintln(stderr, "fast-forwards the workspace's local default branch, then rebases the")
		moePrintln(stderr, "currently-checked-out branch onto it. Refuses on dirty workspaces and")
		moePrintln(stderr, "detached HEAD. On rebase conflicts, a one-shot agent gets one attempt")
		moePrintln(stderr, "to resolve before the conflict surfaces back to you.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "workspace rebase: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if !workspace.Exists(root, projectID, name) {
		moePrintf(stderr, "workspace %q for project %q does not exist on disk; create it with `moe workspace new`\n", name, projectID)
		return 1
	}
	defaultBranch, err := defaultBranchForProject(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	wp := workspace.Path(root, projectID, name)

	res, err := workspace.Rebase(wp, defaultBranch)
	if err == nil {
		printWorkspaceRebaseResult(stdout, projectID, name, res)
		return 0
	}
	var rebaseFail *workspace.RebaseFailureError
	if !errors.As(err, &rebaseFail) {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Single-shot recovery: fire the resolver agent inside the
	// workspace, then retry once. The retry calls workspace.Rebase
	// again rather than just `git rebase main`, because the resolver
	// may have left the branch in a state where the FF/fetch dance
	// still has work to do — re-running the whole sequence is cheaper
	// than reasoning about which steps the agent already completed.
	moePrintf(stderr, "workspace rebase: %v\n", err)
	moePrintln(stderr, "  launching an agent to resolve the rebase; single-shot — if it can't, the message above is what you'll see again")

	prompt := buildWorkspaceRebaseResolveKickoff(rebaseFail)
	if agentErr := launchRebaseResolve(rebaseFail.WorkspacePath, prompt, stdout, stderr); agentErr != nil {
		moePrintf(stderr, "  auto-resolve agent: %v\n", agentErr)
	}
	res, err = workspace.Rebase(wp, defaultBranch)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	printWorkspaceRebaseResult(stdout, projectID, name, res)
	return 0
}

// printWorkspaceRebaseResult renders the success summary. Three shapes
// keyed off what moved: nothing (already up to date), main only (HEAD
// was the default branch or no feature commits past it), or both
// main and the feature branch.
func printWorkspaceRebaseResult(w io.Writer, projectID, name string, res workspace.RebaseResult) {
	id := projectID + "/" + name
	mainMoved := res.MainBefore != res.MainAfter
	branchMoved := res.BranchBefore != res.BranchAfter
	if !mainMoved && !branchMoved {
		moePrintf(w, "workspace %s: already up to date with %s\n", id, res.DefaultBranch)
		return
	}
	if res.Branch == res.DefaultBranch {
		moePrintf(w, "workspace %s: %s fast-forwarded %s -> %s\n",
			id, res.DefaultBranch, git.ShortSHA(res.MainBefore), git.ShortSHA(res.MainAfter))
		return
	}
	if !branchMoved {
		moePrintf(w, "workspace %s: %s fast-forwarded %s -> %s; %s already on top of %s\n",
			id, res.DefaultBranch, git.ShortSHA(res.MainBefore), git.ShortSHA(res.MainAfter),
			res.Branch, res.DefaultBranch)
		return
	}
	moePrintf(w, "workspace %s: %s %s -> %s; rebased %s %s -> %s\n",
		id, res.DefaultBranch, git.ShortSHA(res.MainBefore), git.ShortSHA(res.MainAfter),
		res.Branch, git.ShortSHA(res.BranchBefore), git.ShortSHA(res.BranchAfter))
}

// buildWorkspaceRebaseResolveKickoff is the agent-facing user prompt
// for the workspace rebase chain-back. Conflict-state only: refuseDirty
// fires earlier so the resolver never sees the "rebase refused to
// start" case session-close has to handle. Names the workspace branch
// explicitly so the agent doesn't get confused with the bureaucracy
// session worktree case the same resolver also serves.
func buildWorkspaceRebaseResolveKickoff(e *workspace.RebaseFailureError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe workspace rebase` tried to rebase %s onto the project's default branch and hit conflicts. The rebase has been aborted, so the working tree is clean and the branch is back at its pre-rebase tip — you are starting from the conflict state, not mid-rebase.\n\n", e.Branch)
	if len(e.Conflicts) > 0 {
		b.WriteString("Files git flagged as conflicting on the abandoned rebase:\n")
		for _, p := range e.Conflicts {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\n")
	}
	b.WriteString("Re-run the rebase yourself (`git rebase main`), resolve the conflicts, and commit (or `git rebase --continue`) as you go. Then exit — the outer workspace-rebase verb will retry.\n\n")
	if e.GitOutput != "" {
		b.WriteString("Raw git output that triggered this:\n\n")
		b.WriteString(e.GitOutput)
		if !strings.HasSuffix(e.GitOutput, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
