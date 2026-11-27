package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
)

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (backlog / runs)",
		Run:     runDash,
	})
}

// runDash is the cli/handler. Loads the inputs the dash package
// needs (run scan, journal index, open-session list, per-run
// next-stage decisions) and hands them to
// dash for assembly + render.
func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show every completed run")
	project := fs.String("project", "", "show only rows whose run belongs to this project")
	workflow := fs.String("workflow", "", "show only rows whose run uses this workflow")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all] [--project <id>] [--workflow <name>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	filter := DashFilter{
		ProjectFilter:  *project,
		WorkflowFilter: *workflow,
	}

	// Local, not UTC: `now` is what the banner prints as a bare
	// wall-clock stamp, and an unmarked timestamp reads as local.
	// Everything else it feeds is zone-independent (durations) or
	// re-anchors to UTC itself (the histogram's day keys).
	now := time.Now()
	snap, err := GatherDashSnapshot(root, now, filter)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	renderDashFrame(stdout, now, snap, *all, readServeState(root), readPulseNotes(root, stderr))
	return 0
}

// ttyFrameBuffer lets the UI render a complete frame without hiding
// the terminal from its colour gates. The bytes land in memory; Unwrap points
// cliout.IsTTY at the terminal they will ultimately be painted to.
type ttyFrameBuffer struct {
	bytes.Buffer
	terminal io.Writer
}

func (b *ttyFrameBuffer) Unwrap() io.Writer { return b.terminal }

// renderDashFrame writes one full dashboard frame — banner, the PULSE
// section, activity histogram, sections — to its destination.
func renderDashFrame(stdout io.Writer, now time.Time, snap DashSnapshot, all bool, serveState serveState, notes input.File) {
	// The same one-line gradient bar every stage session opens with,
	// carrying only the clock.
	banner.Dash(stdout, dash.Clock(now))
	// Directly under the banner, first on the board: the pulse's state —
	// the serve cluster and the projects a sweep is working, holding back,
	// or has killed — and what the operator has told it.
	var detail bytes.Buffer
	serveState.renderLines(&detail, now)
	dash.RenderPulse(stdout, now, serveState.pulse(now, snap.Rows, notes, detail.String()))
	// One shot: the live star stays at rest; moe ui is what twinkles it.
	histogram := dash.BuildActivityHistogram(trailingDays(snap.Histogram, dash.HistDays), dash.Live(snap.Rows))
	dash.Render(stdout, now, histogram, snap.Rows, snap.ProjectCount, snap.ActiveProjects, all)
}
