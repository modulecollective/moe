package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// `moe run list` is the board's run rows as plain text: the read an agent
// or a script takes where `moe ui` refuses a non-terminal stdout. One
// tab-separated line per run, no header, colour or ages, so the output is
// stable to read and to diff. Rows and notes come from the same gather
// moe ui paints, so the list and the board cannot disagree.
func init() {
	g := NewCommandGroup("run", "runs across the bureaucracy")
	g.Register(&Command{
		Name:    "list",
		Summary: "list active runs, one tab-separated line each (--all adds completed)",
		Run:     runRunList,
	})
	RegisterGroup(g)
}

func runRunList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "list only this project's runs")
	workflow := fs.String("workflow", "", "list only runs of this workflow")
	all := fs.Bool("all", false, "also list completed runs (merged, closed, promoted)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe run list [--project <id>] [--workflow <name>] [--all]")
		moePrintln(stderr, "")
		moePrintln(stderr, "One line per run: <project>/<slug> <TAB> <workflow> <TAB> <note>,")
		moePrintln(stderr, "newest first, active runs before completed ones.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if *project != "" {
		if err := requireProject(root, *project); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
	}
	snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{ProjectFilter: *project, WorkflowFilter: *workflow})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	buckets := []dash.Bucket{dash.BucketActiveRuns}
	if *all {
		buckets = append(buckets, dash.BucketCompletedRuns)
	}
	for _, b := range buckets {
		for _, r := range snap.Rows {
			if r.Bucket == b {
				fmt.Fprintf(stdout, "%s/%s\t%s\t%s\n", r.Project, r.Run, r.Workflow, dash.NoteWithControl(r))
			}
		}
	}
	return 0
}
