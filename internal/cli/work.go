package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/stage"
)

func init() {
	Register(&Command{
		Name:    "work",
		Summary: "open a Claude Code session on a request document",
		Run:     runWork,
	})
}

// runWork is the core loop: resolve the request/document, hand the operator
// an interactive Claude Code session keyed to that document's session-id,
// and commit whatever changed when Claude exits. See README §"moe work".
func runWork(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe work <project> <request> <document-name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "<document-name> is a slug like 'spec', 'architecture', or 'implementation'.")
		moePrintln(stderr, "First use on a name creates the document; re-runs resume the same Claude session.")
		moePrintln(stderr, "Example: moe work loveletter395 fix-timeout spec")
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

	doc, mutated, err := request.EnsureDocument(root, md, docID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if mutated {
		if err := request.Save(root, md); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		moePrintf(stderr, "document %q ready (session %s)\n", docID, doc.Session)
	}

	claude, err := exec.LookPath("claude")
	if err != nil {
		moePrintf(stderr, "claude CLI not found on PATH: %v\n", err)
		return 1
	}

	// Every `moe work` gets a private copy-on-write clone of the project's
	// submodule. First turn creates it; later turns reuse the same clone so
	// session state in the target repo (branches, uncommitted edits) persists
	// across invocations. Document-only projects (no projects/<id>/ on disk)
	// silently skip this — the feature only applies where there's code to
	// isolate.
	clonePath := ""
	if _, err := os.Stat(filepath.Join(root, "projects", md.Project)); err == nil {
		clonePath, err = sandbox.Ensure(root, md.Project, md.ID)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
	}

	prompt, err := buildSystemPrompt(root, md, docID, clonePath)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Claude Code uses --session-id to *create* a session and --resume to
	// continue one. EnsureDocument set mutated=true exactly when the UUID
	// was freshly minted (new doc, or healed from an invalid session id),
	// which is the same condition as "no server-side session yet."
	sessionFlag := "--resume"
	if mutated {
		sessionFlag = "--session-id"
	}
	cmd := exec.Command(claude,
		sessionFlag, doc.Session,
		"--append-system-prompt", prompt,
	)
	cmd.Dir = root
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	// Commit any document changes even if Claude exited non-zero — the
	// operator may have chosen to bail mid-edit but kept the edits.
	commitErr := commitTurn(root, md, docID)

	if runErr != nil {
		moePrintf(stderr, "claude exited: %v\n", runErr)
		// Fall through to report commit result and exit non-zero.
	}
	switch {
	case errors.Is(commitErr, request.ErrNothingToCommit):
		moePrintln(stdout, "no document changes; nothing committed")
	case commitErr != nil:
		moePrintf(stderr, "commit turn: %v\n", commitErr)
		return 1
	default:
		moePrintf(stdout, "committed turn for %s/%s/%s\n", md.Project, md.ID, docID)
	}
	if runErr != nil {
		return 1
	}
	return 0
}

// buildSystemPrompt is the v1 context injection — just enough for Claude to
// know which file to treat as the document, which clone to treat as the
// target-code workspace (when the project has a submodule), and which
// lifecycle-stage guidance applies. Upstream-document assembly, per-document
// fragments, and soul.md layering come later.
func buildSystemPrompt(root string, md *request.Metadata, docID, clonePath string) (string, error) {
	content := request.ContentPath(md.Project, md.ID, docID)
	prompt := fmt.Sprintf(`You are collaborating with the operator on the %q document
for request %q (project %q) in a Ministry of Everything bureaucracy repo.

Your canvas for this document is the single file:
  %s

Treat the conversation as exploratory, and the file as the compressed
artifact. When the operator asks for edits, write them directly to that
file (create it if it doesn't exist). Keep the file tidy — it becomes
upstream context for downstream agents once the operator signs it.

Request title: %s
`, docID, md.ID, md.Project, content, md.Title)

	if clonePath != "" {
		prompt += fmt.Sprintf(`
For any work that touches the target project's code, the workspace is a
private copy-on-write clone of the submodule at:
  %s
Read and edit files there, not under projects/%s/. The clone is yours for
the lifetime of this request — your edits are isolated from other concurrent
activities and from the canonical submodule until the request is signed off.
`, clonePath, md.Project)
	}

	frag, err := stageFragment(root, md.ID)
	if err != nil {
		return "", err
	}
	if frag != "" {
		prompt += "\n---\n\n" + frag
	}
	return prompt, nil
}

// stageFragment returns the markdown guidance for the lifecycle stage the
// request is currently in, or "" if there's nothing to inject.
//
// Only the design stage is wired up today: it applies from request open
// until `pr` is signed. When a pr-stage (or later) fragment lands, add a
// branch here — the mechanic is deliberately a chain of explicit checks
// rather than a table, so each stage's applicability rule stays readable.
//
// A missing fragment file is not an error. Guidance fragments are optional;
// bureaucracies that haven't written one just get no fragment.
func stageFragment(root, requestID string) (string, error) {
	prSigned, err := stage.IsSigned(root, requestID, "pr")
	if err != nil {
		return "", err
	}
	if prSigned {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(root, "stages", "design.md"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read stages/design.md: %w", err)
	}
	return string(b), nil
}

// commitTurn stages the document dir and request.json, then commits with
// a trailer block keyed to the document/session. See README §"one request
// branch per request" for the trailer convention.
func commitTurn(root string, md *request.Metadata, docID string) error {
	docDir := request.DocDir(md.Project, md.ID, docID)
	reqJSON := request.RunDir(md.Project, md.ID) + "/request.json"
	msg := fmt.Sprintf(`work: update %s

MoE-Request: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, docID, md.Documents[docID].Session)
	return request.StageAndCommit(root, msg, docDir, reqJSON)
}
