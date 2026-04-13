package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
)

func init() {
	Register(&Command{
		Name:    "approve",
		Summary: "approve a request (flip status; v1 stops there)",
		Run:     runApprove,
	})
}

// runApprove is the v1 hard gate: refuses unless every document is `signed`,
// then flips the request status to "approved" and commits. The submodule
// push, derived-artifact generation, and persistent-doc updates described
// in the README are deferred until later phases — this command exists now
// so the lifecycle terminates somewhere meaningful.
func runApprove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe approve <project> <request>")
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

	if md.Status == "approved" {
		moePrintf(stdout, "%s/%s already approved\n", projectID, reqID)
		return 0
	}
	if len(md.Documents) == 0 {
		moePrintf(stderr, "request %s/%s has no documents to approve\n", projectID, reqID)
		return 1
	}

	var unsigned []string
	for id, doc := range md.Documents {
		if doc.Status != "signed" {
			unsigned = append(unsigned, fmt.Sprintf("%s (%s)", id, doc.Status))
		}
	}
	sort.Strings(unsigned)
	if len(unsigned) > 0 {
		moePrintf(stderr, "cannot approve — %d document(s) not signed: %v\n", len(unsigned), unsigned)
		moePrintln(stderr, "run `moe sign <project> <request> <document>` on each first")
		return 1
	}

	md.Status = "approved"
	if err := request.Save(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	msg := fmt.Sprintf(`approve: %s/%s

MoE-Request: %s
MoE-Project: %s
`, projectID, reqID, reqID, projectID)
	reqJSON := request.RunDir(projectID, reqID) + "/request.json"
	if err := request.StageAndCommit(root, msg, reqJSON); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "approved %s/%s\n", projectID, reqID)
	return 0
}
