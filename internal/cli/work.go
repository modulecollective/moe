package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
)

var workCmd = &Command{
	Name:    "work",
	Summary: "run the next stage of a request",
	Run:     runWork,
}

func init() {
	Register(workCmd)
}

// runWork is the Command.Run shim. It wires the real stdin and TTY
// probe into the injectable runWorkInternal, which tests call directly.
func runWork(args []string, stdout, stderr io.Writer) int {
	return runWorkInternal(args, os.Stdin, stdinIsTerminal(), stdout, stderr)
}

// runWorkInternal loads a request, asks its workflow what's next, and
// dispatches to the resulting Command. After a stage exits cleanly it
// prompts to continue into the following stage — so a single
// invocation can carry a request from design all the way to push.
//
// Guards the loop is carrying:
//
//   - Non-zero exit from a stage stops the loop with its exit code.
//   - Non-interactive stdin (!isTTY) suppresses the continue prompt —
//     scripts chain via shell `&&` instead.
//   - "No progress" detection: if the next move after a stage run is
//     the same stage (nothing was committed), we exit with a hint
//     rather than risk a re-run loop.
func runWorkInternal(args []string, stdin io.Reader, isTTY bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe work <project> <request>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the first incomplete stage of this request's workflow, then")
		moePrintln(stderr, "offers to continue into the following stage after a clean exit.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, reqID := fs.Arg(0), fs.Arg(1)

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
	md, err := request.Load(root, projectID, reqID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	for {
		next, kind, err := wf.Next(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if kind == NextKindDone {
			moePrintf(stdout, "nothing to do; %s/%s is %s\n", md.Project, md.ID, md.Status)
			return 0
		}

		moePrintf(stdout, "running %s\n", commandInvocation(wf, next, md))
		if code := next.Run([]string{md.Project, md.ID}, stdout, stderr); code != 0 {
			return code
		}

		// Re-load after every step: the stage may have flipped Status
		// (push) or added a work turn that changes what comes next.
		md, err = request.Load(root, projectID, reqID)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		following, followKind, err := wf.Next(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if followKind == NextKindDone {
			return 0
		}
		// Same-stage re-entry signals "the turn produced no commit" —
		// don't loop forever waiting for progress that isn't coming.
		if following.Name == next.Name {
			moePrintf(stdout, "stage %s produced no progress; rerun `moe work %s %s` to retry\n",
				next.Name, md.Project, md.ID)
			return 0
		}
		if !isTTY {
			moePrintf(stdout, "next: %s\n", commandInvocation(wf, following, md))
			return 0
		}
		if !promptYes(stdin, stdout, fmt.Sprintf("Continue to %s? [Y/n] ", following.Name)) {
			moePrintf(stdout, "next: %s\n", commandInvocation(wf, following, md))
			return 0
		}
	}
}

// commandInvocation renders the CLI grammar for a next-move Command.
// Every stage lives under `moe <workflow> <stage>`. Used for both the
// "running" banner and the "next:" hint so operators can copy-paste.
func commandInvocation(wf *Workflow, c *Command, md *request.Metadata) string {
	return fmt.Sprintf("moe %s %s %s %s", wf.Name, c.Name, md.Project, md.ID)
}

// promptYes writes prompt to w and reads one line from r. Treats an
// empty response or anything starting with "y"/"Y" as yes; anything
// else (including a bare "n") as no. EOF is no.
func promptYes(r io.Reader, w io.Writer, prompt string) bool {
	moePrint(w, prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" {
		return true
	}
	return strings.HasPrefix(answer, "y")
}
