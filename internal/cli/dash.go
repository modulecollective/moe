package cli

import (
	"flag"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
)

// dashWatchInterval is how long `--watch` sleeps between frames. Baked
// rather than flagged: single-operator tool, and 3s is the operator's
// `watch -n 3` habit.
const dashWatchInterval = 3 * time.Second

// dashClear is cursor-home + erase-display, the clear(1) sequence.
// Scrollback (\x1b[3J) is deliberately left alone so the operator keeps
// whatever was in the pane before watch mode started.
const dashClear = "\x1b[H\x1b[2J"

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (backlog / runs)",
		Run:     runDash,
	})
}

// runDash is the cli/handler. Loads the inputs the dash package
// needs (run scan, journal index, open-session list, per-run
// next-stage decisions, per-project twin configs) and hands them to
// dash for assembly + render.
func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show every completed run, not just the newest 10")
	project := fs.String("project", "", "show only rows whose run belongs to this project")
	workflow := fs.String("workflow", "", "show only rows whose run uses this workflow")
	watch := fs.Bool("watch", false, "redraw the dashboard every 3s until Ctrl-C (terminal only)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all] [--project <id>] [--workflow <name>] [--watch]")
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

	if !*watch {
		now := time.Now().UTC()
		snap, err := GatherDashSnapshot(root, now, filter)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		renderDashFrame(stdout, now, snap, *all)
		return 0
	}

	// Watch mode writes raw clear sequences and leans on every layer's
	// TTY colour gate, so a piped stdout gets an error instead of an
	// unwatchable infinite loop.
	if !cliout.IsTTY(stdout) {
		moePrintln(stderr, "dash: --watch needs a terminal on stdout")
		return 2
	}
	for first := true; ; first = false {
		// Gather before clearing: the scan is the slow part, so doing
		// it first keeps the cleared-to-drawn gap down to the in-memory
		// formatting and the flicker with it.
		now := time.Now().UTC()
		snap, err := GatherDashSnapshot(root, now, filter)
		if err != nil && first {
			// A typo'd invocation (or a missing bureaucracy) should
			// fail fast, same as non-watch mode.
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		// Raw, not through cliout: a control sequence isn't moe's
		// voice and doesn't want the amber wrap.
		_, _ = io.WriteString(stdout, dashClear)
		if err != nil {
			// A dashboard left running overnight has to survive a
			// transient scan error (a run closed mid-scan), so the
			// error becomes the frame and the loop continues. It goes
			// to stdout because stdout is the frame surface — on
			// stderr a redirect would leave the pane blank instead.
			moePrintf(stdout, "%v\n", err)
		} else {
			renderDashFrame(stdout, now, snap, *all)
		}
		time.Sleep(dashWatchInterval)
	}
}

// renderDashFrame writes one full dashboard frame — banner, factory
// art, sections — straight to stdout. Frames are never buffered:
// cliout's colour gates only fire on a real *os.File terminal, so
// routing a frame through a bytes.Buffer would strip every style.
func renderDashFrame(stdout io.Writer, now time.Time, snap DashSnapshot, all bool) {
	state := dash.FactoryStateFromRows(snap.Rows)
	// Fresh rand per frame, so the factory's smoke re-rolls each tick
	// and a watched dash reads as alive.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Mark the dash render with the same one-line gradient bar every
	// stage session opens with, suffixed with the render timestamp so
	// the operator can tell a stale tab from a fresh one. Dash refreshes
	// are frequent, so we keep it to one line instead of a multi-line
	// block.
	banner.Dash(stdout, now)
	histogram := dash.BuildActivityHistogram(snap.Histogram)
	dash.Render(stdout, now, histogram, snap.Rows, snap.ProjectCount, snap.ActiveProjects, all, state, r)
}
