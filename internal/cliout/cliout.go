// Package cliout writes moe-originated lines to the operator terminal
// with an amber SGR wrap when the destination is a TTY and NO_COLOR is
// unset. Passthrough output from subprocesses (git, claude) bypasses
// these helpers and stays unstyled, so the operator can tell at a
// glance which lines are moe talking vs. a subprocess.
//
// Lives in its own package so domain packages (push, sync, dash)
// can emit the same styled progress as cli without importing cli
// or duplicating the SGR-aware writer detection.
package cliout

import (
	"fmt"
	"io"
	"os"
)

// The moe palette: the operator's "gas plasma" tmux theme (amber chrome
// on warm black), mapped to xterm-256 indices. 256-colour rather than
// truecolour on purpose — the indices are visually indistinguishable
// from the theme's hexes and they sidestep the question of whether tmux
// negotiates RGB passthrough with the outer terminal.
//
// DIM/MID/BRIGHT are one hue, lightness-monotonic: a valid sequential
// ramp for the dash's activity histogram. PLASMA is the theme's
// attention hue, reserved for accents (the histogram's peak day, the
// factory's hot exhaust).
const (
	Dim    = "\x1b[38;5;94m"  // #875f00 — structure, chrome, "low"
	Mid    = "\x1b[38;5;172m" // #d78700 — idle, mid-ramp, captions
	Bright = "\x1b[38;5;214m" // #ffaf00 — live, top-of-ramp, moe's voice
	Plasma = "\x1b[38;5;202m" // #ff5f00 — heat accent
	Reset  = ansiReset
)

// ansiMoe is Bright plus bold: every moe-originated line's wrap.
const (
	ansiMoe   = "\x1b[1;38;5;214m"
	ansiReset = "\x1b[0m"
)

// Enabled reports whether w is a TTY we should style. Non-file writers
// (test buffers) and NO_COLOR=1 disable styling. Exported so callers
// that compose their own SGR (dash's histogram and factory-art stylers)
// gate on exactly the same predicate these helpers do.
func Enabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return IsTTY(w)
}

// IsTTY reports whether w is a *os.File pointing at a real terminal.
// Test buffers and pipes return false; /dev/null also returns false
// even though it has ModeCharDevice set. NO_COLOR is deliberately
// ignored: callers using this for layout decisions (banner.IndentStderr)
// want the operator's terminal indented even when colour is suppressed.
//
// The /dev/null guard mirrors stdinIsTerminal in internal/cli/init.go,
// where it was load-bearing: an exec.Command-spawned `moe init` gets
// stdin=/dev/null by default on Unix, ModeCharDevice matches, and the
// helper has to additionally rule out the null device via os.SameFile.
// No current caller of IsTTY has been observed pointing at /dev/null,
// but the predicate reads as a general "is this a real terminal?"
// question and the next caller shouldn't have to relearn the lesson.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if nullStat, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, nullStat) {
		return false
	}
	return true
}

// Printf writes a styled line to w.
func Printf(w io.Writer, format string, args ...any) {
	if Enabled(w) {
		fmt.Fprintf(w, ansiMoe+format+ansiReset, args...)
		return
	}
	fmt.Fprintf(w, format, args...)
}

// Println writes s plus a trailing newline, styled.
func Println(w io.Writer, s string) {
	Printf(w, "%s\n", s)
}

// Print writes s without a trailing newline, styled.
func Print(w io.Writer, s string) {
	Printf(w, "%s", s)
}
