package cli

import (
	"io"
	"time"

	"github.com/modulecollective/moe/internal/servepause"
)

func runServePause(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		moePrintln(stderr, "usage: moe serve pause|unpause|status")
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	var s servepause.State
	if args[0] == "status" {
		s, err = servepause.Read(root)
	} else {
		s, err = servepause.Set(root, args[0] == "pause", nil, time.Now)
	}
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintln(stdout, s.Summary(time.Now()))
	return 0
}
