// Package cliout writes moe-originated lines to the operator terminal
// with a cyan SGR wrap when the destination is a TTY and NO_COLOR is
// unset. Passthrough output from subprocesses (git, claude) bypasses
// these helpers and stays unstyled, so the operator can tell at a
// glance which lines are moe talking vs. a subprocess.
//
// Lives in its own package so domain packages (push, sync, dash,
// queue) can emit the same styled progress as cli without importing
// cli or duplicating the SGR-aware writer detection.
package cliout

import (
	"fmt"
	"io"
	"os"
)

const (
	ansiMoe   = "\x1b[1;96m"
	ansiReset = "\x1b[0m"
)

// colorOn reports whether w is a TTY we should style. Non-file writers
// (test buffers) and NO_COLOR=1 disable styling.
func colorOn(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return IsTTY(w)
}

// IsTTY reports whether w is a *os.File pointing at a character device
// (a real terminal). Test buffers and pipes return false. NO_COLOR is
// deliberately ignored: callers using this for layout decisions
// (banner.IndentStderr) want the operator's terminal indented even
// when colour is suppressed.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Printf writes a styled line to w.
func Printf(w io.Writer, format string, args ...any) {
	if colorOn(w) {
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
