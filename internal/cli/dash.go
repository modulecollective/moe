package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
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

// Watch mode repaints in place rather than clearing. The clear(1)
// sequence (`\x1b[H\x1b[2J`) it used to emit cost the operator twice:
// tmux pushes every ED-2'd frame into pane history — ~49 lines a tick,
// turning the default 2000-line history over in a couple of minutes —
// and the cleared-to-drawn gap reads as a flash on every tick. Both go
// away if nothing is ever erased ahead of the redraw:
//
//   - dashFramePre homes the cursor inside a synchronized-output DECSET
//     (`?2026`), so tmux ≥3.4 applies the whole repaint atomically and
//     terminals that don't know the mode ignore the pair.
//   - every frame line ends with dashEraseLine (EL, erase-to-EOL) so a
//     line that got shorter than last tick's leaves no stale tail.
//   - dashFramePost resets SGR in case clipping swallowed a renderer's
//     line-end reset, then closes with ED-0, erasing whatever the old
//     frame had below the new one's last line, and ends the sync block.
//
// The watch viewport clips every logical line one cell short of the
// terminal width and reserves the terminal's bottom row. Keeping clear of
// both margins prevents wrapping or line advance from scrolling the pane.
// \x1b[3J (wipe scrollback) stays omitted — blowing away the operator's
// history isn't ours to do.
const (
	dashFramePre  = "\x1b[?2026h\x1b[H"
	dashFramePost = cliout.Reset + "\x1b[J\x1b[?2026l"
	dashEraseLine = "\x1b[K"
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
	watch := fs.Bool("watch", false, "redraw the dashboard every 3s until Ctrl-C (terminal only)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all] [--project <id>] [--workflow <name>] [--watch]")
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

	if !*watch {
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
		renderDashFrame(stdout, now, snap, *all, readServeState(root))
		return 0
	}

	// Watch mode writes raw repaint sequences and leans on every layer's
	// TTY colour gate, so a piped stdout gets an error instead of an
	// unwatchable infinite loop.
	if !cliout.IsTTY(stdout) {
		moePrintln(stderr, "dash: --watch needs a terminal on stdout")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watchDash(ctx, root, filter, *all, stdout, stderr)
}

func watchDash(ctx context.Context, root string, filter DashFilter, all bool, stdout, stderr io.Writer) int {
	status := dashStatus{}
	if cliout.RexStatusEnabled(stdout) {
		status.w = stdout
	}
	defer status.close()
	_ = status.update(nil)
	for first := true; ; first = false {
		rows, columns, sizeErr := cliout.TerminalSize(stdout)
		if sizeErr != nil && first {
			moePrintf(stderr, "dash: %v\n", sizeErr)
			return 1
		}

		// Gather before repainting: the scan is the slow part, so doing
		// it first keeps the in-place overwrite down to one burst of
		// formatting rather than a scan-long half-drawn frame.
		now := time.Now()
		var snap DashSnapshot
		var err error
		if sizeErr != nil {
			// With no trustworthy dimensions, no content row is safe.
			// Paint only the control stream, clearing the old frame, and
			// retry the terminal on the next tick.
			rows, columns = 0, 0
		} else {
			snap, err = GatherDashSnapshot(root, now, filter)
		}
		if err != nil && first {
			// A typo'd invocation (or a missing bureaucracy) should
			// fail fast, same as non-watch mode.
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		serveState := readServeState(root)
		// Raw, not through cliout: a control sequence isn't moe's
		// voice and doesn't want the amber wrap. Straight to stdout,
		// not through frame — the sequence carries no newline to erase
		// past and EL here would clobber the line the cursor lands on.
		_, _ = io.WriteString(stdout, dashFramePre)
		frame := ttyFrameBuffer{terminal: stdout}
		if sizeErr == nil && err != nil {
			// A dashboard left running overnight has to survive a
			// transient scan error (a run closed mid-scan), so the
			// error becomes the frame and the loop continues. It goes
			// to stdout because stdout is the frame surface — on
			// stderr a redirect would leave the pane blank instead.
			// The ED-0 below erases the rest of the dashboard it
			// replaces, so no special case is needed.
			moePrintf(&frame, "%v\n", err)
		} else if sizeErr == nil {
			renderDashFrame(&frame, now, snap, all, serveState)
		}
		_ = writeWatchViewport(stdout, frame.Bytes(), rows, columns)
		_, _ = io.WriteString(stdout, dashFramePost)
		observationErr := err
		if sizeErr != nil {
			observationErr = sizeErr
		}
		// Reporting is best-effort and cannot change the command's outcome.
		// Failed reports remain unacknowledged and retry on the next tick.
		_ = status.update(dashStatusReports(snap, observationErr, serveState, filter))
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(dashWatchInterval):
		}
	}
}

// ttyFrameBuffer lets the dashboard render a complete frame without hiding
// the terminal from its colour gates. The bytes land in memory; Unwrap points
// cliout.IsTTY at the terminal they will ultimately be painted to.
type ttyFrameBuffer struct {
	bytes.Buffer
	terminal io.Writer
}

func (b *ttyFrameBuffer) Unwrap() io.Writer { return b.terminal }

// writeWatchViewport paints the leading terminal-sized viewport of a
// completed dashboard frame. Each source line occupies at most one physical
// row: printable runes are clipped one cell short of the terminal width,
// complete CSI sequences pass through at zero width, and every visible row
// ends with EL.
func writeWatchViewport(w io.Writer, frame []byte, rows, columns int) error {
	if rows <= 1 || columns <= 1 {
		return nil
	}
	contentRows := rows - 1
	contentColumns := columns - 1

	for row, start := 0, 0; row < contentRows && start < len(frame); row++ {
		end := bytes.IndexByte(frame[start:], '\n')
		hasNewline := end >= 0
		if hasNewline {
			end += start
		} else {
			end = len(frame)
		}
		if err := cliout.ClipLine(w, frame[start:end], contentColumns); err != nil {
			return err
		}
		if _, err := io.WriteString(w, dashEraseLine); err != nil {
			return err
		}
		if row+1 == contentRows || !hasNewline {
			break
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		start = end + 1
	}
	return nil
}

// renderDashFrame writes one full dashboard frame — banner, serve lines,
// factory art, sections — to its destination. Watch mode uses
// ttyFrameBuffer so cliout's colour gates still reach the real terminal
// through Unwrap. The caller shares one serve read with status reporting.
func renderDashFrame(stdout io.Writer, now time.Time, snap DashSnapshot, all bool, serveState serveState) {
	state := dash.FactoryStateFromRows(snap.Rows)
	// Fresh rand per frame, so the factory's smoke re-rolls each tick
	// and a watched dash reads as alive.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Mark the dash render with the same one-line gradient bar every
	// stage session opens with, carrying the render timestamp and serve's
	// status in its tail. Serve status is ambient context, not an event —
	// it belongs with the render metadata rather than on a line of its
	// own, and this way it costs the frame nothing.
	banner.Dash(stdout, now, serveState.bannerCluster(now))
	// Directly under the banner, above the art: the projects a sweep is
	// working, holding back, or has killed. The banner's serve cluster is
	// their label.
	serveState.renderLines(stdout, now)
	histogram := dash.BuildActivityHistogram(snap.Histogram)
	dash.Render(stdout, now, histogram, snap.Rows, snap.ProjectCount, snap.ActiveProjects, all, state, r)
}
