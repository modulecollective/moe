package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// pushCommand builds the `push` facade for a workflow. Same shape as
// lintCommand/reflectCommand — the factory closes over the workflow
// name so the Usage closure can render a runnable banner instead of a
// `<wf>` placeholder. runPushTyped itself stays workflow-agnostic: it
// loads the run, reads md.Workflow, and threads it into the per-call
// messages that need it. The wrapper discards the typed error so the
// Command.Run contract stays `int`; the cascade calls runPushTyped
// directly to pick up the deferred-to-recovery signal.
func pushCommand(workflow string) *Command {
	return &Command{
		Name:    "push",
		Summary: "ship the run's code branch: fast-forward merge to default, or open a PR with --pr",
		Run: func(args []string, stdout, stderr io.Writer) int {
			code, _ := runPushTyped(workflow, args, stdout, stderr)
			return code
		},
	}
}

const branchPrefix = "moe/"

type pushRunOptions struct {
	HeadlessRecovery bool
	SkipTerminalEdit bool
}

// PushDeferredError is the typed value runPushTyped returns when the
// pre-push gate hit a conflict or hook failure and pushed control to
// a fresh code session instead of shipping. The recovery session's
// own exit is propagated as the int return — non-zero if the agent
// gave up, zero if it resolved cleanly — but the typed error rides
// alongside so the caller can tell "push handed off" apart from
// "push shipped." The cascade reads this to render
// `push deferred to recovery (rebase conflict) — stopped` instead of
// claiming a ship that never happened.
//
// Recovery is "rebase-conflict" or "hook-failure". Project and Run
// echo the run the deferral fired on, so callers logging this don't
// have to plumb the metadata separately.
type PushDeferredError struct {
	Recovery string
	Project  string
	Run      string
}

func (e *PushDeferredError) Error() string {
	return fmt.Sprintf("push deferred to recovery (%s) for %s/%s", e.Recovery, e.Project, e.Run)
}

// pushCanvasSkeleton is the fixed structural shape every synthesized
// push canvas opens with. The `## PR body` section is what lands on an
// actual PR; fast-forward merge writes a deterministic note instead of
// running synthesis.
const pushCanvasSkeleton = `# Push

## PR body

(agent fills: the final PR body / merge commit message — curated from code's draft, amended by test's findings)

## Ship readiness

(agent fills: two or three sentences — what was verified, what wasn't, why this is ready to ship; or what's blocking)

## Conflicts surfaced

(agent fills: disagreements between code's draft and test's findings; empty if the two agree)
`

// pushSynthesisKickoff is the interactive synthesis session's first
// user message — same shape as the design/code/test kickoffs. Tells
// the agent to read the prior canvases and acknowledge state before
// drafting; without this, fresh sessions tend to dive into writing
// without first checking what code stage's draft already said.
const pushSynthesisKickoff = "The operator just opened this push synthesis session. " +
	"Read the code canvas (and the test canvas, if present) before replying, so your acknowledgement reflects " +
	"what's actually been drafted and verified. In one or two sentences, acknowledge where synthesis " +
	"stands (fresh start vs. resumed) and ask what they'd like to refine. Then wait for their reply."

// runPushSynthesisSession opens the push stage session that curates
// code's draft and test's findings into push/content.md. The PR path
// invokes the headless variant because PR creation needs body text
// synchronously; the fast-forward merge path writes a mechanical note
// instead. The interactive variant (headless=false) lives on for a
// future `moe sdlc <whatever>` verb that lets the operator iterate on
// the canvas by hand; no caller wires it today. SkipNextStage suppresses
// the post-session chain prompt — synthesis sits inside the push action,
// which owns its own routing.
func runPushSynthesisSession(projectID, runID string, headless bool, stdout, stderr io.Writer) int {
	opts := stageSessionOpts{
		NeedsSandbox:   true,
		Headless:       headless,
		SkipNextStage:  true,
		CanvasSkeleton: pushCanvasSkeleton,
	}
	if !headless {
		opts.InitialPrompt = pushSynthesisKickoff
	}
	return runStageSession(projectID, runID, "push", opts, stdout, stderr)
}

