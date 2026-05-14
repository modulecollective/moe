// Package cli, stage.go: the runStageSession orchestration that
// wraps `moe sdlc design`, `moe sdlc code`, and the rest of the
// per-stage entry points around a worktree-on-branch session.
//
// The chain prompts (promptNextStage / promptStageNextStage /
// promptPushNextStage) live in stage_next.go. System-prompt
// assembly (buildSystemPrompt / operationalCore /
// upstreamChangeBanner) lives in stage_prompt.go. Per-turn commits
// (commitSessionStart / commitTurn / stageableFollowups) live in
// stage_commit.go. This file owns the session worktree dance —
// open under lock, hand to executor, finalize wiki, commit turn,
// close under lock — that ties the others together.
package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/claude"
	"github.com/modulecollective/moe/internal/executor"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/wiki"
)

// oneShotPromptDelimiter separates the assembled stage system prompt
// from the appended one-shot addendum, matching the section delimiter
// buildSystemPrompt uses internally.
const oneShotPromptDelimiter = "\n---\n\n"

// inCascade is set by cascadeFromGate while the chain-prompt cascade
// driver is walking stages. runStageSession reads it as an OR with
// opts.SkipNextStage to suppress the inner promptNextStage call —
// the cascade owns routing, and re-firing the prompt inside each
// stage would either deadlock at the next gate (no operator stdin)
// or double-prompt. Scoped via defer in the cascade driver.
var inCascade bool

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
	// blank prompt. In Headless mode it's the entire user turn for
	// `claude -p` — typically the run title.
	InitialPrompt string
	// Headless drives the stage as a one-turn `claude -p` call instead
	// of an interactive REPL. Output streams to the operator's
	// terminal (no stdin), the workflow's oneshot.md fragment is
	// appended to the system prompt, and transcript mirroring is
	// skipped (the canvas + per-turn commit are the durable
	// artifacts). Set by `moe sdlc new --one-shot`.
	Headless bool
	// SkipNextStage suppresses the post-turn "next: …" prompt /
	// chained-stage call. Used by one-shot, which composes its own
	// chain (design → code) and never wants the interactive next-stage
	// prompt to fire mid-chain.
	SkipNextStage bool
	// CanvasSkeleton, when non-empty, is written to the canvas file the
	// first time the document is opened (the EnsureDocument-mutated
	// branch). Lets stages with a fixed structural canvas — test stage's
	// "What was verified / What wasn't verified / Fixes applied /
	// Operator spot-check" headings — seed the agent's first read with
	// the shape it has to fill, instead of relying on the prompt
	// fragment alone. Skipped on resume turns.
	CanvasSkeleton string
	// WikiBuilder, when non-nil, is invoked after the bureaucracy
	// root and run metadata are resolved. It returns the wiki engine
	// config for this stage; nil means the stage is not an ingest
	// stage and the wiki integration is skipped. The builder takes
	// the resolved root rather than asking callers to discover it
	// themselves — runStageSession owns root discovery.
	WikiBuilder func(root string, md *run.Metadata) (*wiki.Config, error)
	// ExtraStagePaths, when non-nil, runs after the agent session
	// ends and before commitTurn. It receives the session worktree
	// root and the run metadata; it may write files inside the
	// worktree (e.g. publish a synthesized artifact) and returns
	// extra path specs (relative to workRoot) to stage in the same
	// per-turn commit. Used by meta-moe to copy the report canvas to
	// projects/<p>/meta-moe.md so the project-root snapshot rides
	// alongside the per-pass canvas in one commit.
	ExtraStagePaths func(workRoot string, md *run.Metadata) ([]string, error)
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
//
// Declared as a var so the chain-back closures (hooks.go,
// push.go) can be exercised end-to-end in tests without spinning a
// real session worktree. Production callers see no difference.
var runStageSession = func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int {
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

	// Run-scoped state captured by closure. md is pre-loaded from the
	// canonical root so the entry banner can name md.Workflow before
	// the session worktree opens; BuildSpec uses the same pointer and
	// promptNextStage reads it after the executor returns. Loading
	// from `root` rather than the worktree is safe — run.json doesn't
	// drift on `git worktree add`.
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	banner.StageEntry(stdout, md.Workflow, docID, md.Project, md.ID)
	// committed flips true when CommitStager returns a clean nil —
	// the same branch reportWikiSessionExit treats as "committed turn".
	// A ErrNothingToCommit return leaves it false so the exit footer
	// reads `no-op`. Other commit errors short-circuit before the
	// footer fires (non-zero exit code below).
	var committed bool

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
		// md is pre-loaded at runStageSession entry; BuildSpec rides on
		// the same pointer rather than re-reading run.json from the
		// worktree, which is identical content. Run-scoped extras
		// (sandbox, prompt, transcript probe) still resolve here.
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			doc, mutated, err := run.EnsureDocument(workRoot, md, docID)
			if err != nil {
				return wikiTurnSpec{}, err
			}
			if mutated {
				if err := run.Save(workRoot, md); err != nil {
					return wikiTurnSpec{}, err
				}
				// Seed the canvas skeleton on first open if requested —
				// stages with a fixed structural shape (test stage) want
				// the agent's first read to land on the headings it has
				// to fill, not a blank file. Only writes if the canvas
				// doesn't already exist on disk: a pre-existing canvas
				// from a stale stub or test fixture stays untouched.
				if opts.CanvasSkeleton != "" {
					canvasRel := run.ContentPath(md.Project, md.ID, docID)
					canvasAbs := filepath.Join(workRoot, canvasRel)
					if _, statErr := os.Stat(canvasAbs); errors.Is(statErr, fs.ErrNotExist) {
						if err := os.WriteFile(canvasAbs, []byte(opts.CanvasSkeleton), 0o644); err != nil {
							return wikiTurnSpec{}, fmt.Errorf("session: seed canvas skeleton: %w", err)
						}
					}
				}
				// Commit on the session branch — no repo lock needed
				// because the branch has a single writer (this session).
				if err := commitSessionStart(workRoot, md, docID); err != nil {
					return wikiTurnSpec{}, err
				}
				moePrintf(stderr, "opened %s canvas (session %s)\n  %s\n", docID, doc.Session, filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID)))
			}

			// Code workspace — still keyed off the canonical bureaucracy
			// root so per-run sandbox persistence works across turns.
			// design=false never sees a clone; code=true insists on one
			// and pre-positions it on the moe/<run-id> branch so the
			// agent's commits (and any later `moe sdlc push`) land on a
			// branch we own. attachRunWorkspace routes per-run sandbox
			// vs named workspace based on md.Workspace; the callers
			// here don't need to know which.
			clonePath := ""
			var devEnv map[string]string
			if opts.NeedsSandbox {
				if _, err := os.Stat(filepath.Join(root, project.SubmoduleDir(md.Project))); err != nil {
					return wikiTurnSpec{}, fmt.Errorf("project %q has no submodule on disk; cannot run %q without code to edit", md.Project, docID)
				}
				clonePath, err = attachRunWorkspace(root, md, branchPrefix+md.ID)
				if err != nil {
					return wikiTurnSpec{}, err
				}
				// Dev-env hooks fire on every code/test stage open
				// against this working tree. First touch runs the
				// project's dev-env.d/* setup scripts and caches the
				// parsed KEY=VALUE output to <tree>/.moe/dev-env.env;
				// later turns re-source the cache. Projects with no
				// dev-env.d/ directory get an empty env (the
				// single-driver default) — no warning, no refusal.
				env, _, err := devEnvSetupEnv(root, clonePath, md, stdout, stderr)
				if err != nil {
					return wikiTurnSpec{}, fmt.Errorf("dev-env: %w", err)
				}
				devEnv = env
			}

			// Document-only stages need a cwd that's stable across
			// turns so claude's encoded-cwd project dir doesn't churn
			// and `--resume <sid>` can find the JSONL it wrote on
			// turn 1. The session worktree is per-turn (UUID), so
			// using workRoot would re-break it. Code stages have
			// ClonePath, which is already stable.
			sessionCwd := ""
			if clonePath == "" {
				sessionCwd = sessionDocCwd(root, md.Project, md.ID, docID)
				if err := os.MkdirAll(sessionCwd, 0o755); err != nil {
					return wikiTurnSpec{}, fmt.Errorf("session: mkdir %s: %w", sessionCwd, err)
				}
			}

			// mutated means EnsureDocument just minted the session
			// UUID this turn — fresh session, nothing to validate.
			// Otherwise stat the exact path claude will read for
			// `--resume <sid>` from the cwd it'll run in (clonePath
			// for code stages, sessionCwd for document-only — the
			// same precedence the executor uses for cmd.Dir) and
			// decide between two outcomes:
			//   - JSONL at the canonical path → resume normally.
			//   - JSONL absent (cross-machine fresh checkout, wiped
			//     cache, dirty exit before claude wrote turn 1, or
			//     a prior --one-shot turn which doesn't honor moe's
			//     --session-id) → re-mint the session id, persist +
			//     commit run.json, and pass --session-id instead of
			//     --resume. Chat history is gone but the canvas on
			//     disk is intact; we warn on stderr.
			// Pre-flighting beats letting claude error mid-run: the
			// operator gets a clear stderr line, not a stuck run.
			newSession := mutated
			if !newSession {
				resumeCwd := clonePath
				if resumeCwd == "" {
					resumeCwd = sessionCwd
				}
				if resumeCwd != "" {
					canonical := claude.CanonicalTranscriptPath(resumeCwd, doc.Session)
					if canonical != "" {
						switch _, statErr := os.Stat(canonical); {
						case statErr == nil:
							// At the canonical path — normal --resume.
						case errors.Is(statErr, fs.ErrNotExist):
							moePrintf(stderr, "session %s not found; starting fresh (prior chat history not recoverable)\n", doc.Session)
							sid, err := run.NewSessionID()
							if err != nil {
								return wikiTurnSpec{}, err
							}
							doc.Session = sid
							if err := run.Save(workRoot, md); err != nil {
								return wikiTurnSpec{}, err
							}
							if err := commitSessionStart(workRoot, md, docID); err != nil {
								return wikiTurnSpec{}, err
							}
							newSession = true
						default:
							return wikiTurnSpec{}, fmt.Errorf("session: stat transcript: %w", statErr)
						}
					}
				}
			}

			// Headless mode has no operator on stdin to type the seed
			// prompt, so default it to the run title — the same shape
			// `moe sdlc new --one-shot` has been seeding by hand.
			// Callers that pass an explicit InitialPrompt keep theirs.
			initialPrompt := opts.InitialPrompt
			if opts.Headless && initialPrompt == "" {
				initialPrompt = md.Title
			}

			return wikiTurnSpec{
				Metadata:         md,
				DocID:            docID,
				ClonePath:        clonePath,
				SessionCwd:       sessionCwd,
				SessionUUID:      doc.Session,
				NewSession:       newSession,
				InitialPrompt:    initialPrompt,
				Headless:         opts.Headless,
				FinalizeRunID:    md.ID,
				FinalizeRunTitle: md.Title,
				ExtraEnv:         mapToEnv(devEnv),
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					p, err := buildSystemPrompt(workRoot, md, docID, clonePath, worktreeWiki)
					if err != nil {
						return "", err
					}
					if opts.Headless {
						if frag := moe.OneShot(md.Workflow); frag != "" {
							p += oneShotPromptDelimiter + frag
						}
					}
					return p, nil
				},
				CommitStager: func(workRoot, wikiRel string) error {
					var extras []string
					if wikiRel != "" {
						extras = append(extras, wikiRel)
					}
					if opts.ExtraStagePaths != nil {
						more, err := opts.ExtraStagePaths(workRoot, md)
						if err != nil {
							return err
						}
						extras = append(extras, more...)
					}
					err := commitTurn(workRoot, md, docID, extras...)
					if err == nil {
						committed = true
					}
					return err
				},
			}, nil
		},
	}

	code := runWikiSession(root, in, stdout, stderr)
	if code != 0 {
		// Error exit — skip the footer. Pairing every error with a
		// "complete" footer would be worse than the asymmetry, and the
		// entry banner is still in scrollback so the operator can
		// locate where things went wrong.
		return code
	}
	banner.StageExit(stdout, md.Workflow, docID, md.Project, md.ID, committed)
	if opts.SkipNextStage || inCascade {
		return 0
	}
	return promptNextStage(root, md, docID, stdout, stderr)
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
	// SessionCwd is the document-only fallback cwd — a stable
	// per-document path under <root>/.moe/sessions/. Empty for code
	// stages (ClonePath wins) and for run-less / lint sessions, which
	// can keep using the worktree root since they don't `--resume`.
	SessionCwd string
	// SessionUUID is the Claude Code session id. Stage sessions reuse
	// the per-document UUID stored in run.json; lint sessions mint a
	// fresh one each invocation.
	SessionUUID string
	// NewSession picks --session-id (true) over --resume (false).
	NewSession bool
	// InitialPrompt, if non-empty, is auto-sent as the first user
	// message of the turn. In Headless mode it is the entire `claude
	// -p` user prompt.
	InitialPrompt string
	// Headless flips runWikiSession from the interactive REPL path
	// (executor.Execute) to the one-shot streaming path
	// (executor.ExecuteOneShot): no stdin, no transcript mirror, exits
	// after one turn. The rest of the lifecycle — open session
	// worktree, prompt assembly, commitTurn, close — is unchanged.
	Headless bool
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
	// PreFinalizeGate, when non-nil, runs after the executor returns
	// and before FinalizeIngest. Returning a non-nil error skips
	// FinalizeIngest *and* CommitStager (no log entry, no commit, no
	// checkpoint bump) and forces a non-zero exit code. Used by
	// reflect to enforce a clean post-execute hygiene scan before the
	// engine seals the pass — same shape as a pre-push hook. The
	// callback owns its own stderr formatting; runWikiSession only
	// uses the error to decide whether to short-circuit.
	PreFinalizeGate func(workRoot string, worktreeWiki *wiki.Config) error
	// CommitStager runs after a successful FinalizeIngest. It
	// receives the worktree root and the wiki dir's path relative to
	// it (or "" if there is no wiki). It owns staging the
	// caller-specific paths and committing with an appropriate
	// message. Returning run.ErrNothingToCommit is treated as a soft
	// empty turn — reported but not fatal.
	CommitStager func(workRoot, wikiRel string) error
	// ExtraEnv is the merged dev-env exports (parsed from the
	// project's `hooks/dev-env.d/*` setup scripts) that should ride
	// the claude subprocess as additional KEY=VALUE entries. Empty
	// for stages without a working tree (design, lint, etc.) or for
	// projects that ship no dev-env hooks. Routed unchanged to
	// executor.Request.ExtraEnv / executor.OneShotRequest.ExtraEnv.
	ExtraEnv []string
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
	sess, closeSess, err := openWikiSession(root, in, stdout, stderr)
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
		_ = closeSess()
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
			_ = closeSess()
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
			// Open-schema is a no-op. Failures here are real I/O or
			// config errors — bail before the executor runs so the
			// operator sees the root cause instead of a downstream
			// invariant breach at finalize.
			if _, err := wiki.EnsureManagedDocs(*wikiCfg); err != nil {
				moePrintf(stderr, "wiki: %v\n", err)
				_ = closeSess()
				return 1
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
		_ = closeSess()
		return 1
	}

	var runErr error
	if spec.Headless {
		runErr = executor.ExecuteOneShot(executor.OneShotRequest{
			Root:       workRoot,
			Prompt:     prompt,
			UserPrompt: spec.InitialPrompt,
			ClonePath:  spec.ClonePath,
			Stdout:     stdout,
			Stderr:     stderr,
			ExtraEnv:   spec.ExtraEnv,
		})
	} else {
		runErr = executor.Execute(executor.Request{
			Root:          workRoot,
			Metadata:      spec.Metadata,
			DocID:         spec.DocID,
			SessionID:     spec.SessionUUID,
			NewSession:    spec.NewSession,
			Prompt:        prompt,
			ClonePath:     spec.ClonePath,
			SessionCwd:    spec.SessionCwd,
			InitialPrompt: spec.InitialPrompt,
			Stdin:         os.Stdin,
			Stdout:        os.Stdout,
			Stderr:        stderr,
			ExtraEnv:      spec.ExtraEnv,
		})
	}

	// Pre-finalize gate (reflect's hygiene scan). Runs after the
	// executor and before FinalizeIngest; a non-nil return short-
	// circuits both FinalizeIngest and CommitStager so the pass
	// produces no log entry, no commit, and no checkpoint bump —
	// the operator re-runs the command to try again.
	var gateErr error
	if spec.PreFinalizeGate != nil {
		gateErr = spec.PreFinalizeGate(workRoot, wikiCfg)
	}

	// Wiki finalize runs before the commit so its writes (log.md
	// and checkpoint.json) ride along in the same per-turn commit
	// as the agent's wiki edits. A no-change session is a no-op —
	// finalize returns without touching disk if the wiki dir is
	// clean. Errors do not block the commit (the agent's content
	// edits should land regardless), but they do surface in the
	// exit-code waterfall so the operator notices instead of the
	// drift quietly accumulating across reflect passes.
	wikiRel := ""
	var finalizeErr error
	var commitErr error
	if gateErr == nil {
		if wikiCfg != nil {
			_, ferr := wiki.FinalizeIngest(*wikiCfg, wiki.FinalizeContext{
				RunID:    spec.FinalizeRunID,
				RunTitle: spec.FinalizeRunTitle,
				Claim:    spec.FinalizeClaim,
			}, stderr)
			if ferr != nil {
				moePrintf(stderr, "wiki: finalize failed: %v\n", ferr)
				moePrintln(stderr, "  agent edits will commit; checkpoint and "+
					"log.md were NOT written. Re-run the session or fix the "+
					"underlying issue before the next reflect.")
				finalizeErr = ferr
			}
			if rel, err := filepath.Rel(workRoot, wikiCfg.ContentDir); err == nil && !strings.HasPrefix(rel, "..") {
				wikiRel = rel
			}
		}
		// Commit any document changes even if Claude exited
		// non-zero — the operator may have chosen to bail mid-edit
		// but kept the edits.
		if spec.CommitStager != nil {
			commitErr = spec.CommitStager(workRoot, wikiRel)
		}
	}

	// Close the session: land it on local main and tear the
	// worktree down. Local-only — origin push is moe sync's job —
	// so a short budget and no heartbeat are fine.
	//
	// closeWithAutoResolve wraps the close: on a *RebaseFailureError
	// it launches a one-shot agent in the session worktree to
	// resolve, then retries close once. Falls through to today's
	// "resolve by hand / moe session abandon" message if the agent
	// can't take.
	closeErr := closeWithAutoResolve(closeSess, stdout, stderr)

	return reportWikiSessionExit(in, runErr, commitErr, closeErr, finalizeErr, gateErr, stdout, stderr)
}

