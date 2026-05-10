package cli

import (
	"io"

	"github.com/modulecollective/moe/internal/cliout"
)

// moePrintf, moePrintln, and moePrint forward to cliout — the cli
// package's longstanding call sites kept their existing names while
// the styled-output implementation moved to a shared package the
// extracted domain packages (push, sync, dash, queue) can import.

func moePrintf(w io.Writer, format string, args ...any) {
	cliout.Printf(w, format, args...)
}

func moePrintln(w io.Writer, s string) {
	cliout.Println(w, s)
}

func moePrint(w io.Writer, s string) {
	cliout.Print(w, s)
}