// runPushTyped ships the sandbox branch. The default path fast-forwards
// the target repo's default branch to include moe/<run>, deletes the
// remote branch, drops the sandbox clone, and marks the run `merged`.
// The `--pr` path is today's behavior: push the branch, open (or re-use)
// a PR, mark the run `pushed`, keep the sandbox. A pushed run later
// reconciles to merged/closed via `moe sync`.
//
// Idempotent on terminal runs: rerunning after a merged/closed run is
// a no-op that prints the terminal state and exits 0.
//
// Returns (exitCode, error). The exit code is what `moe sdlc push`
// hands back to the shell: 0 on a real ship; 0 on a recovery session
// that exited cleanly; non-zero on any other failure. The error is
// non-nil only on the deferred-to-recovery paths — `*PushDeferredError`
// carrying the recovery flavour — so the cascade can render
// `push deferred to recovery (...) — stopped` instead of claiming a
// ship that never happened. Standalone callers (pushCmd.Run) discard
// the error and propagate just the exit code.
func runPushTyped(workflow string, args []string, stdout, stderr io.Writer) (int, error) {
	return runPushTypedWithOptions(workflow, args, pushRunOptions{}, stdout, stderr)
}

func runPushTypedWithOptions(workflow string, args []string, opts pushRunOptions, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet(workflow+" push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prFlag := fs.Bool("pr", false, "open a PR instead of fast-forward merging to the default branch")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s push [--pr] <project>/<run>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Default: push moe/<run>, fast-forward-merge it into the target repo's")
		moePrintln(stderr, "default branch, delete the remote branch, and remove the sandbox clone.")
		moePrintln(stderr, "--pr: push moe/<run> and open (or re-use) a PR; leave the sandbox in place.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2, nil
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2, nil
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "moe %s push: %v\n", workflow, err)
		return 2, nil
	}

	if workflow == "sdlc" {
		resolved, code := resolveSDLCRunSlug(workflow+" push", projectID, runID, stdout, stderr)
		if code != 0 {
			return code, nil
		}
		runID = resolved
	}

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	// Terminal statuses short-circuit before touching the sandbox — the
	// clone is expected to be gone for `merged`, and for `closed` the
	// run is archived. Mirror today's "existing PR" idempotency.
	switch md.Status {
	case run.StatusMerged:
		if sha := push.MergedSHA(root, md.ID); sha != "" {
			moePrintf(stdout, "already merged at %s\n", git.ShortSHA(sha))
		} else {
			moePrintln(stdout, "already merged")
		}
		return 0, nil
	case run.StatusClosed:
		moePrintln(stdout, "already closed")
		return 0, nil
	}

	pj, err := project.Load(root, md.Project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	if err := checkCodeContent(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	clonePath, err := sandboxClonePath(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}
	branch := branchPrefix + md.ID
	// A clone left mid-rebase can only have come from a recovery turn
	// that resolved a conflict but couldn't finalize `git rebase
	// --continue` (codex-rebase-weirdness). EnsureRebasedOntoDefault is
	// about to re-run and would fail confusingly against that state, and
	// CheckCleanWorkTree would misread the staged resolution as an
	// uncommitted-edit lapse. Catch it first and hand the operator the
	// exact unblock — you can't stash out of a mid-rebase, so the only
	// moves are finish it or abort it.
	if push.RebaseInProgress(clonePath) {
		moePrintf(stderr, `push: sandbox clone is mid-rebase — a previous rebase stopped at a conflict and was never finished
       sandbox: %s
       finish it: GIT_EDITOR=true git -C %s rebase --continue   (resolve any remaining conflicts and `+"`git add`"+` them first)
       or abort:  git -C %s rebase --abort                       (discards the in-progress rebase; re-run `+"`moe %s push %s/%s`"+` to retry)
`, clonePath, clonePath, clonePath, md.Workflow, md.Project, md.ID)
		return 1, nil
	}
	if err := push.CheckCleanWorkTree(clonePath, md.Workflow); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}
	if err := push.CheckBranchHasCommits(clonePath, branch, pj.DefaultBranch, md.Workflow); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}
	if err := push.EnsureOrigin(clonePath, pj.Remote); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	hooks := hookEnv{
		Project:      md.Project,
		Run:          md.ID,
		Document:     "push",
		Workflow:     md.Workflow,
		Sandbox:      clonePath,
		Bureaucracy:  root,
		TargetBranch: pj.DefaultBranch,
	}
	if err := runHooks(root, hookEventPrePush, hooks, stdout, stderr); err != nil {
		var conflict *push.RebaseConflictError
		if errors.As(err, &conflict) {
			moePrintf(stderr, "%v\n", conflict)
			return openCodeSessionForRebaseConflict(md, conflict, opts.HeadlessRecovery, stdout, stderr)
		}
		var fail *hookFailure
		if errors.As(err, &fail) {
			moePrintf(stderr, "%v\n", fail)
			return openCodeSessionForHookFailure(md, fail, opts.HeadlessRecovery, stdout, stderr)
		}
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	// When origin already has moe/<run> (a prior `--pr` cycle, or a
	// re-run after an agent-side rebase resolved a conflict), the
	// upcoming push may not be a fast-forward — the local branch's
	// history could differ from origin's. Force-with-lease is harmless
	// when the two match and refuses to overwrite a concurrent update
	// when they don't. Skip when origin has no copy of the branch:
	// the first push is a plain push with -u to establish tracking.
	force := push.OriginHasBranch(clonePath, branch)

	if err := push.PushBranch(clonePath, branch, pj.Remote, force, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1, nil
	}

	if *prFlag {
		if code := runPushSynthesisSession(md.Project, md.ID, true, stdout, stderr); code != 0 {
			return code, nil
		}
		return openPRPath(root, md, pj, branch, stdout, stderr), nil
	}
	return mergePath(root, md, pj, clonePath, branch, opts.SkipTerminalEdit, stdout, stderr), nil
}

