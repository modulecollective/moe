package cli

import (
	"bytes"
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
//   - dashFramePost closes with ED-0, erasing whatever the old frame
//     had below the new one's last line, then ends the sync block.
//
// Measured under tmux 3.5a: zero history growth per tick, pre-watch
// history intact. \x1b[3J (wipe scrollback) stays omitted — blowing
// away the operator's history isn't ours to do.
const (
	dashFramePre  = "\x1b[?2026h\x1b[H"
	dashFramePost = "\x1b[J\x1b[?2026l"
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

	// Watch mode writes raw repaint sequences and leans on every layer's
	// TTY colour gate, so a piped stdout gets an error instead of an
	// unwatchable infinite loop.
	if !cliout.IsTTY(stdout) {
		moePrintln(stderr, "dash: --watch needs a terminal on stdout")
		return 2
	}
	frame := eraseLineWriter{w: stdout}
	for first := true; ; first = false {
		// Gather before repainting: the scan is the slow part, so doing
		// it first keeps the in-place overwrite down to one burst of
		// formatting rather than a scan-long half-drawn frame.
		now := time.Now().UTC()
		snap, err := GatherDashSnapshot(root, now, filter)
		if err != nil && first {
			// A typo'd invocation (or a missing bureaucracy) should
			// fail fast, same as non-watch mode.
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		// Raw, not through cliout: a control sequence isn't moe's
		// voice and doesn't want the amber wrap. Straight to stdout,
		// not through frame — the sequence carries no newline to erase
		// past and EL here would clobber the line the cursor lands on.
		_, _ = io.WriteString(stdout, dashFramePre)
		if err != nil {
			// A dashboard left running overnight has to survive a
			// transient scan error (a run closed mid-scan), so the
			// error becomes the frame and the loop continues. It goes
			// to stdout because stdout is the frame surface — on
			// stderr a redirect would leave the pane blank instead.
			// The ED-0 below erases the rest of the dashboard it
			// replaces, so no special case is needed.
			moePrintf(frame, "%v\n", err)
		} else {
			renderDashFrame(frame, now, snap, *all)
		}
		_, _ = io.WriteString(stdout, dashFramePost)
		time.Sleep(dashWatchInterval)
	}
}

// eraseLineWriter splices EL (erase-to-end-of-line) in before every
// newline it passes through, so each repainted line clears whatever the
// previous frame left to its right. Stateless: the insertion point is
// the newline byte itself, so a frame split across Write calls at any
// boundary still comes out right.
//
// Unwrap lets cliout.IsTTY — and so cliout.Enabled, which the banner,
// histogram and factory-art stylers all gate on — see the terminal
// underneath instead of classifying the wrapper as a non-file writer
// and stripping the dashboard's colour.
type eraseLineWriter struct{ w io.Writer }

func (e eraseLineWriter) Unwrap() io.Writer { return e.w }

func (e eraseLineWriter) Write(p []byte) (int, error) {
	consumed := 0
	for consumed < len(p) {
		i := bytes.IndexByte(p[consumed:], '\n')
		if i < 0 {
			n, err := e.w.Write(p[consumed:])
			return consumed + n, err
		}
		if n, err := e.w.Write(p[consumed : consumed+i]); err != nil {
			return consumed + n, err
		}
		if _, err := io.WriteString(e.w, dashEraseLine+"\n"); err != nil {
			return consumed + i, err
		}
		consumed += i + 1
	}
	return consumed, nil
}

// renderDashFrame writes one full dashboard frame — banner, factory
// art, sections — straight to stdout. Frames are never buffered:
// cliout's colour gates only fire on a real *os.File terminal, so
// routing a frame through a bytes.Buffer would strip every style.
// Watch mode's eraseLineWriter is a pass-through, not a buffer, and
// stays gate-transparent via Unwrap.
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
