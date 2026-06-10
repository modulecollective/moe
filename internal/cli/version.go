package cli

import (
	"io"
	"runtime"
)

// Version is the moe build version. Overridden at release time via -ldflags.
var Version = "0.0.1-dev"

func init() {
	Register(&Command{
		Name:    "version",
		Summary: "print moe version",
		Run:     runVersion,
	})
	Register(&Command{
		Name:    "help",
		Summary: "print usage",
		Run:     runHelp,
	})
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	moePrintf(stdout, "moe %s %s/%s %s\n", Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return 0
}

func runHelp(args []string, stdout, stderr io.Writer) int {
	PrintUsage(stdout)
	return 0
}
