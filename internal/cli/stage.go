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
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/agent"
	_ "github.com/modulecollective/moe/internal/agent/claude"
	_ "github.com/modulecollective/moe/internal/agent/codex"
	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/stylesheet"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/transcript"
	"github.com/modulecollective/moe/internal/wiki"
)

// oneShotPromptDelimiter separates the assembled stage system prompt
// from the appended one-shot addendum, matching the section delimiter
// buildSystemPrompt uses internally.
const oneShotPromptDelimiter = "\n---\n\n"

// headlessTurnTimeout hard-caps a headless stage turn's wall-clock.
// Headless turns have no operator on stdin to Ctrl-C a wedge, so without
// a cap an agent that backgrounds a long-lived subprocess (the dominant
// failure mode) hangs the turn indefinitely. 60min clears any legitimate
// well-scoped stage turn with margin while still bounding the wedge.
const headlessTurnTimeout = 60 * time.Minute

// stageSessionOpts carries the per-stage knobs runStageSession needs
// beyond the run identifiers. Most stages just set NeedsSandbox and
// InitialPrompt. Wiki-aware ingest stages (kb summarize, future twin
// stages) supply WikiBuilder so the engine's prompt section, per-turn
// staging, and FinalizeIngest hook all wire up automatically.
type stageSessionOpts struct {
	// NeedsSandbox switches the per-run sandbox clone on. Code stages
	// require it; document-only stages leave it false. Design stage
	// also opts in (read-only) so the agent can verify facts about the
	// code while drafting — see EnforceSandboxBoundary for the guard.
	NeedsSandbox bool
	// EnforceSandboxBoundary, when true, snapshots the sandbox HEAD at
	// stage open and refuses (with a non-zero exit) once the executor
	// returns if the sandbox HEAD has moved or any tracked file shows
	// a modification, addition, or deletion. The bureaucracy-side
	// canvas commit still lands — only the cascade to the next stage
	// is suppressed. Used by design to keep code changes from leaking
	// in as a spike-as-handoff artifact. Requires NeedsSandbox: true;
	// no-op otherwise. BoundaryAllowsCommits relaxes the HEAD leg for
	// stages that may legitimately commit (review's in-place fixes).
	EnforceSandboxBoundary bool
	// BoundaryAllowsCommits, paired with EnforceSandboxBoundary, drops
	// the entry-HEAD snapshot so the boundary check tolerates commits
	// the stage lands on the run branch while still refusing an
	// uncommitted (dirty tracked) tree. Review sets it: a trivial
	// finding may be fixed in place and committed, but a half-fix left
	// uncommitted still refuses the cascade — attributed to review, the
	// stage that made the mess. Only the HEAD-advanced leg relaxes; the
	// dirty-tracked leg keeps running (checkSandboxBoundary reads an
	// empty entryHEAD as "skip the HEAD comparison"). No-op without
	// EnforceSandboxBoundary.
	BoundaryAllowsCommits bool
	// InitialPrompt is auto-sent as the session's first user message
	// — typically a "greet the operator and ask what they want"
	// kickoff. Empty drops the auto-send and lands the operator in a
	// blank prompt. In Headless mode it's the entire user turn for
	// `claude -p` — typically the run title.
	InitialPrompt string
	// InitialPromptBuilder, when non-nil, supersedes InitialPrompt:
	// runStageSession invokes it after the session worktree is open and
	// the wiki cfg has been rewritten to worktree paths, handing it the
	// worktree root, the rewritten cfg, and the seed signal (stubbed is
	// true when EnsureManagedDocs created at least one managed-doc stub
	// this turn — a fresh-wiki pass the builder can frame as a seed).
	// Callers that bake absolute bureaucracy paths into the kickoff must
	// use this instead of InitialPrompt so those paths resolve inside the
	// worktree — twin reflect assembling its kickoff against the canonical
	// root *before* the worktree existed is what walked a reflect pass
	// into the operator's live checkout. Mirrors PreFinalizeGate's
	// (workRoot, worktreeWiki) shape and runs at the same lifecycle point.
	InitialPromptBuilder func(workRoot string, worktreeWiki *wiki.Config, stubbed bool) (string, error)
	// OnAgentStart, when non-nil, fires immediately before the executor
	// is dispatched. See wikiTurnSpec.OnAgentStart.
	OnAgentStart func()
	// Headless drives the stage as a one-turn `claude -p` call instead
	// of an interactive REPL. Output streams to the operator's
	// terminal (no stdin), the workflow's oneshot.md fragment is
	// appended to the system prompt, and transcript mirroring is
	// skipped (the canvas + per-turn commit are the durable
	// artifacts). Set by the chain prompt's cascade driver
	// (`!` / `!<stage>` / `!!` / `!!!`).
	//
	// Headless implies SkipNextStage: a headless turn has no stdin to
	// answer the post-turn chain prompt, so the post-turn guard
	// (runStageSession's tail) treats Headless as a skip on its own. A
	// caller may still set SkipNextStage explicitly — the two are kept
	// as independent fields because the non-cascade serve path skips the
	// prompt while running interactive (headless=false, skip=true) — but
	// it never needs to pair them by hand to keep a headless turn from
	// prompting.
	Headless bool
	// SkipNextStage suppresses the post-turn "next: …" prompt /
	// chained-stage call. Used by the cascade driver, which composes
	// its own chain (design → code → review → test → push) and never wants the
	// interactive next-stage prompt to fire mid-chain. Headless turns
	// skip the prompt regardless of this field (see Headless above); the
	// field stays meaningful for the interactive-but-suppressed serve
	// path.
	SkipNextStage bool
	// NextStageOverride, when non-empty, replaces the stage the
	// post-turn prompt offers — without touching the back-targets,
	// which still key off the document that just finished. The
	// push-gate recovery session sets it to "push": the recovery is a
	// code turn, but the operator should be offered the push retry, not
	// code's ordinary successor (test). Empty leaves the successor
	// lookup unchanged — the case for every stage but recovery. Ignored
	// when SkipNextStage is set (no prompt fires at all).
	NextStageOverride string
	// Model, if non-empty, is the `--model` value for this turn's agent
	// invocation — both the interactive and headless paths. Empty defers
	// to the vendor CLI's configured default. runStageSession populates it
	// via stageModel when a caller leaves it empty — the stylesheet value,
	// unless a paired `agent:` scopes it to a backend the turn isn't
	// running (then it's dropped with a notice); a bounded curation caller
	// (push synthesis) that sets it explicitly keeps its value
	// (explicit-beats-stylesheet).
	Model string
	// CanvasSkeleton, when non-empty, is written to the canvas file the
	// first time the document is opened (the EnsureDocument-mutated
	// branch). Lets stages with a fixed structural canvas — test stage's
	// "What was verified / What wasn't verified / Fixes applied"
	// headings — seed the agent's first read with the shape it has to
	// fill, instead of relying on the prompt fragment alone. Skipped on
	// resume turns.
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
	// per-turn commit. Used by chores and hooks to stage the project's
	// chores/ or hooks/ directory alongside the per-pass canvas, so the
	// edits the agent made there ride in one commit.
	ExtraStagePaths func(workRoot string, md *run.Metadata) ([]string, error)
	// SkipFinalize, when true, skips wiki.FinalizeIngest at session
	// close. The per-stage twin stages (vision, architecture, …,
	// glossary) commit their managed-doc edits but don't bump the
	// checkpoint or write a log.md entry — the finalize stage owns
	// both at the end of the pass. Without this flag, every per-stage
	// commit would advance `LastIngestAt`, and stage two's kickoff
	// would compute a shorter events list than stage one's — the
	// drift the design forbids.
	SkipFinalize bool
	// PreFinalizeGate, when non-nil, runs after the executor returns
	// and before FinalizeIngest. A non-nil return short-circuits both
	// FinalizeIngest and the per-turn commit. Used by the finalize
	// stage's hygiene re-scan: leftover findings refuse to seal the
	// pass. Routed straight through to wikiTurnSpec.PreFinalizeGate;
	// see that field for the contract.
	PreFinalizeGate func(workRoot string, worktreeWiki *wiki.Config) error
	// Agent names the backend (claude / codex) that should drive this
	// turn. Empty falls through resolveAgentName's precedence ladder:
	// the model stylesheet, then $MOE_AGENT, else "claude". Stage
	// callers populate this from the run.json field when present, or
	// from a --agent flag override.
	Agent string
	// CanvasOnOpen, when non-nil, runs on every session open (fresh and
	// resume) after the rest of BuildSpec has succeeded. It receives the
	// session worktree root, the run metadata, and the resolved agent
	// name for this turn, and may read or write the canvas. chat is the
	// only caller: its canvas is a moe-owned session log the agent never
	// writes, so chat appends a per-session marker (naming the backend
	// that ran) here to make the canvas differ from main every turn —
	// which is what satisfies session.Close's canvas-unchanged guard
	// without an opt-out flag (the canvas genuinely moved). The agent name
	// is threaded in rather than re-resolved by the caller so the marker
	// matches the backend runStageSession actually dispatched — including
	// any model-stylesheet steering. Distinct from CanvasSkeleton, which
	// seeds once on first open only; CanvasOnOpen fires every open, which
	// is what the per-resume marker needs.
	CanvasOnOpen func(workRoot string, md *run.Metadata, agentName string) error
}

