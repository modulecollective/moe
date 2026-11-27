package cli

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// `moe intent` parks a goal on a project: a short statement of where the
// project is going, or of an outcome larger than one run, kept open
// until it is done or stops being relevant. Intents are just runs in a
// dedicated single-stage workflow (dash.IntentWorkflow, dash.IntentDocID),
// the same shape as ideas.
//
// The operator writes the direction, and the canvas is theirs whole. The
// pulse keeps the rest: status.md beside run.json (run.StatusPath),
// which every reader shows under the canvas, the link from each run to
// the intent it serves (run.json's `intent`), and — through its gate,
// each held to a high bar in the stage fragment — opening an intent for
// an outcome nobody named and closing one whose done condition it can
// show met. `moe intent reopen` undoes a close in one command, and a
// pulse never closes the same intent twice. `moe intent edit --chat` is
// a session the operator opens and drives, where the agent is the pen.
//
// An intent is never promoted, never executed, never handed to an agent
// to advance.

func init() {
	g := NewCommandGroup("intent", "intent workflow")
	g.Register(&Command{
		Name:    "new",
		Summary: "park a new intent in $EDITOR",
		Run:     intentKind.runNew,
	})
	g.Register(&Command{
		Name:    "edit",
		Summary: "sharpen a parked intent ($EDITOR, or --chat for an agent session)",
		Run:     intentKind.runEdit,
		argKind: argIntent,
	})
	g.Register(&Command{
		Name:    "close",
		Summary: "close an intent that's satisfied or gone stale (status → closed)",
		Run:     runIntentClose,
		argKind: argIntent,
	})
	g.Register(&Command{
		Name:    "reopen",
		Summary: "reopen a closed intent (same slug, so its links stay)",
		Run:     runIntentReopen,
	})
	g.Register(&Command{
		Name:    "runs",
		Summary: "list the work linked to an intent, grouped by state",
		Run:     runIntentRuns,
		argKind: argIntent,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "list open intents as <project>/<slug> (--project narrows)",
		Run:     intentKind.runList,
	})
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump an intent's canvas to stdout",
		Run:     runCat(dash.IntentWorkflow, dash.IntentDocID),
		argKind: argIntent,
	})
	// `edit --chat` mints a durable session on the intent document, so
	// the transcript needs a reader — without one the sessions this
	// feature creates would be unreadable, the dead surface the removed
	// idea-chat path left behind.
	g.Register(&Command{
		Name:    "log",
		Summary: "render an intent's agent transcript",
		Run:     runLog(dash.IntentWorkflow, dash.IntentDocID),
		argKind: argIntent,
	})
	// Manual recovery for captures a session couldn't fan out — a run
	// that stranded entries before HarvestOnExit existed, or a
	// session-end harvest that failed partway. Intent close never
	// harvests (capture close), so without this verb there is no
	// operator-driven path at all.
	g.Register(harvestCommand(dash.IntentWorkflow))
	RegisterGroup(g)

	// Register the intent workflow so run.Load, dash lookup, and the cat
	// resolver's wf.Stages() all resolve it. The single stage name
	// `intent` lives in the DAG without a matching `moe intent intent`
	// verb — operator-facing verbs (new/edit/close/list/cat) are group
	// subcommands above, same shape as idea.
	w := NewWorkflow(dash.IntentWorkflow)
	w.RegisterStage(dash.IntentDocID)
	RegisterWorkflow(w)
}

// runIntentClose closes an intent (satisfied or abandoned). Delegates to
// the shared close handler in close.go; intents keep the short `Close
// intent <p>/<r>` subject and the capture-workflow close path (clean-tree
// gate, no followups harvest, canvas-empty exemption) — the same path
// idea close rides.
func runIntentClose(args []string, stdout, stderr io.Writer) int {
	return runClose(dash.IntentWorkflow, "Close intent %s/%s", nil, args, stdout, stderr)
}

// runIntentReopen flips a closed intent back to in_progress, whoever
// closed it. The slug is unchanged, so every run linked to it stays
// linked.
func runIntentReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("intent reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe intent reopen <project>/<slug>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Reopens a closed intent. A pulse will not close it again; you can.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "intent reopen: %v\n", err)
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
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := runopen.ReopenIntent(root, projectID, slug); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "intent %s/%s does not exist\n", projectID, slug)
		} else {
			moePrintf(stderr, "intent: reopen: %v\n", err)
		}
		return 1
	}
	moePrintf(stdout, "reopened intent %s/%s\n", projectID, slug)
	return 0
}

