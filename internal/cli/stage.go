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
	"github.com/modulecollective/moe/internal/wiki"
)

// stageSessionOpts carries the per-stage knobs runStageSession needs
// beyond the run identifiers. Most stages just set NeedsSandbox and
// InitialPrompt. Wiki-aware ingest stages (kb summarize, future twin
// stages) supply WikiBuilder so the engine's prompt section, per-turn
// staging, and FinalizeIngest hook all wire up automatically.
type stageSessionOpts struct {
	// NeedsSandbox switches the per-run sandbox clone on. Code stages
	// require it; document-only stages leave it false.
	NeedsSandbox bool
	// InitialPrompt is auto-sent as the session's first user message
	// — typically a "greet the operator and ask what they want"
	// kickoff. Empty drops the auto-send and lands the operator in a
	// blank prompt.
	InitialPrompt string
	// WikiBuilder, when non-nil, is invoked after the bureaucracy
	// root and run metadata are resolved. It returns the wiki engine
	// config for this stage; nil means the stage is not an ingest
	// stage and the wiki integration is skipped. The builder takes
	// the resolved root rather than asking callers to discover it
	// themselves — runStageSession owns root discovery.
	WikiBuilder func(root string, md *run.Metadata) (*wiki.Config, error)
}

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
// opts.InitialPrompt, if non-empty, is auto-sent as the first user
// message of the turn — it's how stages spare the operator from
// typing "go" every time they resume a session. opts.WikiBuilder, if
// non-nil, opts the stage into the wiki engine: an extra system-prompt
// section, per-turn staging of the wiki dir, and FinalizeIngest at
// session close.
func runStageSession(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
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

	// Run-scoped state captured by closure. md is loaded inside
	// buildSpec (after the worktree is open) and referenced again by
	// promptNextStage if the turn lands successfully.
	var md *run.Metadata

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runID,
		DocID:       docID,
		LockPurpose: "stage",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			if opts.WikiBuilder == nil {
				return nil, nil
			}
			return opts.WikiBuilder(canonicalRoot, md)
		},
		// WikiBuilder fires after BuildSpec has populated md. Run-scoped
		// extras (sandbox, prompt, transcript probe) live in BuildSpec.
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			loaded, err := run.Load(workRoot, projectID, runID)
			if err != nil {
				return wikiTurnSpec{}, err
			}
			md = loaded

			doc, mutated, err := run.EnsureDocument(workRoot, md, docID)
			if err != nil {
				return wikiTurnSpec{}, err
			}
			if mutated {
				if err := run.Save(workRoot, md); err != nil {
					return wikiTurnSpec{}, err
				}
				// Commit on the session branch — no repo lock needed
				// because the branch has a single writer (this session).
				if err := commitSessionStart(workRoot, md, docID); err != nil {
					return wikiTurnSpec{}, err
				}
				moePrintf(stderr, "document %q ready (session %s)\n", docID, doc.Session)
			}

			// Code sandbox — still keyed off the canonical bureaucracy
			// root so per-run sandbox persistence works across turns.
			// design=false never sees a clone; code=true insists on
			// one and pre-positions it on the moe/<run-id> branch so
			// the agent's commits (and any later `moe sdlc push`)
			// land on a branch we own.
			clonePath := ""
			if opts.NeedsSandbox {
				if _, err := os.Stat(filepath.Join(root, project.SubmoduleDir(md.Project))); err != nil {
					return wikiTurnSpec{}, fmt.Errorf("project %q has no submodule on disk; cannot run %q without code to edit", md.Project, docID)
				}
				clonePath, err = sandbox.Ensure(root, md.Project, md.ID)
				if err != nil {
					return wikiTurnSpec{}, err
				}
				if err := sandbox.CheckoutBranch(clonePath, branchPrefix+md.ID); err != nil {
					return wikiTurnSpec{}, err
				}
			}

			// mutated means EnsureDocument just minted the session
			// UUID this turn, so the session is definitely new. Even
			// when the document already existed, the local Claude
			// Code session data may have been cleaned up (or moe is
			// running on a different machine). Probe the transcript
			// as a proxy: no transcript → nothing to --resume.
			newSession := mutated
			if !newSession {
				tp, _ := claude.TranscriptPath(doc.Session)
				newSession = tp == ""
			}

			return wikiTurnSpec{
				Metadata:         md,
				DocID:            docID,
				ClonePath:        clonePath,
				SessionUUID:      doc.Session,
				NewSession:       newSession,
				InitialPrompt:    opts.InitialPrompt,
				FinalizeRunID:    md.ID,
				FinalizeRunTitle: md.Title,
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildSystemPrompt(workRoot, md, docID, clonePath, worktreeWiki)
				},
				CommitStager: func(workRoot, wikiRel string) error {
					var extras []string
					if wikiRel != "" {
						extras = append(extras, wikiRel)
					}
					return commitTurn(workRoot, md, docID, extras...)
				},
			}, nil
		},
	}

	code := runWikiSession(root, in, stdout, stderr)
	if code != 0 || md == nil {
		return code
	}
	return promptNextStage(root, md, stdout, stderr)
}