// stageAgentName resolves the agent backend for a stage turn. It is
// the contract layer between the per-stage call sites in
// runStageSession and the precedence ladder in resolveAgentName.
// sheetAgent is the model stylesheet's `agent:` value for this
// (workflow, stage), or "" when no rule sets one.
func stageAgentName(opts stageSessionOpts, md *run.Metadata, sheetAgent string) string {
	runDefault := ""
	if md != nil {
		runDefault = md.Agent
	}
	return resolveAgentName(opts.Agent, runDefault, sheetAgent)
}

// stylesheetVocab builds the vocabulary the stylesheet is validated
// against from the live registries this package owns: every registered
// workflow mapped to its stage names, plus the registered agent
// backends. Reading straight from the registries means new workflows,
// stages, or agents extend the accepted vocabulary automatically.
func stylesheetVocab() stylesheet.Vocab {
	wf := make(map[string][]string, len(workflows))
	for name, w := range workflows {
		wf[name] = w.Stages()
	}
	return stylesheet.Vocab{Workflows: wf, Agents: agent.Names()}
}

// stageModel decides the `--model` value for a stage turn. An explicit
// value from a bounded curation caller (opts.Model) always wins — same
// shape as the agent ladder's explicit-beats-stylesheet rule. Otherwise
// the stylesheet's model rides only when the turn's resolved backend
// matches the stylesheet's own resolved `agent:` for this (workflow,
// stage) — the winning `agent` property after the cascade, not
// literally same-rule pairing. A paired model is scoped to its backend:
// when a rung above the stylesheet ($MOE_FORCE_AGENT, --agent,
// run.json.Agent) forces the turn onto a different backend, the model
// was never meant for it, so it is dropped — with a one-line stderr
// notice, never silently — and the backend's own default applies. An
// unpaired model (no resolved `agent:`) keeps the verbatim behaviour:
// handed to whatever backend runs, where a foreign name fails as the
// backend CLI's own error (moe keeps no model catalog by design).
func stageModel(explicit, sheetAgent, sheetModel, agentName string, stderr io.Writer) string {
	if explicit != "" {
		return explicit
	}
	if sheetModel == "" {
		return ""
	}
	if sheetAgent != "" && sheetAgent != agentName {
		moePrintf(stderr, "model-stylesheet: dropping model %q (rule pairs agent %s; turn runs %s)\n",
			sheetModel, sheetAgent, agentName)
		return ""
	}
	return sheetModel
}

// resolveAgentName picks the backend for this turn. Precedence:
// $MOE_FORCE_AGENT (global override) → explicit per-call override
// (--agent flag on this verb) → run-level persisted default
// (run.json.Agent) → model stylesheet → $MOE_AGENT → "claude". Keep
// this helper as the single source for the operator-facing ladder;
// stage call sites should go through stageAgentName.
//
// The stylesheet rung sits below the operator's explicit bindings
// (--agent, run.json.Agent) and above the $MOE_AGENT scalar default:
// this is the moe analog of fabro's "direct node attributes beat the
// stylesheet". Everything above the stylesheet is operator-explicit for
// this run; everything below is a background default the per-(workflow,
// stage) sheet should override.
//
// $MOE_FORCE_AGENT is the high-precedence inverse of the low-precedence
// $MOE_AGENT default: it wins over everything, including an explicit
// --agent flag, so an operator can flip every stage of every run in the
// process onto one backend during an outage. It is read live (never
// persisted to run.json); unsetting it reverts each run to its own
// configured agent. A bad value flows through and fails legibly at
// dispatch via agent.Get, same as any other unknown backend name.
func resolveAgentName(explicit, runDefault, stylesheet string) string {
	if v := os.Getenv("MOE_FORCE_AGENT"); v != "" {
		return v
	}
	if explicit != "" {
		return explicit
	}
	if runDefault != "" {
		return runDefault
	}
	if stylesheet != "" {
		return stylesheet
	}
	if v := os.Getenv("MOE_AGENT"); v != "" {
		return v
	}
	return "claude"
}

