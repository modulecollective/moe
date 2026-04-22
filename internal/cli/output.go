package cli

import (
	"fmt"
	"io"
	"os"
)

// Cyan SGR around every moe-originated line — passthrough output from git
// (moe push) and claude (moe work) stays unstyled, so the operator can tell
// at a glance which lines are moe talking vs. a subprocess.
const (
	ansiMoe   = "\x1b[1;96m"
	ansiReset = "\x1b[0m"
)

// colorOn reports whether w is a TTY we should style. Non-file writers (test
// buffers) and NO_COLOR=1 disable styling.
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

func moePrintf(w io.Writer, format string, args ...any) {
	if colorOn(w) {
		fmt.Fprintf(w, ansiMoe+format+ansiReset, args...)
		return
	}
	fmt.Fprintf(w, format, args...)
}

func moePrintln(w io.Writer, s string) {
	moePrintf(w, "%s\n", s)
}

func moePrint(w io.Writer, s string) {
	moePrintf(w, "%s", s)
}
