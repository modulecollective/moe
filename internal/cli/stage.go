package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/executor"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
)

// runStageSession is the core loop shared by `moe sdlc design` and `moe sdlc code`:
// resolve the run/document, hand the operator an interactive Claude Code
// session keyed to that document's session-id, and commit whatever changed
// when Claude exits.
//
// The session runs inside a throwaway git worktree on a branch named
// session/<project>/<run>/<doc>. All per-turn commits (session-start,
// work turn) land on that branch; when Claude exits, the branch is
// rebased onto main, fast-forwarded in, pushed (best-effort) and
// cleaned up. The repo-wide lock is held only during open (short) and
// close (seconds), not across the Claude session itself.
//
// needsSandbox controls the code sandbox: design=false never gets one,
// code=true always requires one (with a clear error if the project isn't
// registered as a submodule). The sandbox lives under the canonical
// bureaucracy root (not the session worktree) so it persists across
// turns.
//
// initialPrompt, if non-empty, is auto-sent as the first user message of
// the turn — it's how stages spare the operator from typing "go" every
// time they resume a session.
func runStageSession(projectID, runID, docID string, needsSandbox bool, initialPrompt string, stdout, stderr io.Writer) int {
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

	// Open (or resume) the session worktree under the repo lock. Short
	// hold: the only work is `git worktree add` (or a lookup).
	var sess *session.Session
	err = withRepoLock(root, repolock.Options{
		Purpose: "stage-open",
		Run:     projectID + "/" + runID,
	}, func() error {
		s, err := session.Open(root, projectID, runID, docID)
		if err != nil {
			return err
		}
		sess = s
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	workRoot := sess.WorktreePath

	md, err := run.Load(workRoot, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	doc, mutated, err := run.EnsureDocument(workRoot, md, docID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if mutated {
		if err := run.Save(workRoot, md); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		// Commit on the session branch — no repo lock needed because
		// the branch has a single writer (this session).
		if err := commitSessionStart(workRoot, md, docID); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		moePrintf(stderr, "document %q ready (session %s)\n", docID, doc.Session)
	}

	// Code sandbox — still keyed off the canonical bureaucracy root so
	// per-run sandbox persistence works across turns. design=false
	// never sees a clone; code=true insists on one and pre-positions it
	// on the moe/<run-id> branch so the agent's commits (and any later
	// `moe sdlc push`) land on a branch we own.
	clonePath := ""
	if needsSandbox {
		if _, err := os.Stat(filepath.Join(root, project.SubmoduleDir(md.Project))); err != nil {
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

	// Prompt paths point at the session worktree, where Claude's edits
	// land. When the session closes, those edits rebase + ff-merge into
	// main at the canonical root.
	prompt, err := buildSystemPrompt(workRoot, md, docID, clonePath)
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

	runErr := executor.Execute(executor.Request{
		Root:          workRoot,
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
	commitErr := commitTurn(workRoot, md, docID)

	// Close the session: land it on local main and tear the worktree
	// down. Local-only — origin push is moe sync's job — so a short
	// budget and no heartbeat are fine.
	closeErr := withRepoLock(root, repolock.Options{
		Purpose: "stage-close",
		Run:     projectID + "/" + runID,
	}, func() error {
		return session.Close(sess)
	})

	if runErr != nil {
		moePrintf(stderr, "claude exited: %v\n", runErr)
		// Fall through to report commit result and exit non-zero.
	}
	switch {
	case errors.Is(commitErr, run.ErrNothingToCommit):
		moePrintln(stdout, "no document changes; nothing committed")
	case commitErr != nil:
		moePrintf(stderr, "commit turn: %v\n", commitErr)
		return 1
	default:
		moePrintf(stdout, "committed turn for %s/%s/%s\n", md.Project, md.ID, docID)
	}
	if closeErr != nil {
		moePrintf(stderr, "session close: %v\n", closeErr)
		return 1
	}
	if runErr != nil {
		return 1
	}
	return promptNextStage(root, md, stdout, stderr)
}

// promptNextStage prints the next incomplete stage's exact invocation
// and, on an interactive terminal, offers to run it in-process. The
// push stage is special-cased: two ship paths (merge/pr) make the
// prompt three-way ([N/m/p]), and N-as-default preserves the rule that
// an external-side-effect stage can't ship on a reflex Enter. All
// other stages keep the Y-default yes/no prompt. Returns the exit
// code to bubble up from the current stage: 0 on skip/decline/successful
// chain, the inner command's exit code if the chained stage fails.
func promptNextStage(root string, md *run.Metadata, stdout, stderr io.Writer) int {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if kind != NextKindStage || next == nil {
		return 0
	}
	hint := fmt.Sprintf("moe %s %s %s %s", wf.Name, next.Name, md.Project, md.ID)
	if !stdinIsTerminal() {
		moePrintf(stdout, "next: %s\n", hint)
		return 0
	}
	switch next.Name {
	case "push":
		return promptPushNextStage(next, md, hint, stdout, stderr)
	}
	moePrintf(stdout, "next: %s — run now? [Y/n] ", hint)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	accepted := answer == "" || strings.HasPrefix(answer, "y")
	if !accepted {
		return 0
	}
	return next.Run([]string{md.Project, md.ID}, stdout, stderr)
}

// promptPushNextStage offers three choices: decline (default), merge
// (`moe sdlc push`), or PR (`moe sdlc push --pr`). Parsing is
// case-insensitive; the label capitalization just signals the default.
// N-as-default is load-bearing — a reflex Enter must never ship.
func promptPushNextStage(next *Command, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	moePrintf(stdout, "next: %s — run now? [N/m/p] ", hint)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch answer {
	case "m":
		return next.Run([]string{md.Project, md.ID}, stdout, stderr)
	case "p":
		return next.Run([]string{"--pr", md.Project, md.ID}, stdout, stderr)
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
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
func buildSystemPrompt(root string, md *run.Metadata, docID, clonePath string) (string, error) {
	var sections []string

	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}

	if frag := moe.Stage(md.Workflow, docID); frag != "" {
		sections = append(sections, frag)
	}

	sections = append(sections, sharedStageFragments(md.Workflow, docID)...)

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
//   - docID has prerequisites declared by the run's workflow. design
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
func upstreamChangeBanner(root string, md *run.Metadata, docID string) (string, error) {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", err
	}
	deps := wf.Prereqs(docID)
	if len(deps) == 0 {
		return "", nil
	}

	lastSHA, lastWhen, err := run.LatestWorkTurnSHA(root, md.ID, docID)
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
		_, depWhen, err := run.LatestWorkTurnSHA(root, md.ID, dep)
		if err != nil {
			return "", err
		}
		if depWhen.IsZero() || !depWhen.After(lastWhen) {
			continue
		}
		moved = append(moved, move{
			doc:     dep,
			when:    depWhen,
			relPath: run.ContentPath(md.Project, md.ID, dep),
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

// sharedStageFragments returns the cross-workflow guidance blocks that
// apply to Claude-driven stages. They live under stages/_shared/ and
// are appended after the per-stage fragment. Today only sdlc/design
// and sdlc/code receive them; idea-mode prompts are intentionally left
// alone. Order is stable: completeness first (a gate the agent should
// run before anything else), then cross-run (a rule that scopes where
// writes are allowed).
func sharedStageFragments(workflow, docID string) []string {
	if workflow != "sdlc" || (docID != "design" && docID != "code") {
		return nil
	}
	var out []string
	for _, name := range []string{"completeness", "cross-run"} {
		if frag := moe.Stage("_shared", name); frag != "" {
			out = append(out, frag)
		}
	}
	return out
}

// operationalCore is the "what are you doing right now" framing: canvas
// file, clone workspace (if any), run title. It's the one section
// that's always present — everything else in the prompt is optional
// guidance layered on top.
func operationalCore(root string, md *run.Metadata, docID, clonePath string) string {
	// Absolute path so it resolves regardless of where Claude Code's cwd
	// lands — document-only runs sit at the bureaucracy root, code-editing
	// runs sit inside the sandbox clone.
	content := filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))
	out := fmt.Sprintf(`You are collaborating with the operator on the %q document
for run %q (project %q) in a Ministry of Everything bureaucracy repo.

Your canvas for this document is the single file:
  %s

Treat the conversation as exploratory, and the file as the compressed
artifact. When the operator asks for edits, write them directly to that
file (create it if it doesn't exist). Keep the file tidy — it becomes
upstream context for downstream agents once the operator moves on.

Run title: %s
`, docID, md.ID, md.Project, content, md.Title)

	if clonePath != "" {
		out += fmt.Sprintf(`
Your working directory is a private copy-on-write clone of the target
project's submodule:
  %s
That's your code workspace — read and edit files there. The clone is
yours for the lifetime of this run; your edits are isolated from
other concurrent activities and from the canonical submodule until the
run is pushed.
`, clonePath)
	}
	return out
}

// commitSessionStart commits run.json immediately after EnsureDocument
// mints a fresh Claude session UUID, so the long Claude run that follows
// doesn't leave the bureaucracy tree dirty for hours. Only run.json is
// staged — any unrelated edits the operator had in the tree stay put.
//
// ErrNothingToCommit is tolerated silently: the caller only reaches this
// path when mutated=true, so run.json is expected to differ from HEAD,
// but if some concurrent action already committed the identical state
// there's no work to do and no reason to fail the turn.
func commitSessionStart(root string, md *run.Metadata, docID string) error {
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf(`work: start session for %s

MoE-Run: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, docID, md.Documents[docID].Session)
	err := run.StageAndCommit(root, msg, runJSON)
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
}

// commitTurn stages the document dir and run.json, then commits with
// a trailer block keyed to the document/session. See README §"one run
// branch per run" for the trailer convention.
func commitTurn(root string, md *run.Metadata, docID string) error {
	docDir := run.DocDir(md.Project, md.ID, docID)
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")

	if err := run.Stage(root, docDir); err != nil {
		return err
	}
	if !run.HasStagedChanges(root) {
		return run.ErrNothingToCommit
	}

	if err := run.Save(root, md); err != nil {
		return err
	}

	msg := fmt.Sprintf(`work: update %s

MoE-Run: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, docID, md.Documents[docID].Session)
	return run.StageAndCommit(root, msg, docDir, runJSON)
}
