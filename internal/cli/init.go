package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
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
		fmt.Fprintln(stderr, "usage: moe init [--remote <url>] [dir]")
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
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	abs, _ := filepath.Abs(dir)
	fmt.Fprintf(stdout, "initialized bureaucracy at %s\n", abs)
	fmt.Fprintln(stdout, "staged: bureaucracy.conf, projects/.gitkeep, requests/.gitkeep")

	if !stdinIsTerminal() {
		fmt.Fprintln(stdout, "not a terminal — leaving staged; commit when ready.")
		return 0
	}
	fmt.Fprint(stdout, "commit now? [Y/n] ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintf(stderr, "moe: read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "" && !strings.HasPrefix(answer, "y") {
		fmt.Fprintln(stdout, "left staged; commit when ready.")
		return 0
	}
	cmd := exec.Command("git", "commit", "-m", "Initialize bureaucracy")
	cmd.Dir = abs
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "moe: git commit: %v\n", err)
		return 1
	}
	return 0
}

// stdinIsTerminal reports whether os.Stdin is attached to a character
// device — i.e. an interactive terminal, not a pipe or file. Stdlib-only
// (principle 11) via the ModeCharDevice bit.
func stdinIsTerminal() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}
