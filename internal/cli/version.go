package cli

import (
	"io"
	"runtime"
	"runtime/debug"
)

// Version is the moe build version. Overridden at release time via -ldflags.
var Version = "0.0.1-dev"

// moeRevision returns the vcs revision baked into this binary by the
// Go toolchain, falling back to Version when build info carries none
// (go test binaries, -buildvcs=off builds). Eval records stamp this as
// the rubric/guidance version axis: the guidance fragments and the
// eval rubric are embedded assets, so one revision identifies both.
func moeRevision() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return Version
}

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
