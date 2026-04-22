// Package termout prints moe-originated lines in a consistent cyan
// SGR so the operator can tell at a glance which lines come from moe
// and which are passthrough output from subprocesses (git, claude).
// Non-TTY writers and NO_COLOR=1 fall back to plain text, which keeps
// test buffers and piped output clean.
package termout

import (
	"fmt"
	"io"
	"os"
)

const (
	ansiMoe   = "\x1b[1;96m"
	ansiReset = "\x1b[0m"
)

// Printf writes a moe-styled line to w. The styling wraps the whole
// formatted string, not just the prefix, so multi-line messages stay
// visually coherent.
func Printf(w io.Writer, format string, args ...any) {
	if colorOn(w) {
		fmt.Fprintf(w, ansiMoe+format+ansiReset, args...)
		return
	}
	fmt.Fprintf(w, format, args...)
}

// Println is Printf with a trailing newline and no format args.
func Println(w io.Writer, s string) { Printf(w, "%s\n", s) }

// Print is Printf with no format args and no trailing newline.
func Print(w io.Writer, s string) { Printf(w, "%s", s) }

// colorOn reports whether w is a TTY we should style. Non-file writers
// (test buffers) and NO_COLOR=1 disable styling.
func colorOn(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
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