// init registers the rebase-onto-default check as the first pre-push
// built-in. Built-ins run before project scripts (in pre-push.d/) so
// the scripts see the tree the rebase produced — the one about to be
// pushed. Vetting the pre-rebase tree is how a stale call site against
// a sibling branch's API change slips past local hooks and breaks CI.
func init() {
	registerBuiltinHook(hookEventPrePush, builtinHook{
		Name: "rebase-onto-default",
		Run: func(env hookEnv, stdout, stderr io.Writer) error {
			branch := branchPrefix + env.Run
			return push.EnsureRebasedOntoDefault(env.Sandbox, branch, env.TargetBranch, stdout, stderr)
		},
	})
}

// openCodeSessionForRebaseConflict builds the rebase-specific chain-back
// kickoff for a fresh code session against the same run. It names the
// conflicting paths and the target branch, then propagate that session's
// exit code so a clean resolve-and-commit lets the workflow's chain prompt
// offer push next — same shape `moe <wf> code` already produces. Headless
// cascades pass headless=true so recovery runs as a one-shot turn instead
// of waiting in an interactive REPL with no operator on stdin. The second
// return is a *PushDeferredError marking the deferral so the cascade renders
// "deferred to recovery" instead of mistaking the recovery's clean exit for
// a successful ship.
//
// Overridable in tests.
var openCodeSessionForRebaseConflict = func(md *run.Metadata, conflict *push.RebaseConflictError, headless bool, stdout, stderr io.Writer) (int, error) {
	kickoff := buildRebaseConflictKickoff(md.Workflow, conflict)
	return openCodeRecoverySession(md, "rebase-conflict", headless, kickoff, stdout, stderr)
}