// runStageSession is the core loop shared by `moe sdlc design` and `moe sdlc code`:
// resolve the run/document, hand the operator an interactive agent
// session keyed to that document's session-id, and commit whatever changed
// when the agent exits.
//
// The session runs inside a throwaway git worktree on a branch named
// session/<project>/<run>/<doc>. All per-turn commits (session-start,
// work turn) land on that branch; when Claude exits, the branch is
// rebased onto main, fast-forwarded in, pushed (best-effort) and
// cleaned up. The repo-wide lock is held only during open (short) and
// close (seconds), not across the Claude session itself.
//
// needsSandbox controls the per-run sandbox clone: document-only
// stages leave it false, code stages require a writable one (with a
// clear error if the project isn't registered as a submodule), and
// design opts in read-only (see stageSessionOpts.NeedsSandbox). The
// sandbox lives under the canonical bureaucracy root (not the session
// worktree) so it persists across turns.
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

	// Materialize the project's submodule before anything else. Every
	// stage either reads source directly (twin/kb wiki ingest), drives
	// a sandbox clone (code/review/test), or kicks off an agent whose first
	// action is usually a project-side read. Cold projects hit one
	// `git submodule update --init --recursive`; warm projects pay one
	// os.ReadDir. Failures surface as *project.SubmoduleInitError with
	// the verbatim retry command — same shape sandbox used to emit.
	if err := project.EnsureMaterialized(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Run-scoped state captured by closure. md is pre-loaded from the
	// canonical root only to feed the pre-session, pre-pull surface:
	// the entry banner (md.Workflow), stylesheet resolution, and the
	// agent ladder, all of which run before openWikiSession takes the
	// repolock and pulls. This entry copy is deliberately not trusted
	// past that point — BuildSpec reloads run.json from the session
	// worktree (post-pull) into this same pointer, so every downstream
	// closure carries the fresh state. Loading from `root` here rather
	// than the worktree is safe for the banner's purposes — run.json
	// doesn't drift on `git worktree add`, and md.Workflow is immutable
	// per run.
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Model stylesheet: resolve the (workflow, stage) → (agent, model)
	// bindings from the checked-in <root>/model-stylesheet.css. Read from
	// the canonical root here — the same freshness as md, loaded above —
	// before the session worktree opens; the resolution inputs
	// (md.Workflow, docID) are both already in scope. A missing file is a
	// no-op empty sheet (today's behaviour); a malformed file refuses the
	// turn loudly rather than silently ignoring the operator's rules.
	// Living here means every caller of runStageSession — interactive
	// verbs, cascade headless, serve children — gets stylesheet steering
	// by construction.
	sheet, err := stylesheet.Load(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Validate the sheet's own vocabulary against the live registries
	// before resolving. A misspelled selector, property, or agent value
	// refuses the turn here — same fail-loud path as a parse error —
	// rather than silently matching nothing on some future turn.
	if err := sheet.Validate(stylesheetVocab()); err != nil {
		moePrintf(stderr, "stylesheet %s: %v\n", filepath.Join(root, stylesheet.FileName), err)
		return 1
	}
	sheetAgent, sheetModel := sheet.Resolve(md.Workflow, docID)
	agentName := stageAgentName(opts, md, sheetAgent)
	opts.Model = stageModel(opts.Model, sheetAgent, sheetModel, agentName, stderr)
	banner.StageEntry(stdout, agentName, md.Workflow, docID, md.Project, md.ID)

	// Sandbox-boundary snapshot, populated by BuildSpec when
	// opts.EnforceSandboxBoundary is set. checkSandboxBoundary
	// reads these after the executor returns to refuse the cascade
	// if the agent left a half-implementation behind. Empty when
	// the stage opts out (most stages).
	var sandboxBoundaryClone, sandboxBoundaryEntryHEAD string

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runID,
		DocID:       docID,
		Agent:       agentName,
		LockPurpose: "stage",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			if opts.WikiBuilder == nil {
				return nil, nil
			}
			return opts.WikiBuilder(canonicalRoot, md)
		},
		// md is pre-loaded at runStageSession entry from the canonical
		// root, *before* openWikiSession pulled origin/main under the
		// repolock. Reload it here from the session worktree — which
		// session.Open created from post-pull main — so this turn's
		// write-backs ride the pulled run state, not the stale entry
		// copy. Overwrite in place (*md = *fresh): every downstream
		// closure captured this pointer, so the reload routes all of
		// them through fresh state with no signature changes. workRoot,
		// not root: on a resumed crashed-turn branch the worktree's
		// run.json can be ahead of main, and it's what this turn commits
		// against. If the pull deleted the run on the other machine the
		// load fails and the turn refuses, rather than resurrecting it.
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			fresh, err := run.Load(workRoot, md.Project, md.ID)
			if err != nil {
				return wikiTurnSpec{}, err
			}
			*md = *fresh
			doc, mutated, err := run.EnsureDocument(workRoot, md, docID)
			if err != nil {
				return wikiTurnSpec{}, err
			}
			// Resolve sessionCwd early so the skill materialisers can
			// write under it: claude's cwd-walkup skill discovery starts
			// at sessionCwd post-fix, so a workRoot-only materialisation
			// wouldn't be found. See sessionDocCwd's doc for the
			// stable-cwd rationale.
			sessionCwd := sessionDocCwd(root, md.Project, md.ID, docID)
			if err := os.MkdirAll(sessionCwd, 0o755); err != nil {
				return wikiTurnSpec{}, fmt.Errorf("session: mkdir %s: %w", sessionCwd, err)
			}
			// Materialise the moe-bureaucracy skill into the sessionCwd
			// .claude/skills/ (claude runs cwd=sessionCwd and finds it
			// there) and workRoot/.codex/skills/ (codex anchors there).
			// See skill_materialize.go. Refresh on every BuildSpec is
			// cheap; the paths are session-stable but rewriting is
			// faster than reasoning about staleness across resumes.
			if err := materializeMoeBureaucracySkill(workRoot, sessionCwd, md); err != nil {
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
			// Document-only stages see no clone; code stages insist on one
			// and pre-position it on the moe/<run-id> branch so the
			// agent's commits (and any later `moe sdlc push`) land on a
			// branch we own; design opts in read-only. attachRunWorkspace
			// routes per-run sandbox vs named workspace based on
			// md.Workspace; the callers here don't need to know which.
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
				// Dev-env hooks fire on every code/review/test stage open
				// against this working tree. First touch runs the
				// project's dev-env.d/* setup scripts and caches the
				// parsed KEY=VALUE output to <tree>/.moe/dev-env.env;
				// later turns re-source the cache while its allowlisted
				// directories remain valid, or tear it down and rebuild it
				// when one has vanished. Projects with no dev-env.d/
				// directory get an empty env (the single-driver default) —
				// no warning, no refusal.
				env, _, err := devEnvSetupEnv(root, clonePath, md, stdout, stderr)
				if err != nil {
					return wikiTurnSpec{}, fmt.Errorf("dev-env: %w", err)
				}
				devEnv = env
				if opts.EnforceSandboxBoundary {
					sandboxBoundaryClone = clonePath
					// Snapshot post-dev-env so the boundary check
					// tolerates dev-env hooks that may legitimately touch
					// the worktree (e.g. cache writes outside tracked
					// files). Hooks are contracted to leave tracked files
					// alone — see workflows/hooks/code.md.
					//
					// BoundaryAllowsCommits skips the snapshot: a stage
					// that may commit (review) leaves entryHEAD empty, and
					// checkSandboxBoundary reads that as "don't compare
					// HEAD" — only the dirty-tracked leg runs.
					if !opts.BoundaryAllowsCommits {
						head, err := git.HEAD(clonePath)
						if err != nil {
							return wikiTurnSpec{}, fmt.Errorf("sandbox boundary: snapshot HEAD: %w", err)
						}
						sandboxBoundaryEntryHEAD = head
					}
				}
			}

			// Chat grooms the operator's real backlog in-session — point
			// the agent's MOE_HOME at the canonical bureaucracy so
			// `moe idea new` / `edit` commit to live main (visible across
			// windows at once) and the real bureaucracy lands in the
			// agent's writable --add-dir set. One assignment, both
			// effects; see chatGroomingHome. No-op for non-chat stages.
			devEnv = chatGroomingHome(md.Workflow, devEnv, root)

			// Materialise the moe-context skill once clonePath is final
			// — sibling to the bureaucracy materialiser above, but this
			// one needs the clone path threaded so the rendered body can
			// name both roots concretely (or render the document-only
			// branch when there's no clone). Same lifecycle: worktree-
			// only, refreshed every BuildSpec, never staged.
			if err := materializeMoeContextSkill(workRoot, sessionCwd, md, clonePath); err != nil {
				return wikiTurnSpec{}, err
			}

			// moe-howto is the chat workflow's idea-capture / backlog-
			// grooming skill — chat-only by intent (sdlc / twin / kb
			// agents aren't here to groom the backlog, so per "tool
			// scoping by document" they don't get it). One workflow needs
			// it today, so a single gate beats a registry; revisit if a
			// second workflow-specific skill shows up.
			if md.Workflow == chatWorkflow {
				if err := materializeMoeHowtoSkill(workRoot, sessionCwd); err != nil {
					return wikiTurnSpec{}, err
				}
			}

			// mutated means EnsureDocument just minted the session
			// UUID this turn — fresh session, nothing to validate.
			// Otherwise stat the exact path claude will read for
			// `--resume <sid>` from the cwd it'll run in (sessionCwd,
			// the same value the executor's cmd.Dir uses) and decide
			// between two outcomes:
			//   - JSONL at the canonical path → resume normally.
			//   - JSONL absent (cross-machine fresh checkout, wiped
			//     cache, dirty exit before claude wrote turn 1, or
			//     a prior headless turn which doesn't honor moe's
			//     --session-id) → re-mint the session id, persist +
			//     commit run.json, and pass --session-id instead of
			//     --resume. Chat history is gone but the canvas on
			//     disk is intact; we warn on stderr.
			// Pre-flighting beats letting claude error mid-run: the
			// operator gets a clear stderr line, not a stuck run.
			newSession := mutated
			if !newSession {
				if sessionCwd != "" {
					a, agentErr := agent.Get(agentName)
					if agentErr != nil {
						return wikiTurnSpec{}, agentErr
					}
					switch found, err := a.TranscriptExists(doc.Session, sessionCwd); {
					case err != nil:
						return wikiTurnSpec{}, fmt.Errorf("session: stat transcript: %w", err)
					case found:
						// Transcript present — normal --resume path.
					default:
						// TranscriptExists miss. Before re-minting, ask the
						// agent to look anywhere else the transcript might
						// still live (claude: a stale encoded-cwd bucket
						// from a pre-stable-cwd run, or the bureaucracy
						// mirror). Codex returns RestoreMissing as a no-op
						// — its own glob already settled the question.
						mirrorPath := filepath.Join(workRoot, run.ThreadPathFor(agentName, md.Project, md.ID, docID))
						outcome, err := a.RestoreTranscript(doc.Session, sessionCwd, mirrorPath)
						if err != nil {
							return wikiTurnSpec{}, fmt.Errorf("session: restore transcript: %w", err)
						}
						switch outcome.Result {
						case agent.RestoreFromCache:
							moePrintf(stderr, "session %s recovered from cache (%s)\n", doc.Session, outcome.Source)
						case agent.RestoreFromMirror:
							src := outcome.Source
							if rel, relErr := filepath.Rel(workRoot, src); relErr == nil && !strings.HasPrefix(rel, "..") {
								src = rel
							}
							moePrintf(stderr, "session %s restored from %s\n", doc.Session, src)
						case agent.RestoreNotNeeded:
							// Race between probe and restore — the
							// canonical path showed up after the miss. No
							// stderr line; resume normally.
						case agent.RestoreMissing:
							sid, err := run.NewSessionID()
							if err != nil {
								return wikiTurnSpec{}, err
							}
							moePrintf(stderr, "session %s not found anywhere; starting fresh as %s (prior chat history not recoverable)\n", doc.Session, sid)
							doc.Session = sid
							if err := run.Save(workRoot, md); err != nil {
								return wikiTurnSpec{}, err
							}
							if err := commitSessionStart(workRoot, md, docID); err != nil {
								return wikiTurnSpec{}, err
							}
							newSession = true
						}
					}
				}
			}

			// CanvasOnOpen runs last in BuildSpec — after every step that
			// can fail — so a bootstrap error never leaves an uncommitted
			// canvas write behind. chat uses it to append its per-session
			// marker; see the field doc on stageSessionOpts.
			if opts.CanvasOnOpen != nil {
				if err := opts.CanvasOnOpen(workRoot, md, agentName); err != nil {
					return wikiTurnSpec{}, err
				}
			}

			// Headless mode has no operator on stdin to type the seed
			// prompt, so default it to the run slug — the same shape
			// the cascade driver depends on.
			// Callers that pass an explicit InitialPrompt keep theirs.
			initialPrompt := opts.InitialPrompt
			if opts.Headless && initialPrompt == "" {
				initialPrompt = md.ID
			}

			return wikiTurnSpec{
				Metadata:             md,
				DocID:                docID,
				ClonePath:            clonePath,
				SessionCwd:           sessionCwd,
				SessionUUID:          doc.Session,
				NewSession:           newSession,
				InitialPrompt:        initialPrompt,
				InitialPromptBuilder: opts.InitialPromptBuilder,
				OnAgentStart:         opts.OnAgentStart,
				Headless:             opts.Headless,
				Model:                opts.Model,
				Agent:                agentName,
				FinalizeRunID:        md.ID,
				FinalizeRunTitle:     "",
				SkipFinalize:         opts.SkipFinalize,
				ExtraEnv:             mapToEnv(devEnv),
				AddDirs:              devEnvWritableDirs(devEnv),
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					// Read-only wording for the strict-boundary stages,
					// but not for review: it enforces the boundary *and*
					// commits its own fixes (BoundaryAllowsCommits), so
					// the writable paragraph is the true one there.
					readOnly := opts.EnforceSandboxBoundary && !opts.BoundaryAllowsCommits
					p, err := buildSystemPrompt(workRoot, md, docID, clonePath, readOnly, worktreeWiki)
					if err != nil {
						return "", err
					}
					if opts.Headless {
						if frag := moe.OneShot(md.Workflow); frag != "" {
							p += oneShotPromptDelimiter + frag
						}
					}
					// Persist the assembled prompt alongside the canvas
					// and thread JSONL so the operator can see what the
					// agent actually received. Overwrite each turn;
					// commitTurn stages docDir wholesale and picks the
					// file up automatically. Best-effort write — a
					// failure here surfaces to stderr and lets the turn
					// proceed (the prompt itself is the load-bearing
					// payload; the on-disk copy is a debug surface).
					if err := writePromptSnapshot(workRoot, agentName, md, docID, p); err != nil {
						moePrintf(stderr, "prompt snapshot: %v\n", err)
					}
					return p, nil
				},
				CommitStager: func(workRoot, wikiRel string) error {
					// cwd-inversion shape: the agent writes the canvas,
					// followups, and twin feedback at their natural
					// absolute bureaucracy paths under the session
					// worktree. No clone-to-bureaucracy shuttle to run
					// here — commitTurn reads the same paths the agent
					// just wrote.
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
					return commitTurn(workRoot, md, docID, extras...)
				},
				PreFinalizeGate: opts.PreFinalizeGate,
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
	// Boundary check runs AFTER the bureaucracy commit (canvas + run
	// state ride along regardless) but BEFORE the cascade prompt, so a
	// barfing design stage doesn't drag downstream stages forward
	// against a dirty sandbox. The check is best-effort wrt recovery:
	// the operator resets the sandbox clone and re-runs design.
	if opts.EnforceSandboxBoundary && sandboxBoundaryClone != "" {
		if err := checkSandboxBoundary(sandboxBoundaryClone, sandboxBoundaryEntryHEAD, docID); err != nil {
			moePrintf(stderr, "%s: %v\n", docID, err)
			return 1
		}
	}
	banner.StageExit(stdout, md.Workflow, docID, md.Project, md.ID)
	if skipPostTurnPrompt(opts) {
		// Headless ⇒ skip is structural, not a caller convention: a
		// headless turn has no stdin to answer the post-turn prompt, so
		// it must never fire one. Every cascade dispatch is headless and
		// no longer threads a separate suppress flag, so the
		// opts.Headless term is what makes the cascade skip. The
		// SkipNextStage term stays for the interactive callers that skip
		// without being headless — chat, push. Serve-spawned sessions
		// skip through the env handshake read inside skipPostTurnPrompt,
		// so every workflow's stage verb is serve-safe without each
		// callsite threading the flag. See the field doc comments above.
		return 0
	}
	return promptNextStageOverride(root, md, docID, opts.NextStageOverride, false, stdout, stderr)
}

