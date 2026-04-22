package cli

import (
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
)

func init() {
	Register(&Command{
		Name:    "sync",
		Summary: "sync the bureaucracy repo with origin (git push)",
		Run:     runSync,
	})
}

func runSync(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe sync")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// If the current branch has no upstream configured, push with -u so the
	// first push sets one. After that, plain `git push` is correct and keeps
	// whatever upstream the operator chose.
	pushArgs := []string{"push"}
	if !hasUpstream(root) {
		pushArgs = []string{"push", "-u", "origin", "HEAD"}
	}
	cmd := exec.Command("git", pushArgs...)
	cmd.Dir = root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// git already printed the details; just propagate non-zero.
		return 1
	}
	return 0
}

func hasUpstream(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}
