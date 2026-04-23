package cli

import (
	"io"
	"sort"
)

// `moe workflow <name> <sub>` is the single top-level verb that
// dispatches into any registered stage-laddered workflow — sdlc, kb,
// quick, and whatever joins them next. `moe wf` is the same command
// under a shorter name; both share a single `*Command` so they stay in
// lockstep and duplicate-registration guards keep working. Workflows
// opt out by setting `ExposedViaCLI=false` (see idea.go), which keeps
// them in the lookup registry for dash/run.Load without claiming a
// verb under `moe workflow`.

func init() {
	cmd := &Command{
		Name:    "workflow",
		Summary: "run a workflow stage (sdlc, kb, quick, …)",
		Run:     dispatchWorkflow,
	}
	Register(cmd)
	Register(&Command{
		Name:    "wf",
		Summary: "alias for `moe workflow`",
		Run:     dispatchWorkflow,
	})
}

func dispatchWorkflow(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printWorkflowUsage(stdout)
		return 0
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		printWorkflowUsage(stdout)
		return 0
	}
	wf, ok := workflows[name]
	if !ok || !wf.ExposedViaCLI {
		moePrintf(stderr, "unknown workflow %q\n", name)
		printWorkflowUsage(stderr)
		return 1
	}
	return wf.Command().Run(args[1:], stdout, stderr)
}

func printWorkflowUsage(out io.Writer) {
	moePrintln(out, "usage: moe workflow <name> <subcommand> [args...]")
	moePrintln(out, "")
	moePrintln(out, "workflows:")
	names := make([]string, 0, len(workflows))
	for n, wf := range workflows {
		if !wf.ExposedViaCLI {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		moePrintf(out, "  %-14s  %s\n", n, workflows[n].Summary)
	}
}