// buildRebaseConflictKickoff is the agent-facing kickoff prompt for a
// chain-back code session. Names the target branch, lists the
// conflicting paths (when git left any), and tells the agent what
// "done" looks like — resolve, commit, exit; the post-turn chain
// prompt will offer push.
func buildRebaseConflictKickoff(workflow string, c *push.RebaseConflictError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe %s push` just tried to rebase %s onto origin/%s and hit conflicts. ",
		workflow, c.Branch, c.DefaultBranch)
	b.WriteString("The rebase has been aborted, so the working tree is clean and the branch is back at its pre-rebase tip — you are starting from the conflict state, not mid-rebase.\n\n")
	if len(c.Conflicts) > 0 {
		b.WriteString("Files git flagged as conflicting on the abandoned rebase:\n")
		for _, p := range c.Conflicts {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Re-run the rebase yourself (`git rebase origin/%s` from the sandbox), resolve the conflicts, ",
		c.DefaultBranch)
	fmt.Fprintf(&b, "verify the result still does what the design intended, and commit. Then exit the session — the post-turn chain prompt will offer `moe %s push` next.\n", workflow)
	return b.String()
}

// openPRPath is the --pr behavior: open (or re-use) a PR for the
// already-pushed branch and record the first push's state. The
// sandbox is intentionally left in place — iteration via
// `moe <wf> code` stays a one-liner until the PR merges. Synthesis
// already ran in runPushTyped, so this path only consumes the push
// canvas when a new PR needs a body.
func openPRPath(root string, md *run.Metadata, pj *project.Metadata, branch string, stdout, stderr io.Writer) int {
	ghRepo, err := push.GHRepoSpec(pj.Remote)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	url, existing, err := push.FindOpenPR(ghRepo, branch)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if existing {
		moePrintf(stdout, "existing PR: %s\n", url)
	} else {
		// The shared push preflight synthesized the canvas. The
		// `## PR body` section is what `gh pr create --body-file`
		// reads below.
		bodyPath, cleanup, err := writePRBodyFile(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		defer cleanup()
		url, err = push.CreatePR(ghRepo, branch, pj.DefaultBranch, md.ID, bodyPath, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		moePrintf(stdout, "opened PR: %s\n", url)
	}

	// Only the first push flips status and records the MoE-PR trailer.
	// Re-runs just pushed branch updates to an already-recorded PR.
	if md.Status != run.StatusPushed {
		md.Status = run.StatusPushed
		if err := run.Save(root, md); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
		msg := fmt.Sprintf("push: %s/%s\n\n", md.Project, md.ID) +
			trailers.Block{
				Run:      md.ID,
				Project:  md.Project,
				Workflow: md.Workflow,
				Document: "push",
				PR:       url,
			}.String()
		err := repolock.With(root, repolock.Options{
			Purpose: "push-pr",
			Run:     md.Project + "/" + md.ID,
		}, func() error {
			return run.StageAndCommit(root, msg, runJSON)
		})
		if err != nil {
			moePrintf(stderr, "commit push record: %v\n", err)
			return 1
		}
	}
	return 0
}

// mergePath is the default path: fast-forward the target repo's
// default branch to include moe/<run>, delete the remote branch, drop
// the sandbox, and mark the run merged. Sandbox and branch deletion
// happen after the merge-push succeeds so a failure mid-flight leaves
// both intact for retry.
func mergePath(root string, md *run.Metadata, pj *project.Metadata, clonePath, branch string, skipTerminalEdit bool, stdout, stderr io.Writer) int {
	tipSHA, err := git.RevParse(clonePath, "refs/heads/"+branch)
	if err != nil {
		moePrintf(stderr, "push: resolve %s: %v\n", branch, err)
		return 1
	}
	touched := touchedChoresForBranch(root, md.Project, clonePath, pj.DefaultBranch, branch)

	// Harvest follow-ups and flip run.json to merged before the
	// ff-push: harvest (and any per-idea slug failures) must be
	// reversible, and FastForwardToDefault is the point of no return
	// for the merged transition. enterTerminal does the harvest under
	// lock so each createIdea sees a held bureaucracy lock. Interactive
	// push keeps the editor pop just like close; cascade push harvests
	// as-is so a headless ship has no hidden interactive surface.
	priorStatus := md.Status
	var paths []string
	err = repolock.With(root, repolock.Options{
		Purpose: "push-harvest",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		var ferr error
		paths, ferr = enterTerminal(root, md, run.StatusMerged, skipTerminalEdit)
		return ferr
	})
	if err != nil {
		moePrintf(stderr, "push: harvest: %v\n", err)
		return 1
	}

	moePrintf(stdout, "fast-forwarding %s to %s on %s...\n", pj.DefaultBranch, branch, pj.Remote)
	if err := push.FastForwardToDefault(clonePath, branch, pj.DefaultBranch, stdout, stderr); err != nil {
		// Roll back the status flip enterTerminal just wrote: the
		// remote merge didn't happen, so the run shouldn't be
		// "merged" on disk. Harvest commits and followups.md
		// rewrites stay; harvest is idempotent on retry.
		if rerr := revertTerminal(root, md, priorStatus); rerr != nil {
			moePrintf(stderr, "warning: revert run.json after ff-push failure: %v\n", rerr)
		}
		moePrintf(stderr, "%v\n", err)
		moePrintf(stderr, "       origin/%s may have advanced between the pre-push rebase and ff-push — re-run `moe %s push %s/%s`\n",
			pj.DefaultBranch, md.Workflow, md.Project, md.ID)
		return 1
	}

	if err := push.DeleteRemoteBranch(clonePath, branch, stdout, stderr); err != nil {
		// Merge already landed; warn but don't fail the command.
		moePrintf(stderr, "warning: %v\n", err)
	}

	pushCanvasPath, err := writeMechanicalPushNote(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	paths = append(paths, pushCanvasPath)

	msg := fmt.Sprintf("push: %s/%s merged\n\n", md.Project, md.ID) +
		trailers.Block{
			Run:          md.ID,
			Project:      md.Project,
			Workflow:     md.Workflow,
			Document:     "push",
			Merged:       tipSHA,
			ChoreTouched: touched,
		}.String()
	err = repolock.With(root, repolock.Options{
		Purpose: "push-merge",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		if err := releaseRunWorkspace(root, md); err != nil {
			moePrintf(stderr, "warning: release workspace: %v\n", err)
		}
		if err := run.StageAndCommit(root, msg, paths...); err != nil {
			return err
		}
		// Advance the bureaucracy's gitlink for this project now that
		// origin's default branch has moved to the merged tip. Soft-warn
		// on failure: the remote merge already landed, so the operator
		// can recover with an explicit `moe sync`.
		if err := sync.BumpOne(root, md.Project, stdout, stderr); err != nil {
			moePrintf(stderr, "warning: auto-bump project pointer: %v\n", err)
		}
		return nil
	})
	if err != nil {
		moePrintf(stderr, "commit merge record: %v\n", err)
		return 1
	}
	moePrintf(stdout, "merged %s/%s at %s\n", md.Project, md.ID, git.ShortSHA(tipSHA))
	return 0
}

// writeMechanicalPushNote leaves an explicit push canvas for the
// fast-forward merge path without running synthesis. The note points
// future readers at the canonical records and names the test canvas
// only when that stage actually wrote one.
func writeMechanicalPushNote(root string, md *run.Metadata) (string, error) {
	rel := run.ContentPath(md.Project, md.ID, "push")
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("push: create push canvas dir: %w", err)
	}

	codeRel := run.ContentPath(md.Project, md.ID, "code")
	testRel := run.ContentPath(md.Project, md.ID, "test")
	testLine := fmt.Sprintf("- No test-stage canvas was present at `%s`.\n", testRel)
	if _, err := os.Stat(filepath.Join(root, testRel)); err == nil {
		testLine = fmt.Sprintf("- Test-stage record: `%s`.\n", testRel)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("push: stat test canvas: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Push\n\n")
	b.WriteString("Shipped by fast-forward merge. No push synthesis was run for this path.\n\n")
	b.WriteString("Authoritative records:\n")
	fmt.Fprintf(&b, "- Code-stage record: `%s`.\n", codeRel)
	b.WriteString(testLine)
	b.WriteString("- Target git history and the terminal commit trailers on this push record.\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("push: write push canvas: %w", err)
	}
	return rel, nil
}

// writePRBodyFile reads the push canvas, slices out the `## PR body`
// section, and writes the trimmed content to a tempfile. Returns the
// tempfile path and a cleanup func the caller defers. Errors if the
// push canvas is missing, unreadable, or has no `## PR body` section
// — synthesis ran immediately before this call, so any of those is a
// real failure the operator needs to see, not a soft fallback to
// stale source.
func writePRBodyFile(root string, md *run.Metadata) (string, func(), error) {
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "push"))
	canvas, err := os.ReadFile(canvasPath)
	if err != nil {
		return "", nil, fmt.Errorf("push: read push canvas: %w", err)
	}
	body := extractMarkdownSection(string(canvas), "PR body")
	if body == "" {
		return "", nil, fmt.Errorf("push: push canvas %s has no `## PR body` section (synthesis produced nothing usable)", canvasPath)
	}
	f, err := os.CreateTemp("", "moe-pr-body-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("push: create pr-body tempfile: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("push: write pr-body tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("push: close pr-body tempfile: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	return path, cleanup, nil
}

// extractMarkdownSection returns the body under a top-level `## <name>`
// heading: every line between that heading and the next `## ` heading
// (or EOF), trimmed. Returns "" if the heading is not present. Used to
// pull `## PR body` out of the push canvas without redoing markdown
// parsing — the canvas has a fixed skeleton so a line-by-line slice is
// enough.
func extractMarkdownSection(md, name string) string {
	want := "## " + name
	lines := strings.Split(md, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == want {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

func checkCodeContent(root string, md *run.Metadata) error {
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("push: code document not written yet; run `moe %s code %s/%s` first", md.Workflow, md.Project, md.ID)
		}
		return fmt.Errorf("push: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("push: code document is empty; run `moe %s code %s/%s` and produce a PR body first", md.Workflow, md.Project, md.ID)
	}
	return nil
}

func sandboxClonePath(root string, md *run.Metadata) (string, error) {
	wp, err := resolveRunWorkspacePath(root, md)
	if err != nil {
		return "", fmt.Errorf("push: %w", err)
	}
	return wp, nil
}