// wikiSessionInputs is everything runWikiSession needs to drive a
// wiki-aware session through its full lifecycle: open the session
// worktree, rewrite the wiki cfg to worktree paths, seed .wiki-ops,
// run the executor, finalize the wiki, commit, and close. The two
// callbacks — WikiBuilder and BuildSpec — defer the work that depends
// on the worktree path (or, for ingest, on the run metadata loaded
// from inside the worktree).
type wikiSessionInputs struct {
	// Project / RunSlug / DocID identify the session worktree branch
	// (`session/<project>/<runslug>/<doc>`). Stage sessions reuse the
	// real run id; lint sessions synthesise one (e.g.
	// "lint-2026-04-27-153022").
	Project string
	RunSlug string
	DocID   string
	// LockPurpose is the repo-lock label prefix; the helper appends
	// "-open" / "-close" for the two short-held windows.
	LockPurpose string
	// WikiBuilder, if non-nil, returns the canonical wiki cfg the
	// helper rewrites to worktree paths. Receives the canonical
	// bureaucracy root. Stage sessions defer until BuildSpec has
	// populated run metadata; lint sessions return the cfg directly.
	// May return nil to opt out of the wiki integration entirely
	// (no .wiki-ops, no FinalizeIngest, no wiki dir staging).
	WikiBuilder func(canonicalRoot string) (*wiki.Config, error)
	// BuildSpec resolves the per-turn parameters once the worktree is
	// open. Errors abort with a stderr report and exit code 1.
	BuildSpec func(workRoot string) (wikiTurnSpec, error)
}

// wikiTurnSpec is the data BuildSpec hands back to runWikiSession.
// Carries everything the executor and commit step need plus the
// pluggable callbacks for prompt assembly and per-turn staging that
// differ between ingest and lint.
type wikiTurnSpec struct {
	// Metadata is the run state, or nil for run-less sessions (lint).
	// Drives transcript mirroring in the executor.
	Metadata *run.Metadata
	// DocID is which document this turn drives — for transcript
	// path. Ignored when Metadata is nil.
	DocID string
	// ClonePath is the sandbox clone working directory. Empty for
	// document-only / lint sessions.
	ClonePath string
	// SessionUUID is the Claude Code session id. Stage sessions reuse
	// the per-document UUID stored in run.json; lint sessions mint a
	// fresh one each invocation.
	SessionUUID string
	// NewSession picks --session-id (true) over --resume (false).
	NewSession bool
	// InitialPrompt, if non-empty, is auto-sent as the first user
	// message of the turn.
	InitialPrompt string
	// FinalizeRunID + FinalizeRunTitle drive the log.md entry header.
	FinalizeRunID    string
	FinalizeRunTitle string
	// FinalizeClaim, when true, signals a closed-schema claim
	// session. The agent appends to log.md themselves; finalize
	// advances the checkpoint without writing a fresh entry.
	FinalizeClaim bool
	// BuildPrompt assembles the --append-system-prompt payload.
	// Receives the worktree root and the worktree-rewritten wiki cfg
	// (nil if the session has no wiki).
	BuildPrompt func(workRoot string, worktreeWiki *wiki.Config) (string, error)
	// CommitStager runs after a successful FinalizeIngest. It
	// receives the worktree root and the wiki dir's path relative to
	// it (or "" if there is no wiki). It owns staging the
	// caller-specific paths and committing with an appropriate
	// message. Returning run.ErrNothingToCommit is treated as a soft
	// empty turn — reported but not fatal.
	CommitStager func(workRoot, wikiRel string) error
}