// skipPostTurnPrompt decides whether runStageSession's tail fires the
// post-turn chain prompt. Three suppressors: the caller asked
// (SkipNextStage — chat, push), the turn was headless (no stdin to
// answer), or the process was spawned by `moe serve` (the
// MOE_SERVE_AGENT handshake). Reading the handshake here — instead of
// each stage opener passing SkipNextStage: serveAgentSuppress() — makes
// every present and future workflow serve-safe by construction; the
// per-callsite pattern is exactly the kind of thing a new workflow's
// openers can miss.
func skipPostTurnPrompt(opts stageSessionOpts) bool {
	return opts.SkipNextStage || opts.Headless || serveAgentSuppress()
}

// serveAgentSuppress reports whether the current process was spawned
// by `moe serve` to host a single agent session. The serve↔CLI
// handshake is invisible to shell-side operators: setting
// MOE_SERVE_AGENT=1 in the spawn env tells runStageSession to skip the
// post-turn `next: …` chain prompt (which has no input source under
// serve — the child's stdin is a PTY nobody types into) so moe exits
// cleanly after the agent returns.
//
// Read once per stage exit; same shape MOE_SERVE_NOTIFY_URL takes
// (env-var handshake, not a documented operator flag).
func serveAgentSuppress() bool {
	return os.Getenv("MOE_SERVE_AGENT") == "1"
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
	// Agent is the resolved backend name (claude / codex) the executor
	// will dispatch to. Populated by runStageSession before
	// runWikiSession runs so reportWikiSessionExit can attribute the
	// "<agent> exited" line honestly. Empty falls back to "agent" in
	// the reporter, which keeps the lint caller correct without
	// forcing it to resolve up front.
	Agent string
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
	// SessionCwd is the stable per-document cwd for claude turns — a
	// path under <root>/.moe/sessions/<p>/<r>/<d>. Code-bearing stages
	// reach the sandbox clone via --add-dir, not via cwd. Empty for
	// run-less / lint sessions, which don't `--resume` and can keep
	// using the worktree root.
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
	// InitialPromptBuilder, when non-nil, is invoked after the wiki cfg
	// is rewritten to worktree paths and supersedes InitialPrompt with
	// its result. Lets a caller defer kickoff assembly until the
	// worktree root and the rewritten cfg are known, so any absolute
	// bureaucracy paths it renders resolve inside the worktree. The
	// stubbed argument carries the EnsureManagedDocs seed signal. See
	// stageSessionOpts.InitialPromptBuilder for the why.
	InitialPromptBuilder func(workRoot string, worktreeWiki *wiki.Config, stubbed bool) (string, error)
	// OnAgentStart, when non-nil, is invoked immediately before the
	// executor is dispatched — after every bootstrap step that can
	// fail. It is the "the agent turn actually began" signal; the
	// pulse uses it to tell a Ctrl-C that landed before the survey
	// started (dispose the run) from one that landed during or after
	// it (leave the run open for review), without inferring either
	// from an exit code.
	OnAgentStart func()
	// Headless flips runWikiSession from the interactive REPL path
	// (executor.Execute) to the one-shot streaming path
	// (executor.ExecuteOneShot): no stdin, no transcript mirror, exits
	// after one turn. The rest of the lifecycle — open session
	// worktree, prompt assembly, commitTurn, close — is unchanged.
	Headless bool
	// Model, if non-empty, is passed to the executor as the `--model`
	// value on both the interactive (Execute) and headless
	// (ExecuteOneShot) paths. Routes stageSessionOpts.Model through to the
	// executor; see that field for usage notes.
	Model string
	// FinalizeRunID + FinalizeRunTitle drive the log.md entry header.
	FinalizeRunID    string
	FinalizeRunTitle string
	// SkipFinalize, when true, skips wiki.FinalizeIngest at session
	// close — the per-stage twin stages commit their managed-doc
	// edits but leave checkpoint advancement and log.md to the
	// finalize stage. The gate / commit / close sequence is
	// otherwise unchanged.
	SkipFinalize bool
	// Agent names the backend the executor should dispatch to. Always
	// non-empty in production paths (runStageSession resolves it via
	// stageAgentName before populating this struct); test callers
	// that build wikiTurnSpec directly leave it empty and runWikiSession
	// falls back to resolveAgentName("", "", "") at dispatch time.
	Agent string
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
	// AddDirs are the dev-env directories the agent should be allowed
	// to write to alongside the sandbox clone and bureaucracy root —
	// MOE_HOME and MOE_DEV_TMPDIR for the moe project's own hooks.
	// Empty for stages without a working tree and for projects that
	// emit no recognised directory env vars. Routed unchanged to
	// agent.Request.AddDirs / agent.OneShotRequest.AddDirs.
	AddDirs []string
}

