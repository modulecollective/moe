package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
)

func init() {
	Register(&Command{
		Name:    "init",
		Summary: "scaffold a new bureaucracy repo in the current directory",
		Run:     runInit,
	})
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	remote := fs.String("remote", "", "git URL to set as origin (optional)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe init [--remote <url>] [dir]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var dir string
	switch fs.NArg() {
	case 0:
		// No explicit target — prefer $MOE_HOME if set, else cwd.
		// $MOE_HOME pointing at a not-yet-bureaucracy directory is the exact
		// "I want to init there" signal.
		if home := os.Getenv(bureaucracy.EnvHome); home != "" {
			dir = home
		} else {
			dir = "."
		}
	case 1:
		dir = fs.Arg(0)
	default:
		fs.Usage()
		return 2
	}
	if err := bureaucracy.Init(dir, *remote); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	abs, _ := filepath.Abs(dir)
	moePrintf(stdout, "initialized bureaucracy at %s\n", abs)
	moePrintln(stdout, "staged: bureaucracy.conf, projects/.gitkeep")

	if !stdinIsTerminal() {
		moePrintln(stdout, "not a terminal — leaving staged; commit when ready.")
		return 0
	}
	return promptInitCommit(abs, stdout, stderr)
}

// promptInitCommit asks whether to commit the freshly scaffolded
// bureaucracy, and commits it on Y. Blank (a reflex Enter) accepts;
// `n` or any other text leaves the tree staged; both abort keys —
// Ctrl-C, and a bare Ctrl-D (EOF with nothing typed) — take the same
// path as `n`, because an abort key must not perform the state change
// the default would. An answer typed before the EOF still counts as
// an answer.
//
// Caller responsibility: gate on stdinIsTerminal() before invoking —
// the same contract the stage_next.go chain prompts document. Split
// out of runInit so a test can drive the answer through a pipe
// without tripping that gate.
func promptInitCommit(abs string, stdout, stderr io.Writer) int {
	moePrint(stdout, "commit now? [Y/n] ")
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	declined := answer != "" && !strings.HasPrefix(answer, "y")
	if interrupted {
		// Echo the abort the way the chain prompts do. The newline it
		// carries also breaks the line off the trailing "[Y/n] ", so
		// the "left staged" message below lands on its own.
		moePrintln(stdout, "^C")
		declined = true
	}
	if err == io.EOF && line == "" {
		// A terminal echoes nothing for Ctrl-D and the prompt above
		// has no trailing newline — break the line so the message
		// lands on its own rather than trailing "[Y/n] ".
		fmt.Fprintln(stdout)
		declined = true
	}
	if declined {
		moePrintln(stdout, "left staged; commit when ready.")
		return 0
	}
	if err := git.Stream(abs, stdout, stderr, "commit", "-m", "Initialize bureaucracy"); err != nil {
		moePrintf(stderr, "git commit: %v\n", err)
		return 1
	}
	return 0
}

// stdinIsTerminal reports whether os.Stdin is attached to an
// interactive terminal — not a pipe, file, or the null device.
// Stdlib-only (principle 11): ModeCharDevice covers TTYs but also
// matches /dev/null, so we additionally rule that out via os.SameFile
// against os.DevNull. /dev/null is the load-bearing false positive in
// practice (an exec.Command with no Stdin gets it on Unix, as do many
// hook runners); other char devices like /dev/zero aren't real
// surfaces for moe-init.
func stdinIsTerminal() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	if stat.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if nullStat, err := os.Stat(os.DevNull); err == nil && os.SameFile(stat, nullStat) {
		return false
	}
	return true
}
