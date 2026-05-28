package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

// The meta-moe workflow walks one project's run history and produces a
// markdown report — `projects/<p>/meta-moe.md` — that an operator can
// hand to the moe maintainers as upstream feedback ("how is moe working
// for project X"). The audience is moe maintainers; the runner is anyone
// using moe. A bureaucracy with multiple projects gets multiple reports
// — one per project — run when the operator wants one. The design lives
// under projects/moe/runs/meta-moe-2026-05-05/.
//
// Shape: one stage today (`report`). Each invocation is a tracked run at
// projects/<p>/runs/<slug>/, the same as every other workflow. The
// project-root snapshot at projects/<p>/meta-moe.md is a copy-of-the-
// latest so maintainers and operators can find recent output without
// knowing run ids; the per-pass canvas under the run is the audit
// record.

// metaMoeWorkflow is the workflow name written to run.json.
const metaMoeWorkflow = "meta-moe"

// metaMoeReportDoc is the document id for the (single) report stage.
// Canvas lives at projects/<p>/runs/<slug>/documents/report/content.md.
const metaMoeReportDoc = "report"

func init() {
	g := NewCommandGroup(metaMoeWorkflow, "meta-moe workflow: new, report")
	g.Register(newRunCommand(metaMoeWorkflow))
	g.Register(&Command{
		Name:    metaMoeReportDoc,
		Summary: "open a Claude Code session on the run's report canvas; publishes to projects/<p>/meta-moe.md on commit",
		Run:     runMetaMoeReport,
	})
	// meta-moe has no workspace, no sandbox clone, and no moe/<run>
	// branch (NeedsSandbox: false below), so the shared close skeleton
	// has nothing to clean up — pass nil and ride the standard
	// state-guard / harvest / status-flip path.
	g.Register(closeCommand(metaMoeWorkflow, "Close meta-moe run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump the run's report canvas to stdout",
		Run:     runCat(metaMoeWorkflow, metaMoeReportDoc),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render the run's report-stage agent transcript",
		Run:     runLog(metaMoeWorkflow, metaMoeReportDoc),
	})
	RegisterGroup(g)

	w := NewWorkflow(metaMoeWorkflow)
	w.RegisterStage(metaMoeReportDoc)
	RegisterWorkflow(w)
}

func runMetaMoeReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("meta-moe report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe meta-moe report [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the report canvas.")
		moePrintln(stderr, "The agent walks this project's run history (design canvases, transcripts,")
		moePrintln(stderr, "followups, slug collisions) and synthesises feedback for moe maintainers.")
		moePrintln(stderr, "On session-end, the canvas is published to projects/<project>/meta-moe.md")
		moePrintln(stderr, "in the same commit; the per-pass canvas under the run is the audit record.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "meta-moe report: %v\n", err)
		return 2
	}
	return openMetaMoeReport(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

// openMetaMoeReport is the Go-level seam behind `moe meta-moe report`.
// Mirrors openSdlcDesign / openKbResearch / etc.: the typed Command.Run
// parses args, this helper does the per-stage scan, builds the kickoff,
// and hands to runStageSession. The chain prompt's cascade driver
// reaches it through openMetaMoeStage in metamoe_stages.go.
func openMetaMoeReport(projectID, runID string, headless, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	scan, err := metaMoeScanProject(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	kickoff := metaMoeRenderKickoff(scan)
	return runStageSession(projectID, runID, metaMoeReportDoc, stageSessionOpts{
		NeedsSandbox:    false,
		InitialPrompt:   kickoff,
		Headless:        headless,
		SkipNextStage:   suppressNextStage,
		Agent:           agentOverride,
		ExtraStagePaths: metaMoePublishCanvas,
	}, stdout, stderr)
}

// metaMoePublishCanvas copies the per-pass report canvas to
// projects/<p>/meta-moe.md inside the session worktree, so the
// project-root snapshot rides along in the same commit as the canvas
// itself. Returns the (relative) path of the published file so
// runStageSession's commit stager can stage it.
//
// Reads the canvas straight from disk (workRoot is the session
// worktree; the agent's edits are already there). A missing or empty
// canvas falls through silently — commitTurn's existing canvas-existence
// guard will turn that into a clear error one layer up.
func metaMoePublishCanvas(workRoot string, md *run.Metadata) ([]string, error) {
	canvasRel := run.ContentPath(md.Project, md.ID, metaMoeReportDoc)
	canvasAbs := filepath.Join(workRoot, canvasRel)
	body, err := os.ReadFile(canvasAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("metamoe: read canvas %s: %w", canvasAbs, err)
	}
	if len(body) == 0 {
		return nil, nil
	}
	publishRel := filepath.Join("projects", md.Project, "meta-moe.md")
	publishAbs := filepath.Join(workRoot, publishRel)
	if err := os.MkdirAll(filepath.Dir(publishAbs), 0o755); err != nil {
		return nil, fmt.Errorf("metamoe: mkdir %s: %w", filepath.Dir(publishAbs), err)
	}
	if err := os.WriteFile(publishAbs, body, 0o644); err != nil {
		return nil, fmt.Errorf("metamoe: write %s: %w", publishAbs, err)
	}
	return []string{publishRel}, nil
}
