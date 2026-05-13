package cli

import (
	"flag"
	"io"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// `moe workspace` is the top-level verb group for named-workspace
// administration. Today's only verb is `dev-env-refresh`, which
// invalidates the cached dev-env so the next stage open against the
// workspace rebuilds it. As more workspace-scoped operations land
// (`moe workspace teardown`, `moe workspace list`), they slot in
// here without growing a new top-level command.
//
// The verb sits at the top level rather than under `moe sdlc shell`
// because the workspace concept spans workflows — a future workflow
// that uses named workspaces (none today, but the shape is in place)
// would share the same admin surface. Same reason `moe project` sits
// at the top level.

func init() {
	g := NewCommandGroup("workspace", "named-workspace administration: dev-env-refresh")
	g.Register(&Command{
		Name:    "dev-env-refresh",
		Summary: "tear down a workspace's cached dev-env so the next stage open rebuilds it",
		Run:     runWorkspaceDevEnvRefresh,
	})
	RegisterGroup(g)
}

// runWorkspaceDevEnvRefresh runs the project's dev-env-teardown.d/*
// scripts against the cached env, then clears <workspace>/.moe/dev-env.env.
// The next time a code or test stage opens against this workspace,
// dev-env.d/* runs again from a clean slate.
//
// Teardown halts on first non-zero exit so a stuck script doesn't
// silently leak resources past the cache delete — the operator sees
// the error and decides how to recover. The cache is left in place
// in that case so a retry can re-run teardown without conjuring a
// fresh dev-env on top of the half-torn-down old one.
//
// A workspace that exists but has no cached dev-env is a no-op (still
// exits 0): there's nothing to tear down, and clearing a missing
// cache is idempotent.
func runWorkspaceDevEnvRefresh(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace dev-env-refresh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workspace dev-env-refresh <project> <name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the project's dev-env-teardown.d/* scripts against the workspace's")
		moePrintln(stderr, "cached env, then deletes <workspace>/.moe/dev-env.env. The next code or")
		moePrintln(stderr, "test stage opened against this workspace rebuilds the env from scratch.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, name := fs.Arg(0), fs.Arg(1)

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
	moePrintf(stdout, "dev-env refreshed for workspace %s/%s\n", projectID, name)
	return 0
}