// closeBootstrapFailedSession runs closeSess on an early-exit path
// (BuildSpec / wiki bootstrap / BuildPrompt failed before the executor
// ran) and surfaces any non-nil close error to stderr. The bootstrap
// failure has already been printed; this layer makes sure a subsequent
// canvas-unchanged refusal — the new "no-op session" gate's loud-fail
// behaviour — doesn't get swallowed alongside the session worktree it
// leaves intact.
//
// okToPush is hard-wired to false: no turn ran, so origin must not
// receive the bureaucracy-side per-turn commit. Same shape as the
// post-executor path's failure case.
func closeBootstrapFailedSession(closeSess func(okToPush bool) error, stderr io.Writer) {
	if err := closeSess(false); err != nil {
		moePrintf(stderr, "session close: %v\n", err)
	}
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
		closeBootstrapFailedSession(closeSess, stderr)
		return 1
	}

	// Wiki integration — built after BuildSpec so callers that need
	// run metadata (e.g. the kb wiki builder reads md.Project) can
	// resolve it first. The canonical config's ContentDir gets
	// rewritten to live inside the session worktree so prompt paths
	// and engine git-status calls reference the active worktree.
	var wikiCfg *wiki.Config
	// docsStubbed is the EnsureManagedDocs seed signal, handed to the
	// kickoff builder below. False for non-wiki stages and for a reflect
	// against an already-seeded twin; true on the first reflect, where
	// every managed doc is freshly stubbed and the builder frames the
	// pass as a seed-and-author rather than a walk-against-events.
	var docsStubbed bool
	if in.WikiBuilder != nil {
		canonical, err := in.WikiBuilder(root)
		if err != nil {
			moePrintf(stderr, "wiki: %v\n", err)
			closeBootstrapFailedSession(closeSess, stderr)
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
			stubbed, err := wiki.EnsureManagedDocs(*wikiCfg)
			if err != nil {
				moePrintf(stderr, "wiki: %v\n", err)
				closeBootstrapFailedSession(closeSess, stderr)
				return 1
			}
			docsStubbed = stubbed
			// Seed the .wiki-ops stash so the agent has a fresh
			// scratchpad. Failure is non-fatal — the log entry
			// degrades to content-edit-only if the stash never
			// materialises.
			if err := wiki.EnsureOpsStash(wikiCfg.ContentDir); err != nil {
				moePrintf(stderr, "wiki: %v\n", err)
			}
		}
	}

	// Assemble the kickoff now that the worktree exists and wikiCfg
	// points at it. Callers that bake absolute bureaucracy paths into
	// the first user message (twin reflect) defer to this builder so
	// those paths land inside the worktree instead of the canonical
	// checkout — assembling the kickoff before the worktree existed is
	// what walked a reflect pass into the operator's live tree. Runs at
	// the same post-rewrite point as BuildPrompt and supersedes any
	// static spec.InitialPrompt.
	if spec.InitialPromptBuilder != nil {
		ip, err := spec.InitialPromptBuilder(workRoot, wikiCfg, docsStubbed)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			closeBootstrapFailedSession(closeSess, stderr)
			return 1
		}
		spec.InitialPrompt = ip
	}

	// Prompt paths point at the session worktree, where Claude's
	// edits land. When the session closes, those edits rebase +
	// ff-merge into main at the canonical root.
	prompt, err := spec.BuildPrompt(workRoot, wikiCfg)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		closeBootstrapFailedSession(closeSess, stderr)
		return 1
	}

	// spec.Agent is populated by runStageSession via stageAgentName;
	// test callers that build wikiTurnSpec directly may leave it empty.
	// Fall back through the same ladder with no run default so the
	// dispatch never sees an empty key.
	//
	// Also reflect the resolved name back into `in` so
	// reportWikiSessionExit attributes the "<agent> exited" line
	// honestly even when the caller (lint) didn't pre-populate
	// in.Agent.
	agentName := spec.Agent
	if agentName == "" {
		agentName = resolveAgentName("", "", "")
	}
	if in.Agent == "" {
		in.Agent = agentName
	}
	a, err := agent.Get(agentName)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		closeBootstrapFailedSession(closeSess, stderr)
		return 1
	}
	var runErr error
	var returnedSid string
	if spec.OnAgentStart != nil {
		spec.OnAgentStart()
	}
	if spec.Headless {
		// Hard-cap every headless turn's wall-clock. A headless stage has
		// no operator on stdin to Ctrl-C a wedged turn, and the dominant
		// wedge is an agent backgrounding a long-lived subprocess (e.g.
		// `moe serve`): a Claude Code turn won't end while a background
		// task is alive, so the turn hangs forever. 60min is well clear of
		// any legitimate well-scoped stage turn while still bounding the
		// wedge — model-independent, and a net under every future
		// "agent wedged a turn" variant, not just serve.
		// ThreadPath enables transcript mirroring on one-shot so the
		// post-Wait auto-tail has something to render. Empty for
		// run-less callers (e.g. the rebase-resolve fallback).
		var threadPath string
		if spec.Metadata != nil && spec.DocID != "" {
			threadPath = filepath.Join(workRoot, run.ThreadPathFor(in.Agent, spec.Metadata.Project, spec.Metadata.ID, spec.DocID))
		}
		returnedSid, runErr = a.ExecuteOneShot(agent.OneShotRequest{
			Root:       workRoot,
			Prompt:     prompt,
			UserPrompt: spec.InitialPrompt,
			ClonePath:  spec.ClonePath,
			SessionCwd: spec.SessionCwd,
			Model:      spec.Model,
			Stdout:     stdout,
			Stderr:     stderr,
			ExtraEnv:   spec.ExtraEnv,
			AddDirs:    spec.AddDirs,
			ThreadPath: threadPath,
			Timeout:    headlessTurnTimeout,
		})
		// Auto-tail: render the last few normalised events to stderr
		// so the operator sees "what just happened" without having
		// to `moe <workflow> log` after every headless exit. Best-effort — a
		// missing or parse-broken transcript is reported softly and
		// doesn't override the executor's exit status.
		if threadPath != "" {
			// spec.Metadata and spec.DocID are non-nil here by the same
			// guard that set threadPath above, so the command is fully
			// concrete — no placeholder fallback.
			logCmd := fmt.Sprintf("moe %s log %s/%s %s", spec.Metadata.Workflow, spec.Metadata.Project, spec.Metadata.ID, spec.DocID)
			tailHeadlessTranscript(in.Agent, threadPath, logCmd, stderr)
		}
	} else {
		returnedSid, runErr = a.Execute(agent.Request{
			Root:          workRoot,
			Metadata:      spec.Metadata,
			DocID:         spec.DocID,
			SessionID:     spec.SessionUUID,
			NewSession:    spec.NewSession,
			Prompt:        prompt,
			ClonePath:     spec.ClonePath,
			SessionCwd:    spec.SessionCwd,
			InitialPrompt: spec.InitialPrompt,
			Model:         spec.Model,
			Stdin:         os.Stdin,
			Stdout:        os.Stdout,
			Stderr:        stderr,
			ExtraEnv:      spec.ExtraEnv,
			AddDirs:       spec.AddDirs,
		})
	}

	// Codex generates its session id itself and reads it back post-
	// launch (rollout filename suffix for interactive, `thread.started`
	// JSON event for one-shot). Claude one-shot is the same shape —
	// it doesn't accept `--session-id` so it mints a fresh id that we
	// pull off the first `system/init` stream event. Interactive
	// Claude echoes the id we minted, so the `returnedSid !=
	// spec.SessionUUID` guard keeps it a no-op there.
	//
	// Persisting the returned id lets the next turn's `--resume`
	// point at the right transcript. Both headless and interactive
	// claude turns now share the same SessionCwd, so a headless →
	// interactive transition resolves to the same encoded-cwd
	// bucket and `--resume` works without recovery on turn 2.
	// Run-less callers (lint) carry no document to mutate.
	if spec.Metadata != nil && returnedSid != "" && returnedSid != spec.SessionUUID {
		if doc, ok := spec.Metadata.Documents[spec.DocID]; ok {
			doc.Session = returnedSid
			if err := run.Save(workRoot, spec.Metadata); err != nil {
				moePrintf(stderr, "session: persist returned id: %v\n", err)
			}
		}
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
			if !spec.SkipFinalize {
				_, ferr := wiki.FinalizeIngest(*wikiCfg, wiki.FinalizeContext{
					RunID:    spec.FinalizeRunID,
					RunTitle: spec.FinalizeRunTitle,
				}, stderr)
				if ferr != nil {
					moePrintf(stderr, "wiki: finalize failed: %v\n", ferr)
					moePrintln(stderr, "  agent edits will commit; checkpoint and "+
						"log.md were NOT written. Re-run the session or fix the "+
						"underlying issue before the next reflect.")
					finalizeErr = ferr
				}
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
	//
	// okToPush gates the in-closure sync.AutoPush: the bureaucracy
	// per-turn commit only races to origin when the agent's turn
	// genuinely succeeded. runErr / gateErr both mean the turn didn't
	// produce shippable output (codex turn.failed; reflect hygiene
	// scan caught residue), so we keep the local commit but suppress
	// the push — origin won't see it until a later successful turn.
	// commitErr / finalizeErr are not gates here: a finalize failure
	// leaves real agent edits on disk that the operator may want
	// mirrored to other machines, and a CanvasUnchangedError surfaces
	// through closeErr below regardless of the push toggle.
	okToPush := runErr == nil && gateErr == nil
	closeErr := closeWithAutoResolve(closeSess, okToPush, stdout, stderr)

	return reportWikiSessionExit(in, runErr, commitErr, closeErr, finalizeErr, gateErr, stdout, stderr)
}

// openWikiSession opens the session worktree under the repo lock and
// returns a closeSess closure already bound to the matching `-close`
// lock options. Centralising both halves means each early-failure path
// in runWikiSession is one `_ = closeSess(...)` line, and adding a new
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
//
// closeSess takes okToPush: when false, session.Close still runs (so
// the worktree is torn down and any committed work lands on local
// main), but sync.AutoPush is suppressed. The caller passes false when
// the executor's turn failed — bureaucracy must not race ahead of the
// project repo when the turn that motivated the commit didn't produce
// shippable output. The silent-failure-at-push run was the motivating
// incident: a failed push synthesis turn auto-pushed an empty "work:
// update push" commit to origin while the moe branch never reached its
// remote, leaving bureaucracy claiming the ship landed.
func openWikiSession(root string, in wikiSessionInputs, stdout, stderr io.Writer) (*session.Session, func(okToPush bool) error, error) {
	// Open (or resume) the session worktree under the repo lock.
	// The local work is just `git worktree add` (or a lookup); the
	// auto-pull before it can sit on the network briefly.
	var sess *session.Session
	err := repolock.With(root, repolock.Options{
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
	closeSess := func(okToPush bool) error {
		return repolock.With(root, repolock.Options{
			Purpose:   in.LockPurpose + "-close",
			Run:       in.Project + "/" + in.RunSlug,
			Heartbeat: true,
		}, func() error {
			if err := session.Close(sess); err != nil {
				return err
			}
			if !okToPush {
				return nil
			}
			return sync.AutoPush(root, stdout, stderr)
		})
	}
	return sess, closeSess, nil
}

// exitInterrupted is the exit code reportWikiSessionExit mints when the
// turn was cut short by an operator Ctrl-C (runErr is
// agent.ErrInterrupted) rather than a genuine stage failure. 130 is the
// conventional 128+SIGINT — distinct from the bare 1 a failed turn
// returns, so the cascade decision points (cascadeFromGate,
// maybeRideChain, dispatchCascade) can tell "operator interrupted a good
// turn" from "the stage failed" and hard-stop the chain instead of
// reacting as if a stage barfed.
const exitInterrupted = 130

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
//
// An operator Ctrl-C is the one runErr that exits 130 (exitInterrupted)
// rather than 1: the turn's commit is kept (the work is on disk, and
// push is already suppressed upstream because okToPush gates on
// runErr == nil), but the distinct code lets the cascade halt the whole
// chain instead of mistaking the interrupt for a failed stage.
func reportWikiSessionExit(in wikiSessionInputs, runErr, commitErr, closeErr, finalizeErr, gateErr error, stdout, stderr io.Writer) int {
	if runErr != nil {
		// in.Agent is populated by runWikiSession after agent resolution.
		// Empty falls back to "agent" — callers that bypass the resolver
		// (test stubs constructing wikiSessionInputs by hand) still get
		// a readable line.
		agentLabel := in.Agent
		if agentLabel == "" {
			agentLabel = "agent"
		}
		moePrintf(stderr, "%s exited: %v\n", agentLabel, runErr)
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
		moePrintf(stdout, "committed %s turn for %s/%s\n", in.DocID, in.Project, in.RunSlug)
	}
	if closeErr != nil {
		moePrintf(stderr, "session close: %v\n", closeErr)
		return 1
	}
	if runErr != nil || finalizeErr != nil || gateErr != nil {
		// An operator Ctrl-C during the turn is a stop, not a failure:
		// surface it as exitInterrupted so the cascade halts the chain
		// rather than reacting as if the stage barfed. finalizeErr /
		// gateErr collateral of an interrupted turn rides under the same
		// code — the interrupt is the dominant intent.
		if errors.Is(runErr, agent.ErrInterrupted) {
			return exitInterrupted
		}
		return 1
	}
	return 0
}

// writePromptSnapshot persists the assembled `--append-system-prompt`
// payload to <workRoot>/<docDir>/prompt-<agent>.md so the operator can
// inspect what the agent actually received. Same dir as the canvas
// and per-agent thread JSONL; commitTurn stages docDir wholesale, so
// the snapshot rides along in the per-turn commit without extra
// wiring. Overwrites each turn — the git history is the per-turn
// record; the file on disk is the latest.
//
// Soft-failure design: callers swallow the error to a stderr line so a
// debug-surface write doesn't break the agent's turn. The prompt has
// already been handed to the executor by then; the on-disk copy is
// strictly for the operator.
func writePromptSnapshot(workRoot, agent string, md *run.Metadata, docID, prompt string) error {
	rel := run.PromptPathFor(agent, md.Project, md.ID, docID)
	path := filepath.Join(workRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(prompt), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// sessionDocCwd is the cwd every claude stage hands to claude — a
// stable per-document path under <root>/.moe/sessions/<project>/<run>/<doc>/.
// Stable across turns because the inputs are stable; that's the whole
// point: claude encodes cwd into its on-disk project dir, so a churning
// cwd (e.g. the per-turn worktree path) leaves `--resume <sid>` looking
// in a fresh dir on every turn and reporting the session missing. Code
// stages don't get a different cwd — they reach the sandbox clone and
// the bureaucracy worktree via `--add-dir`. The dir itself stays empty
// of source — `.claude/skills/` is the one tree materialized inside it
// so claude's cwd-walkup skill discovery finds the moe-bureaucracy /
// moe-context skills.
func sessionDocCwd(root, projectID, runID, docID string) string {
	return filepath.Join(root, ".moe", "sessions", projectID, runID, docID)
}

// headlessTailLines is the default count for the post-headless
// auto-tail. Tuned by eyeball — about what fits on a laptop terminal
// without scrolling, while still showing the conversational arc
// (operator's prompt, the agent's last message or two, the final tool
// call and its result). The design left the exact number open ("~20
// is a guess; tune once we see real output"); revisit once we have
// real-world feedback.
const headlessTailLines = 20

// tailHeadlessTranscript reads threadPath, parses it with the
// per-agent adapter, and renders the last few normalised events to w
// so the operator sees what just happened after a one-shot exit. All
// failure paths are soft: a missing transcript (one-shot agent died
// before writing anything), a parse error, a render write error each
// produce a short note rather than overriding the executor's exit
// status. The auto-tail is "extra context", not a gate.
func tailHeadlessTranscript(agentName, threadPath, logCmd string, w io.Writer) {
	f, err := os.Open(threadPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		moePrintf(w, "auto-tail: %v\n", err)
		return
	}
	defer f.Close()
	events, err := transcript.Parse(agentName, f)
	if err != nil {
		moePrintf(w, "auto-tail parse: %v\n", err)
		return
	}
	if len(events) == 0 {
		return
	}
	moePrintln(w, "")
	moePrintf(w, "--- last %d transcript events (%s for full) ---\n", min(headlessTailLines, len(events)), logCmd)
	if err := transcript.Render(w, transcript.Tail(events, headlessTailLines), transcript.RenderOptions{}); err != nil {
		moePrintf(w, "auto-tail render: %v\n", err)
	}
}