// openWikiSession opens the session worktree under the repo lock and
// returns a closeSess closure already bound to the matching `-close`
// lock options. Centralising both halves means each early-failure path
// in runWikiSession is one `_ = closeSess()` line, and adding a new
// path can't drift the lock purpose / Run key away from the open side.
//
// Auto-sync is woven into both lock windows: an auto-pull runs before
// session.Open so the operator's first edit lands on current state,
// and an auto-push runs after session.Close so the turn commit reaches
// the other machine without the operator having to remember `moe sync`.
// A rebase-conflict on auto-pull refuses-loud (the turn never starts);
// a network failure on either side warns and continues. Heartbeat is on
// because the network legs can sit for several seconds on a slow link
// and we don't want a contending invocation to declare the lock stale.
func openWikiSession(root string, in wikiSessionInputs, stdout, stderr io.Writer) (*session.Session, func() error, error) {
	// Open (or resume) the session worktree under the repo lock.
	// The local work is just `git worktree add` (or a lookup); the
	// auto-pull before it can sit on the network briefly.
	var sess *session.Session
	err := withRepoLock(root, repolock.Options{
		Purpose:   in.LockPurpose + "-open",
		Run:       in.Project + "/" + in.RunSlug,
		Heartbeat: true,
	}, func() error {
		if err := sync.AutoPull(root, stdout, stderr); err != nil {
			return err
		}
		s, err := session.Open(root, in.Project, in.RunSlug, in.DocID)
		if err != nil {
			return err
		}
		sess = s
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	closeSess := func() error {
		return withRepoLock(root, repolock.Options{
			Purpose:   in.LockPurpose + "-close",
			Run:       in.Project + "/" + in.RunSlug,
			Heartbeat: true,
		}, func() error {
			if err := session.Close(sess); err != nil {
				return err
			}
			return sync.AutoPush(root, stdout, stderr)
		})
	}
	return sess, closeSess, nil
}

// reportWikiSessionExit prints the closing per-turn messages and
// returns the exit code for runWikiSession. It is the one place that
// decides how the possible failures (claude run, gate, commit, close,
// finalize) compose into a single exit status. Run / finalize / gate
// errors each independently force a non-zero exit even when the
// per-turn commit landed cleanly — finalize failure means
// checkpoint.json / log.md weren't written, and a 0 exit there would
// let the operator move on without noticing. Gate failure means we
// deliberately skipped both finalize and commit; the gate's own
// stderr block carries the explanation.
func reportWikiSessionExit(in wikiSessionInputs, runErr, commitErr, closeErr, finalizeErr, gateErr error, stdout, stderr io.Writer) int {
	if runErr != nil {
		moePrintf(stderr, "claude exited: %v\n", runErr)
		// Fall through to report commit result and exit non-zero.
	}
	switch {
	case gateErr != nil:
		// Gate already explained itself on stderr; no commit happened.
	case errors.Is(commitErr, run.ErrNothingToCommit):
		moePrintln(stdout, "no document changes; nothing committed")
	case commitErr != nil:
		moePrintf(stderr, "commit turn: %v\n", commitErr)
		return 1
	default:
		moePrintf(stdout, "committed %s turn for %s %s\n", in.DocID, in.Project, in.RunSlug)
	}
	if closeErr != nil {
		moePrintf(stderr, "session close: %v\n", closeErr)
		return 1
	}
	if runErr != nil || finalizeErr != nil || gateErr != nil {
		return 1
	}
	return 0
}

// sessionDocCwd is the cwd document-only stages hand to claude — a
// stable per-document path under <root>/.moe/sessions/<project>/<run>/<doc>/.
// Stable across turns because the inputs are stable; that's the whole
// point: claude encodes cwd into its on-disk project dir, so a churning
// cwd (e.g. the per-turn worktree path) leaves `--resume <sid>` looking
// in a fresh dir on every turn and reporting the session missing. The
// dir itself stays empty — `--add-dir` is what actually scopes the
// session to the worktree.
func sessionDocCwd(root, projectID, runID, docID string) string {
	return filepath.Join(root, ".moe", "sessions", projectID, runID, docID)
}
