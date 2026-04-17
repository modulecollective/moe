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
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/claude"
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

	claudeBin, err := exec.LookPath("claude")
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
	// --add-dir <root> grants the agent native filesystem access to the
	// bureaucracy repo even when cwd is the sandbox clone. The canvas (and,
	// at code stage, upstream documents the banner may point at) live under
	// root, and we want `git -C <root> diff` to work without per-call
	// permission prompts. For document-only projects this is redundant
	// (cwd == root) but harmless.
	cmd := exec.Command(claudeBin,
		sessionFlag, doc.Session,
		"--append-system-prompt", prompt,
		"--add-dir", root,
	)
	// Run Claude from inside the sandbox clone when there is one — that's
	// the same posture a human operator would take (cd into the target repo,
	// open Claude Code there), and it lets Claude Code's own CLAUDE.md
	// discovery pick up whatever guidance the target repo ships. For
	// document-only projects, fall back to the bureaucracy root.
	if clonePath != "" {
		cmd.Dir = clonePath
	} else {
		cmd.Dir = root
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	// Mirror Claude Code's session JSONL into the document dir so the
	// conversation lives in-repo alongside content.md. A missing transcript
	// (operator aborted before claude wrote anything, or the session was
	// resumed on a different machine) is legal — just skip. Other I/O
	// errors get reported but don't block the document commit: the
	// operator's edits are the valuable state.
	threadPath := filepath.Join(root, request.ThreadPath(md.Project, md.ID, docID))
	if _, err := claude.CopyTranscript(doc.Session, threadPath); err != nil {
		moePrintf(stderr, "save transcript: %v\n", err)
	}

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
//	stages/<stage>.md      → lifecycle-phase lens (earliest unsigned)
//	operational core       → what specifically this invocation is doing
//	upstream-change banner → prereq stages that moved since last turn
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

	frag, err := stageFragment(root, md.ID)
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
// stages that were (re-)signed after this document's most recent work turn,
// or "" if there is nothing to surface. The banner names each prerequisite,
// the absolute path to its content.md, and the SHA the agent last ran on,
// so the agent can `git -C <root> diff <sha>..HEAD -- <relpath>` to see what
// changed.
//
// Conditions for firing:
//   - The request has an active stage (one whose prerequisites are all
//     signed and whose own sign is pending). If every stage is signed, work
//     is done; nothing to surface.
//   - The active stage has prerequisites. design has none, so this is a
//     no-op there.
//   - There has been at least one prior work turn for docID. First-turn
//     sessions get no banner — the agent will read prerequisites fresh on
//     its own; there is no "since" to compute against.
//   - At least one prerequisite was MoE-Stage-Signed *after* the last work
//     turn's commit time.
//
// The banner is advisory. Per stages/code.md "Match the design" the
// contract is still social — we're just making the social cue legible
// instead of trusting the agent to notice on its own.
func upstreamChangeBanner(root string, md *request.Metadata, docID string) (string, error) {
	active, ok, err := stage.Active(root, md.ID)
	if err != nil {
		return "", err
	}
	if !ok || len(active.Requires) == 0 {
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
		stage   string
		when    time.Time
		relPath string
	}
	var moved []move
	for _, dep := range active.Requires {
		_, signedWhen, err := stage.LatestSign(root, md.ID, dep)
		if err != nil {
			return "", err
		}
		if signedWhen.IsZero() || !signedWhen.After(lastWhen) {
			continue
		}
		moved = append(moved, move{
			stage:   dep,
			when:    signedWhen,
			relPath: request.ContentPath(md.Project, md.ID, dep),
		})
	}
	if len(moved) == 0 {
		return "", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Since your last turn on %q (bureaucracy commit %s),\n", docID, lastSHA)
	fmt.Fprintf(&b, "the following prerequisite stage(s) for %q were re-signed and may have\n", active.Name)
	b.WriteString("changed under you:\n\n")
	for _, m := range moved {
		fmt.Fprintf(&b, "- %s (signed %s)\n", m.stage, m.when.Format(time.RFC3339))
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
upstream context for downstream agents once the operator signs it.

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
request is signed off.
`, clonePath)
	}
	return out
}

// stageFragment returns the markdown guidance for the lifecycle stage the
// request is currently in, or "" if there's nothing to inject (all stages
// signed, or no candidate is ready). The "which stage is active" walk
// lives in stage.Active; this function is only the file-lookup side.
func stageFragment(root, requestID string) (string, error) {
	active, ok, err := stage.Active(root, requestID)
	if err != nil || !ok {
		return "", err
	}
	return readBureaucracyFile(root, filepath.Join("stages", active.Name+".md"))
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