// runWikiSession owns the full wiki-aware session lifecycle: open the
// session worktree under the repo lock, rewrite the wiki cfg to the
// worktree, seed .wiki-ops, ask the caller for the per-turn spec, run
// the executor, finalize the wiki, commit the turn (via the caller's
// CommitStager), and close the session worktree. Run-scoped extras
// (run.json, EnsureDocument, sandbox, promptNextStage) layer on top
// in runStageSession; lint sessions call the helper directly with no
// run scaffolding. Returns the exit code to bubble up.
func runWikiSession(root string, in wikiSessionInputs, stdout, stderr io.Writer) int {
	// Open (or resume) the session worktree under the repo lock.
	// Short hold: the only work is `git worktree add` (or a lookup).
	var sess *session.Session
	err := withRepoLock(root, repolock.Options{
		Purpose: in.LockPurpose + "-open",
		Run:     in.Project + "/" + in.RunSlug,
	}, func() error {
		s, err := session.Open(root, in.Project, in.RunSlug, in.DocID)
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

	// Caller's setup: load run metadata, configure sandbox, etc.
	// Failures here mean we never reached the executor; close the
	// worktree before returning so we don't leave a dangling branch.
	spec, err := in.BuildSpec(workRoot)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		_ = withRepoLock(root, repolock.Options{
			Purpose: in.LockPurpose + "-close",
			Run:     in.Project + "/" + in.RunSlug,
		}, func() error { return session.Close(sess) })
		return 1
	}

	// Wiki integration — built after BuildSpec so callers that need
	// run metadata (e.g. the kb wiki builder reads md.Project) can
	// resolve it first. The canonical config's ContentDir gets
	// rewritten to live inside the session worktree so prompt paths
	// and engine git-status calls reference the active worktree.
	var wikiCfg *wiki.Config
	if in.WikiBuilder != nil {
		canonical, err := in.WikiBuilder(root)
		if err != nil {
			moePrintf(stderr, "wiki: %v\n", err)
			_ = withRepoLock(root, repolock.Options{
				Purpose: in.LockPurpose + "-close",
				Run:     in.Project + "/" + in.RunSlug,
			}, func() error { return session.Close(sess) })
			return 1
		}
		if canonical != nil {
			worktreeCfg := *canonical
			if rel, relErr := filepath.Rel(root, canonical.ContentDir); relErr == nil && !strings.HasPrefix(rel, "..") {
				worktreeCfg.ContentDir = filepath.Join(workRoot, rel)
			}
			worktreeCfg.BureaucracyPath = workRoot
			wikiCfg = &worktreeCfg
			// Closed-schema bootstrap: create stubs for any managed
			// doc that doesn't yet exist. Runs before EnsureOpsStash
			// so the rest of the turn sees a populated content dir.
			// Open-schema is a no-op.
			if _, err := wiki.EnsureManagedDocs(*wikiCfg); err != nil {
				moePrintf(stderr, "wiki: %v\n", err)
			}
			// Seed the .wiki-ops stash so the agent has a fresh
			// scratchpad. Failure is non-fatal — the log entry
			// degrades to content-edit-only if the stash never
			// materialises.
			if err := wiki.EnsureOpsStash(wikiCfg.ContentDir); err != nil {
				moePrintf(stderr, "wiki: %v\n", err)
			}
		}
	}

	// Prompt paths point at the session worktree, where Claude's
	// edits land. When the session closes, those edits rebase +
	// ff-merge into main at the canonical root.
	prompt, err := spec.BuildPrompt(workRoot, wikiCfg)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		_ = withRepoLock(root, repolock.Options{
			Purpose: in.LockPurpose + "-close",
			Run:     in.Project + "/" + in.RunSlug,
		}, func() error { return session.Close(sess) })
		return 1
	}

	runErr := executor.Execute(executor.Request{
		Root:          workRoot,
		Metadata:      spec.Metadata,
		DocID:         spec.DocID,
		SessionID:     spec.SessionUUID,
		NewSession:    spec.NewSession,
		Prompt:        prompt,
		ClonePath:     spec.ClonePath,
		InitialPrompt: spec.InitialPrompt,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        stderr,
	})

	// Wiki finalize runs before the commit so its writes (log.md
	// and checkpoint.json) ride along in the same per-turn commit
	// as the agent's wiki edits. A no-change session is a no-op —
	// finalize returns without touching disk if the wiki dir is
	// clean. Errors surface but do not block the commit: the
	// agent's edits should land regardless of whether finalization
	// succeeded, so the operator can recover by hand if needed.
	wikiRel := ""
	if wikiCfg != nil {
		_, ferr := wiki.FinalizeIngest(*wikiCfg, wiki.FinalizeContext{
			RunID:    spec.FinalizeRunID,
			RunTitle: spec.FinalizeRunTitle,
			Claim:    spec.FinalizeClaim,
		}, stderr)
		if ferr != nil {
			moePrintf(stderr, "wiki: finalize: %v\n", ferr)
		}
		if rel, err := filepath.Rel(workRoot, wikiCfg.ContentDir); err == nil && !strings.HasPrefix(rel, "..") {
			wikiRel = rel
		}
	}

	// Commit any document changes even if Claude exited non-zero —
	// the operator may have chosen to bail mid-edit but kept the
	// edits.
	var commitErr error
	if spec.CommitStager != nil {
		commitErr = spec.CommitStager(workRoot, wikiRel)
	}

	// Close the session: land it on local main and tear the
	// worktree down. Local-only — origin push is moe sync's job —
	// so a short budget and no heartbeat are fine.
	closeErr := withRepoLock(root, repolock.Options{
		Purpose: in.LockPurpose + "-close",
		Run:     in.Project + "/" + in.RunSlug,
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
		moePrintf(stdout, "committed turn for %s/%s/%s\n", in.Project, in.RunSlug, in.DocID)
	}
	if closeErr != nil {
		moePrintf(stderr, "session close: %v\n", closeErr)
		return 1
	}
	if runErr != nil {
		return 1
	}
	return 0
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
	hint := fmt.Sprintf("moe workflow %s %s %s %s", wf.Name, next.Name, md.Project, md.ID)
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
// (`moe workflow <wf> push`), or PR (`moe workflow <wf> push --pr`).
// Parsing is case-insensitive; the label capitalization just signals
// the default. N-as-default is load-bearing — a reflex Enter must
// never ship.
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
func buildSystemPrompt(root string, md *run.Metadata, docID, clonePath string, wikiCfg *wiki.Config) (string, error) {
	var sections []string

	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}

	if frag := moe.Stage(md.Workflow, docID); frag != "" {
		sections = append(sections, frag)
	}

	// Twin-as-context: every wiki-aware stage gets a reference block
	// pointing at the project's digital-twin/ dir (when one exists).
	// Lands before any wiki-specific section so an ingest agent reads
	// the twin first, then sees the wiki it's working on.
	if ref := wiki.TwinReferenceSectionAt(root, md.Project); ref != "" {
		sections = append(sections, ref)
	}

	sections = append(sections, operationalCore(root, md, docID, clonePath))

	if wikiCfg != nil {
		sections = append(sections, wiki.IngestPromptSection(*wikiCfg))
	}

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

	lastSHA, lastWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, docID)
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
		_, depWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, dep)
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

	// Capture-as-you-go: the close-time harvester turns each unchecked
	// line of this file into an idea run. Worded so the agent appends
	// rather than rewrites — the file accumulates across stages.
	followups := filepath.Join(root, run.FollowupsPath(md.Project, md.ID))
	out += "\n" +
		"If you notice something worth doing but out of scope for this cycle —\n" +
		"adjacent cleanup, a deferred investigation, a reference to chase —\n" +
		"append a line to:\n" +
		"  " + followups + "\n" +
		"Format: - [ ] `slug` — Title (lowercase hyphenated slug, em-dash,\n" +
		"terse title, no body). The operator harvests unchecked entries into\n" +
		"ideas at close.\n"
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
MoE-Workflow: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, md.Workflow, docID, md.Documents[docID].Session)
	err := run.StageAndCommit(root, msg, runJSON)
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
}

