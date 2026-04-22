package cli

import (
	"io"

	"github.com/modulecollective/moe/internal/termout"
)

// cli's historical helpers delegate to internal/termout so the styling
// logic stays in one place. Other packages that need to print
// moe-originated lines (e.g. internal/executor) import termout directly.

func moePrintf(w io.Writer, format string, args ...any) { termout.Printf(w, format, args...) }
func moePrintln(w io.Writer, s string)                  { termout.Println(w, s) }
func moePrint(w io.Writer, s string)                    { termout.Print(w, s) }
