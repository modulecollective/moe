package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/modulecollective/moe/internal/bureaucracy"
)

func init() {
	Register(&Command{
		Name:    "where",
		Summary: "print resolved bureaucracy path",
		Run:     runWhere,
	})
}

func runWhere(args []string, stdout, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	path, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}