// commitTurn stages the document dir and run.json, then commits with
// a trailer block keyed to the document/session. See README §"one run
// branch per run" for the trailer convention.
//
// extraPaths lists additional path specs (relative to root) to stage
// alongside the document dir. Used by ingest stages to ride the wiki
// dir into the same per-turn commit as the canvas, so the operator
// always sees the agent's wiki edits and the canvas snapshot moving
// together in git history.
func commitTurn(root string, md *run.Metadata, docID string, extraPaths ...string) error {
	docDir := run.DocDir(md.Project, md.ID, docID)
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")

	stagePaths := append([]string{docDir}, extraPaths...)
	// followups.md is sibling of run.json — stages append to it as
	// they spot adjacent work to capture. Stage it conditionally so
	// turns that touched neither the doc nor the followups file still
	// trip ErrNothingToCommit cleanly.
	if followupsRel, ok := stageableFollowups(root, md); ok {
		stagePaths = append(stagePaths, followupsRel)
	}
	if err := run.Stage(root, stagePaths...); err != nil {
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
MoE-Workflow: %s
MoE-Document: %s
MoE-Session: %s
`, docID, md.ID, md.Project, md.Workflow, docID, md.Documents[docID].Session)
	allPaths := append([]string{docDir, runJSON}, extraPaths...)
	if followupsRel, ok := stageableFollowups(root, md); ok {
		allPaths = append(allPaths, followupsRel)
	}
	return run.StageAndCommit(root, msg, allPaths...)
}

// stageableFollowups returns the run's followups.md path (relative to
// root) if the file exists, along with true. A missing file means no
// agent or operator has captured anything yet — leave it out of the
// staging set rather than passing a non-existent pathspec to git add.
func stageableFollowups(root string, md *run.Metadata) (string, bool) {
	rel := run.FollowupsPath(md.Project, md.ID)
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		return "", false
	}
	return rel, true
}
