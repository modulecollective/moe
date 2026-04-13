package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
)

func init() {
	Register(&Command{
		Name:    "ok",
		Summary: "mark a document as settled (soft gate)",
		Run:     runOk,
	})
	Register(&Command{
		Name:    "unok",
		Summary: "reverse a previous moe ok",
		Run:     runUnok,
	})
}

func runOk(args []string, stdout, stderr io.Writer) int {
	return flipDocStatus(args, stdout, stderr, "ok", "ok")
}

func runUnok(args []string, stdout, stderr io.Writer) int {
	return flipDocStatus(args, stdout, stderr, "draft", "unok")
}

// flipDocStatus is the shared implementation behind `moe ok` and `moe unok`.
// It flips Documents[doc].Status to newStatus, persists request.json, and
// commits with trailers scoped to the request and document.
//
// verb names the operation in user output and the commit subject — "ok" or
// "unok" today; tomorrow could host "review" or other status verbs.
func flipDocStatus(args []string, stdout, stderr io.Writer, newStatus, verb string) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: moe %s <project> <request> <document>\n", verb)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, reqID, docID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

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
	md, err := request.Load(root, projectID, reqID)
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}

	doc, exists := md.Documents[docID]
	if !exists {
		fmt.Fprintf(stderr, "moe: document %q not found in request %s/%s\n", docID, projectID, reqID)
		return 1
	}
	if doc.Status == newStatus {
		fmt.Fprintf(stdout, "%s/%s/%s already %s; no change\n", projectID, reqID, docID, newStatus)
		return 0
	}
	doc.Status = newStatus
	if err := request.Save(root, md); err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}

	msg := fmt.Sprintf(`%s: %s

MoE-Request: %s
MoE-Project: %s
MoE-Document: %s
`, verb, docID, reqID, projectID, docID)
	reqJSON := request.RunDir(projectID, reqID) + "/request.json"
	if err := request.StageAndCommit(root, msg, reqJSON); err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s/%s/%s\n", verb, projectID, reqID, docID)
	return 0
}
