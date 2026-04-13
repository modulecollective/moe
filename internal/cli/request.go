package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
)

func init() {
	Register(&Command{
		Name:    "request",
		Summary: "manage requests (subcommands: new)",
		Run:     runRequest,
	})
}

func runRequest(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: moe request <subcommand> [args...]")
		fmt.Fprintln(stderr, "subcommands: new")
		return 2
	}
	switch args[0] {
	case "new":
		return runRequestNew(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "moe: unknown request subcommand %q\n", args[0])
		return 2
	}
}

func runRequestNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("request new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: moe request new [--id <slug>] <project> "title"`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	project := fs.Arg(0)
	// Join remaining args so an unquoted multi-word title still works.
	title := strings.Join(fs.Args()[1:], " ")

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	md, err := request.New(root, project, title, request.Options{ID: *idOverride})
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "opened request %s/%s\n", md.Project, md.ID)
	return 0
}
