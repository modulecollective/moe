package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/executor"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/sandbox"
)

// runStageSession is the core loop shared by `moe sdlc design` and `moe sdlc code`:
// resolve the request/document, hand the operator an interactive Claude Code
// session keyed to that document's session-id, and commit whatever changed
// when Claude exits.
//
// needsSandbox controls the sandbox clone: design=false never gets one,
// code=true always requires one (with a clear error if the project isn't
// registered as a submodule). See README §"moe work" for the broader model.
//
// initialPrompt, if non-empty, is auto-sent as the first user message of
// the turn — it's how stages spare the operator from typing "go" every
// time they resume a session.
func runStageSession(projectID, reqID, docID string, needsSandbox bool, initialPrompt string, stdout, stderr io.Writer) int {
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

	// Sandbox clone: only for stages that actually edit code. design=false
	// never sees a clone; code=true insists on one and pre-positions it on
	// the moe/<request-id> branch so the agent's commits (and any later
	// `moe push`) land on a branch we own.
	clonePath := ""
	if needsSandbox {
		if _, err := os.Stat(filepath.Join(root, "projects", md.Project)); err != nil {
			moePrintf(stderr, "project %q has no submodule on disk; cannot run %q without code to edit\n", md.Project, docID)
			return 1
		}
		clonePath, err = sandbox.Ensure(root, md.Project, md.ID)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if err := sandbox.CheckoutBranch(clonePath, branchPrefix+md.ID); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
	}

	prompt, err := buildSystemPrompt(root, md, docID, clonePath)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// mutated means EnsureDocument just minted the session UUID this
	// turn, so the session is definitely new. But even when the document
	// already existed, the local Claude Code session data may have been
	// cleaned up (or moe is running on a different machine). Probe the
	// transcript as a proxy: no transcript → nothing to --resume.
	newSession := mutated
	if !newSession {
		tp, _ := claude.TranscriptPath(doc.Session)
		newSession = tp == ""
	}

	runErr := executor.ClaudeCLI{}.Execute(executor.Request{
		Root:          root,
		Metadata:      md,
		DocID:         docID,
		SessionID:     doc.Session,
		NewSession:    newSession,
		Prompt:        prompt,
		ClonePath:     clonePath,
		InitialPrompt: initialPrompt,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        stderr,
	})

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

// buildSystemPrompt assembles the `--append-system-prompt` payload in the
// order described in README §"Agent Context Assembly":
//
//	soul.md                → global philosophy / quality bar
//	stages/<stage>.md      → lifecycle-phase lens (for the doc being edited)
//	operational core       → what specifically this invocation is doing
//	upstream-change banner → prereq docs that moved since last turn
//
// Per-document fragments, overrides, and upstream-document assembly are
// expected later passes; each new source of guidance slots in as another
// (string, error)-returning block below.
func buildSystemPrompt(root string, md *request.Metadata, docID, clonePath string) (string, error) {
	var sections []string

	soul, err := readBureaucracyFile(root, "soul.md")
	if err != nil {
		return "", err
	}
	if soul != "" {
		sections = append(sections, soul)
	}

	frag, err := stageFragment(root, docID)
	if err != nil {
		return "", err
	}
	if frag != "" {
		sections = append(sections, frag)
	}

	sections = append(sections, operationalCore(root, md, docID, clonePath))

	banner, err := upstreamChangeBanner(root, md, docID)
	if err != nil {
		return "", err
	}
	if banner != "" {
		sections = append(sections, banner)
	}

	return strings.Join(sections, "\n---\n\n"), nil
}

// upstreamChangeBanner returns a system-prompt section listing prerequisite
// documents that were re-committed after this document's most recent work
// turn, or "" if there is nothing to surface. The banner names each
// prerequisite, the absolute path to its content.md, and the SHA the agent
// last ran on, so the agent can `git -C <root> diff <sha>..HEAD -- <relpath>`
// to see what changed.
//
// Conditions for firing:
//   - docID has prerequisites declared by the request's workflow. design
//     has none in sdlc, so this is a no-op there.
//   - There has been at least one prior work turn for docID. First-turn
//     sessions get no banner — the agent will read prerequisites fresh on
//     its own; there is no "since" to compute against.
//   - At least one prerequisite document had its latest `work: update`
//     commit land *after* the active doc's last work turn.
//
// The banner is advisory. Per stages/code.md "Match the design" the
// contract is still social — we're just making the social cue legible
// instead of trusting the agent to notice on its own.
func upstreamChangeBanner(root string, md *request.Metadata, docID string) (string, error) {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", err
	}
	deps := wf.Prereqs(docID)
	if len(deps) == 0 {
		return "", nil
	}

	lastSHA, lastWhen, err := request.LatestWorkTurnSHA(root, md.ID, docID)
	if err != nil {
		return "", err
	}
	if lastSHA == "" {
		return "", nil
	}

	type move struct {
		doc     string
		when    time.Time
		relPath string
	}
	var moved []move
	for _, dep := range deps {
		_, depWhen, err := request.LatestWorkTurnSHA(root, md.ID, dep)
		if err != nil {
			return "", err
		}
		if depWhen.IsZero() || !depWhen.After(lastWhen) {
			continue
		}
		moved = append(moved, move{
			doc:     dep,
			when:    depWhen,
			relPath: request.ContentPath(md.Project, md.ID, dep),
		})
	}
	if len(moved) == 0 {
		return "", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Since your last turn on %q (bureaucracy commit %s),\n", docID, lastSHA)
	b.WriteString("the following prerequisite document(s) were updated and may have\n")
	b.WriteString("changed under you:\n\n")
	for _, m := range moved {
		fmt.Fprintf(&b, "- %s (updated %s)\n", m.doc, m.when.Format(time.RFC3339))
		fmt.Fprintf(&b, "  document: %s\n", filepath.Join(root, m.relPath))
		fmt.Fprintf(&b, "  diff:     git -C %s diff %s..HEAD -- %s\n", root, lastSHA, m.relPath)
	}
	b.WriteString("\nRe-read the prerequisite document(s) and reconcile your in-progress work\n")
	b.WriteString("before continuing. If the change invalidates the approach, surface it to\n")
	b.WriteString("the operator rather than smuggling a deviation in.\n")
	return b.String(), nil
}

// operationalCore is the "what are you doing right now" framing: canvas
// file, clone workspace (if any), request title. It's the one section
// that's always present — everything else in the prompt is optional
// guidance layered on top.
func operationalCore(root string, md *request.Metadata, docID, clonePath string) string {
	// Absolute path so it resolves regardless of where Claude Code's cwd
	// lands — document-only runs sit at the bureaucracy root, code-editing
	// runs sit inside the sandbox clone.
	content := filepath.Join(root, request.ContentPath(md.Project, md.ID, docID))
	out := fmt.Sprintf(`You are collaborating with the operator on the %q document
for request %q (project %q) in a Ministry of Everything bureaucracy repo.

Your canvas for this document is the single file:
  %s

Treat the conversation as exploratory, and the file as the compressed
artifact. When the operator asks for edits, write them directly to that
file (create it if it doesn't exist). Keep the file tidy — it becomes
upstream context for downstream agents once the operator moves on.

Request title: %s
`, docID, md.ID, md.Project, content, md.Title)

	if clonePath != "" {
		out += fmt.Sprintf(`
Your working directory is a private copy-on-write clone of the target
project's submodule:
  %s
That's your code workspace — read and edit files there. The clone is
yours for the lifetime of this request; your edits are isolated from
other concurrent activities and from the canonical submodule until the
request is pushed.
`, clonePath)
	}
	return out
}

// stageFragment returns the markdown guidance for docID, read from
// stages/<docID>.md, or "" if no fragment exists. Bureaucracies that
// haven't authored a fragment for a given doc just get no injection.
func stageFragment(root, docID string) (string, error) {
	return readBureaucracyFile(root, filepath.Join("stages", docID+".md"))
}

// readBureaucracyFile reads <root>/<relPath> and returns its contents, or
// "" if the file doesn't exist. Used for optional guidance fragments —
// bureaucracies that haven't written one just get no injection. Other I/O
// errors (permissions, unreadable, etc.) are surfaced.
func readBureaucracyFile(root, relPath string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", relPath, err)
	}
	return string(b), nil
}

// commitTurn stages the document dir and request.json, then commits with
// a trailer block keyed to the document/session. See README §"one request
// branch per request" for the trailer convention.
func commitTurn(root string, md *request.Metadata, docID string) error {
	docDir := request.DocDir(md.Project, md.ID, docID)
	reqJSON := filepath.Join(request.RunDir(md.Project, md.ID), "request.json")
	msg := fmt.Sprintf(`work: update %s

MoE-Request: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, docID, md.Documents[docID].Session)
	return request.StageAndCommit(root, msg, docDir, reqJSON)
}
