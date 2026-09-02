package cli

import (
	"bytes"
	"flag"
	"io"
	"math/rand"
	"os"
	"time"
	"unicode"
	"unicode/utf8"

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
		renderDashFrame(stdout, now, root, snap, *all)
		return 0
	}

	// Watch mode writes raw repaint sequences and leans on every layer's
	// TTY colour gate, so a piped stdout gets an error instead of an
	// unwatchable infinite loop.
	if !cliout.IsTTY(stdout) {
		moePrintln(stderr, "dash: --watch needs a terminal on stdout")
		return 2
	}
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
			renderDashFrame(&frame, now, root, snap, *all)
		}
		_ = writeWatchViewport(stdout, frame.Bytes(), rows, columns)
		_, _ = io.WriteString(stdout, dashFramePost)
		time.Sleep(dashWatchInterval)
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
		if err := writeClippedTerminalLine(w, frame[start:end], contentColumns); err != nil {
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

func writeClippedTerminalLine(w io.Writer, line []byte, columns int) error {
	cells := 0
	clipped := false
	for len(line) > 0 {
		if n := csiSequenceLen(line); n > 0 {
			if err := writeBytes(w, line[:n]); err != nil {
				return err
			}
			line = line[n:]
			continue
		}
		if len(line) >= 2 && line[0] == '\x1b' && line[1] == '[' {
			// Never pass through a partial CSI sequence.
			break
		}

		size, width := terminalTextUnit(line)
		if !clipped && width > 0 && cells+width > columns {
			clipped = true
		}
		if !clipped {
			if err := writeBytes(w, line[:size]); err != nil {
				return err
			}
			cells += width
		}
		line = line[size:]
	}
	return nil
}

func terminalTextUnit(p []byte) (size, width int) {
	r, size := utf8.DecodeRune(p)
	width = terminalRuneWidth(r)
	rest := p[size:]

	// Emoji presentation turns otherwise narrow symbols (for example ❤)
	// into two-cell glyphs. Keep the selector with its base so clipping
	// cannot emit a sequence whose width was decided only after the base.
	if next, nextSize := utf8.DecodeRune(rest); next == '\ufe0e' || next == '\ufe0f' {
		size += nextSize
		rest = rest[nextSize:]
		if next == '\ufe0f' && width == 1 {
			width = 2
		}
	}
	// Keycaps are two cells whether or not their optional emoji-presentation
	// selector is present.
	if isKeycapBase(r) {
		if next, nextSize := utf8.DecodeRune(rest); next == '\u20e3' {
			size += nextSize
			width = 2
		}
	}
	return size, width
}

func isKeycapBase(r rune) bool {
	return r == '#' || r == '*' || r >= '0' && r <= '9'
}

func csiSequenceLen(p []byte) int {
	if len(p) < 3 || p[0] != '\x1b' || p[1] != '[' {
		return 0
	}
	for i := 2; i < len(p); i++ {
		if p[i] >= 0x40 && p[i] <= 0x7e {
			return i + 1
		}
	}
	return 0
}

func writeBytes(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func terminalRuneWidth(r rune) int {
	if r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Mn, r) ||
		unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) ||
		r >= 0x1f3fb && r <= 0x1f3ff ||
		r >= 0xe0100 && r <= 0xe01ef {
		return 0
	}
	if isWideTerminalRune(r) {
		return 2
	}
	return 1
}

// isWideTerminalRune is the compact width table this viewport needs. It
// covers East Asian wide/full-width blocks and the emoji ranges terminals
// conventionally render as two cells; ambiguous dashboard glyphs such as box
// drawing, block elements, and ▶ deliberately remain one cell.
func isWideTerminalRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r == 0x2329 || r == 0x232a,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r == 0x1f004 || r == 0x1f0cf || r == 0x1f18e,
		r >= 0x1f191 && r <= 0x1f19a,
		r >= 0x1f200 && r <= 0x1f202,
		r >= 0x1f210 && r <= 0x1f23b,
		r >= 0x1f240 && r <= 0x1f248,
		r >= 0x1f250 && r <= 0x1f251,
		r >= 0x1f300 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	switch r {
	case 0x231a, 0x231b, 0x23e9, 0x23ea, 0x23eb, 0x23ec, 0x23f0, 0x23f3,
		0x25fd, 0x25fe, 0x2614, 0x2615, 0x2648, 0x2649, 0x264a, 0x264b,
		0x264c, 0x264d, 0x264e, 0x264f, 0x2650, 0x2651, 0x2652, 0x2653,
		0x267f, 0x2693, 0x26a1, 0x26aa, 0x26ab, 0x26bd, 0x26be, 0x26c4,
		0x26c5, 0x26ce, 0x26d4, 0x26ea, 0x26f2, 0x26f3, 0x26f5, 0x26fa,
		0x26fd, 0x2705, 0x270a, 0x270b, 0x2728, 0x274c, 0x274e, 0x2753,
		0x2754, 0x2755, 0x2757, 0x2795, 0x2796, 0x2797, 0x27b0, 0x27bf,
		0x2b1b, 0x2b1c, 0x2b50, 0x2b55:
		return true
	}
	return false
}

// renderDashFrame writes one full dashboard frame — banner, serve lines,
// factory art, sections — to its destination. Watch mode uses
// ttyFrameBuffer so cliout's colour gates still reach the real terminal
// through Unwrap, and re-reads serve's state file per frame, which is one
// small os.ReadFile.
func renderDashFrame(stdout io.Writer, now time.Time, root string, snap DashSnapshot, all bool) {
	state := dash.FactoryStateFromRows(snap.Rows)
	// Fresh rand per frame, so the factory's smoke re-rolls each tick
	// and a watched dash reads as alive.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Mark the dash render with the same one-line gradient bar every
	// stage session opens with, carrying the render timestamp and serve's
	// status in its tail. Serve status is ambient context, not an event —
	// it belongs with the render metadata rather than on a line of its
	// own, and this way it costs the frame nothing.
	serveState := readServeState(root)
	banner.Dash(stdout, now, serveState.bannerCluster(now))
	// Directly under the banner, above the art: the projects a sweep is
	// working, holding back, or has killed. The banner's serve cluster is
	// their label.
	serveState.renderLines(stdout, now)
	histogram := dash.BuildActivityHistogram(snap.Histogram)
	dash.Render(stdout, now, histogram, snap.Rows, snap.ProjectCount, snap.ActiveProjects, all, state, r)
}