// runIntentRuns prints the work linked to one intent, grouped by state:
// live (in progress or pushed), landed (merged), then the rest.
func runIntentRuns(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		moePrintln(stderr, "usage: moe intent runs <project>/<slug>")
		return 2
	}
	key := args[0]
	projectID, slug, err := splitProjectRun(key)
	if err != nil {
		moePrintf(stderr, "intent runs: %v\n", err)
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if _, err := intentKind.loadRun(root, projectID, slug); err != nil {
		moePrintf(stderr, "intent runs: %v\n", err)
		return 1
	}
	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "intent runs: %v\n", err)
		return 1
	}
	groups := intentWork(mds, key)
	if groups.empty() {
		moePrintf(stdout, "no work linked to %s\n", key)
		return 0
	}
	for _, g := range []struct {
		name string
		mds  []*run.Metadata
	}{{"live", groups.Live}, {"landed", groups.Landed}, {"other", groups.Other}} {
		if len(g.mds) == 0 {
			continue
		}
		moePrintf(stdout, "%s (%d)\n", g.name, len(g.mds))
		for _, md := range g.mds {
			moePrintf(stdout, "  %s/%s\t%s\t%s\n", md.Project, md.ID, md.Workflow, md.Status)
		}
	}
	return 0
}

// intentWorkGroups is the work linked to one intent, split the way every
// reader shows it: live, landed, and the rest (closed, promoted).
type intentWorkGroups struct {
	Live, Landed, Other []*run.Metadata
}

func (g intentWorkGroups) empty() bool {
	return len(g.Live)+len(g.Landed)+len(g.Other) == 0
}

// intentWork groups the runs in mds that link to the intent key.
func intentWork(mds []*run.Metadata, key string) intentWorkGroups {
	var g intentWorkGroups
	var linked []*run.Metadata
	for _, md := range mds {
		if md.Intent == key {
			linked = append(linked, md)
		}
	}
	sortRunsByKey(linked)
	for _, md := range linked {
		switch md.Status {
		case run.StatusInProgress, run.StatusPushed:
			g.Live = append(g.Live, md)
		case run.StatusMerged:
			g.Landed = append(g.Landed, md)
		default:
			g.Other = append(g.Other, md)
		}
	}
	return g
}

// gatherIntents projects the open intent runs in mds into the
// dash.IntentInput set the INTENTS section renders, reading each title
// off its canvas's first heading and counting the work linked to it.
// All projects are gathered; BuildRows
// applies the dash's ProjectFilter. mds is the caller's existing scan,
// so this adds only the per-intent title read (intents are few and
// operator-pruned).
func gatherIntents(root string, mds []*run.Metadata) []dash.IntentInput {
	open, landed := map[string]int{}, map[string]int{}
	for _, md := range mds {
		switch {
		case md.Intent == "":
		case md.Status == run.StatusInProgress || md.Status == run.StatusPushed:
			open[md.Intent]++
		case md.Status == run.StatusMerged:
			landed[md.Intent]++
		}
	}
	var out []dash.IntentInput
	for _, md := range mds {
		if md.Workflow != dash.IntentWorkflow || md.Status != run.StatusInProgress {
			continue
		}
		key := md.Project + "/" + md.ID
		out = append(out, dash.IntentInput{
			Project: md.Project,
			Slug:    md.ID,
			Title:   intentTitle(root, md.Project, md.ID),
			Pulse:   md.SpawnedBy != "",
			Open:    open[key],
			Landed:  landed[key],
		})
	}
	return out
}

// intentTitle reads an intent canvas's first ATX `# ` heading, the title
// the catalog and dash render. Falls back to the slug when the canvas is
// missing, unreadable, or headless — a half-written intent must never
// break the prompt or the dash. The first heading wins.
func intentTitle(root, projectID, slug string) string {
	path := filepath.Join(root, run.ContentPath(projectID, slug, dash.IntentDocID))
	content, err := os.ReadFile(path)
	if err != nil {
		return slug
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			if title := strings.TrimSpace(rest); title != "" {
				return title
			}
		}
	}
	return slug
}
