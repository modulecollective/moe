package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

var pushCmd = &Command{
	Name:    "push",
	Summary: "ship the run's code branch: fast-forward merge to default, or open a PR with --pr",
	Run:     runPush,
}

const branchPrefix = "moe/"

// runPush ships the sandbox branch. The default path fast-forwards the
// target repo's default branch to include moe/<run>, deletes the remote
// branch, drops the sandbox clone, and marks the run `merged`. The
// `--pr` path is today's behavior: push the branch, open (or re-use) a
// PR, mark the run `pushed`, keep the sandbox. A pushed run later
// reconciles to merged/closed via `moe sync`.
//
// Idempotent on terminal runs: rerunning after a merged/closed run is
// a no-op that prints the terminal state and exits 0.
func runPush(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prFlag := fs.Bool("pr", false, "open a PR instead of fast-forward merging to the default branch")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe <wf> push [--pr] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Default: push moe/<run>, fast-forward-merge it into the target repo's")
		moePrintln(stderr, "default branch, delete the remote branch, and remove the sandbox clone.")
		moePrintln(stderr, "--pr: push moe/<run> and open (or re-use) a PR; leave the sandbox in place.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, runID := fs.Arg(0), fs.Arg(1)

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

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Terminal statuses short-circuit before touching the sandbox — the
	// clone is expected to be gone for `merged`, and for `closed` the
	// run is archived. Mirror today's "existing PR" idempotency.
	switch md.Status {
	case run.StatusMerged:
		if sha := mergedSHA(root, md.ID); sha != "" {
			moePrintf(stdout, "already merged at %s\n", git.ShortSHA(sha))
		} else {
			moePrintln(stdout, "already merged")
		}
		return 0
	case run.StatusClosed:
		moePrintln(stdout, "already closed")
		return 0
	}

	pj, err := project.Load(root, md.Project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if err := checkCodeContent(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	clonePath, err := sandboxClonePath(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	branch := branchPrefix + md.ID
	if err := checkCleanWorkTree(clonePath); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := checkBranchHasCommits(clonePath, branch, pj.DefaultBranch); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := ensureOrigin(clonePath, pj.Remote); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
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
		var conflict *rebaseConflictError
		if errors.As(err, &conflict) {
			moePrintf(stderr, "%v\n", conflict)
			return openCodeSessionForRebaseConflict(md, conflict, stdout, stderr)
		}
		var fail *hookFailure
		if errors.As(err, &fail) {
			moePrintf(stderr, "%v\n", fail)
			return openCodeSessionForHookFailure(md, fail, stdout, stderr)
		}
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// When origin already has moe/<run> (a prior `--pr` cycle, or a
	// re-run after an agent-side rebase resolved a conflict), the
	// upcoming push may not be a fast-forward — the local branch's
	// history could differ from origin's. Force-with-lease is harmless
	// when the two match and refuses to overwrite a concurrent update
	// when they don't. Skip when origin has no copy of the branch:
	// the first push is a plain push with -u to establish tracking.
	force := originHasBranch(clonePath, branch)

	if err := pushBranch(clonePath, branch, pj.Remote, force, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if *prFlag {
		return openPRPath(root, md, pj, branch, stdout, stderr)
	}
	return mergePath(root, md, pj, clonePath, branch, stdout, stderr)
}

// pushBranch pushes moe/<run> to origin with -u so a fresh branch
// gets tracking set. Streams stdout/stderr so credential prompts and
// progress stay visible. force=true switches to --force-with-lease for
// the case where origin already has the branch and the local copy may
// have been rewritten (rebased) since the previous push; the lease
// still refuses to overwrite a concurrent third-party update.
func pushBranch(clonePath, branch, remote string, force bool, stdout, stderr io.Writer) error {
	moePrintf(stdout, "pushing %s to %s...\n", branch, remote)
	args := []string{"-C", clonePath, "push", "-u"}
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "origin", branch)
	cmd := exec.Command("git", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: git push: %w", err)
	}
	return nil
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
			return syncBranchBeforePush(env.Sandbox, branch, env.TargetBranch, stdout, stderr)
		},
	})
}

// rebaseConflictError carries the failed rebase's diagnostic context
// from syncBranchBeforePush up to runPush, where it triggers the
// chain-back to a fresh code session.
type rebaseConflictError struct {
	branch        string
	defaultBranch string
	conflicts     []string
}

func (e *rebaseConflictError) Error() string {
	return fmt.Sprintf("push: rebase %s onto origin/%s hit conflicts in %d file(s); aborted",
		e.branch, e.defaultBranch, len(e.conflicts))
}

// syncBranchBeforePush fetches origin's default branch, then rebases
// moe/<run> onto it if origin has moved past the merge-base. Returns
// nil when the branch was already up to date or after a clean rebase,
// and *rebaseConflictError on a rebase conflict (the rebase is aborted
// before returning so the clone is left clean for the chain-back).
func syncBranchBeforePush(clonePath, branch, defaultBranch string, stdout, stderr io.Writer) error {
	moePrintf(stdout, "fetching origin/%s...\n", defaultBranch)
	if out, err := git.Combined(clonePath, "fetch", "origin", defaultBranch); err != nil {
		return fmt.Errorf("push: fetch origin/%s: %w (%s)", defaultBranch, err, out)
	}

	originRef := "refs/remotes/origin/" + defaultBranch
	originSHA, err := git.RevParse(clonePath, originRef)
	if err != nil {
		return fmt.Errorf("push: resolve %s: %w", originRef, err)
	}
	branchSHA, err := git.RevParse(clonePath, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("push: resolve %s: %w", branch, err)
	}

	// origin/<default> already an ancestor of moe/<run> means the run
	// branch already includes everything on default — no rebase needed.
	if _, err := git.Combined(clonePath, "merge-base", "--is-ancestor", originSHA, branchSHA); err == nil {
		return nil
	}

	moePrintf(stdout, "rebasing %s onto origin/%s...\n", branch, defaultBranch)
	out, err := git.Combined(clonePath, "rebase", originRef)
	if err != nil {
		// Capture the conflict file list before the abort discards the
		// rebase state, so the kickoff prompt can name them.
		conflicts := unmergedPaths(clonePath)
		if _, abortErr := git.Combined(clonePath, "rebase", "--abort"); abortErr != nil {
			// If --abort itself fails the clone is in a bad state and the
			// operator needs to look — surface that specifically.
			return fmt.Errorf("push: rebase %s onto origin/%s failed and --abort also failed: %w (%s)",
				branch, defaultBranch, abortErr, out)
		}
		return &rebaseConflictError{
			branch:        branch,
			defaultBranch: defaultBranch,
			conflicts:     conflicts,
		}
	}
	moePrintf(stdout, "rebased %s onto origin/%s\n", branch, defaultBranch)
	return nil
}

// originHasBranch returns true when origin currently advertises
// refs/heads/<branch> — i.e. a prior `--pr` cycle already pushed this
// branch and the upcoming push needs --force-with-lease (when paired
// with a rebase). Uses `git ls-remote` because the clone may not have
// a remote-tracking ref for moe/<run>.
func originHasBranch(clonePath, branch string) bool {
	out, err := git.Output(clonePath, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		// Treat ls-remote failure as "unknown" → fall back to plain push.
		// A real network failure will surface again on the actual push.
		return false
	}
	return strings.TrimSpace(out) != ""
}

// unmergedPaths reports the files git left in a conflicted (UU/AA/...)
// state — the same set `git status -s` would show with U/A markers. Used
// to name conflicting paths in the chain-back kickoff.
func unmergedPaths(clonePath string) []string {
	entries, err := git.Status(clonePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		// XY codes: UU/AA/DD/AU/UA/DU/UD all indicate unmerged state.
		if e.XY[0] == 'U' || e.XY[1] == 'U' || e.XY == "AA" || e.XY == "DD" {
			out = append(out, e.Path)
		}
	}
	return out
}

// openCodeSessionForRebaseConflict is the chain-back: spawn a fresh
// interactive code session against the same run with a kickoff prompt
// that names the conflicting paths and the target branch, then exit
// non-zero so the operator knows to re-run `push` after the agent
// resolves and commits.
//
// Overridable in tests; the default invokes runStageSession directly
// with docID="code", same as `moe <wf> code` would.
var openCodeSessionForRebaseConflict = func(md *run.Metadata, conflict *rebaseConflictError, stdout, stderr io.Writer) int {
	moePrintln(stderr, "       opening a fresh code session — resolve the conflicts, commit, then re-run push")
	kickoff := buildRebaseConflictKickoff(md.Workflow, conflict)
	_ = runStageSession(md.Project, md.ID, "code", stageSessionOpts{
		NeedsSandbox:  true,
		InitialPrompt: kickoff,
		// SkipNextStage so the post-turn prompt doesn't offer to chain
		// straight into push — the operator needs to re-run push by
		// hand once the conflict is resolved and committed.
		SkipNextStage: true,
	}, stdout, stderr)
	// Always exit non-zero from push — the merge didn't happen, and the
	// operator's next invocation should be `moe <wf> push` again.
	return 1
}

// buildRebaseConflictKickoff is the agent-facing kickoff prompt for a
// chain-back code session. Names the target branch, lists the
// conflicting paths (when git left any), and tells the agent what
// "done" looks like — resolve, commit, exit so the operator can re-run
// push.
func buildRebaseConflictKickoff(workflow string, c *rebaseConflictError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe %s push` just tried to rebase %s onto origin/%s and hit conflicts. ",
		workflow, c.branch, c.defaultBranch)
	b.WriteString("The rebase has been aborted, so the working tree is clean and the branch is back at its pre-rebase tip — you are starting from the conflict state, not mid-rebase.\n\n")
	if len(c.conflicts) > 0 {
		b.WriteString("Files git flagged as conflicting on the abandoned rebase:\n")
		for _, p := range c.conflicts {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Re-run the rebase yourself (`git rebase origin/%s` from the sandbox), resolve the conflicts, ",
		c.defaultBranch)
	fmt.Fprintf(&b, "verify the result still does what the design intended, and commit. Then exit the session and tell the operator to re-run `moe %s push` to ship.\n", workflow)
	return b.String()
}

// openPRPath is the --pr behavior: open (or re-use) a PR for the
// already-pushed branch and record the first push's state. The
// sandbox is intentionally left in place — iteration via
// `moe <wf> code` stays a one-liner until the PR merges.
func openPRPath(root string, md *run.Metadata, pj *project.Metadata, branch string, stdout, stderr io.Writer) int {
	ghRepo, err := ghRepoSpec(pj.Remote)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	url, existing, err := findOpenPR(ghRepo, branch)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if existing {
		moePrintf(stdout, "existing PR: %s\n", url)
	} else {
		bodyPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
		url, err = createPR(ghRepo, branch, pj.DefaultBranch, md.Title, bodyPath, stderr)
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
		msg := fmt.Sprintf(`push: %s/%s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: push
MoE-PR: %s
`, md.Project, md.ID, md.ID, md.Project, md.Workflow, url)
		err := withRepoLock(root, repolock.Options{
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
func mergePath(root string, md *run.Metadata, pj *project.Metadata, clonePath, branch string, stdout, stderr io.Writer) int {
	tipSHA, err := git.RevParse(clonePath, "refs/heads/"+branch)
	if err != nil {
		moePrintf(stderr, "push: resolve %s: %v\n", branch, err)
		return 1
	}

	// Harvest follow-ups and flip run.json to merged before the
	// ff-push: harvest (and any per-idea slug failures) must be
	// reversible, and ffPushToDefault is the point of no return for
	// the merged transition. enterTerminal does the harvest under
	// lock so each createIdea sees a held bureaucracy lock.
	priorStatus := md.Status
	var paths []string
	err = withRepoLock(root, repolock.Options{
		Purpose: "push-harvest",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		var ferr error
		paths, ferr = enterTerminal(root, md, run.StatusMerged, true)
		return ferr
	})
	if err != nil {
		moePrintf(stderr, "push: harvest: %v\n", err)
		return 1
	}

	moePrintf(stdout, "fast-forwarding %s to %s on %s...\n", pj.DefaultBranch, branch, pj.Remote)
	if err := ffPushToDefault(clonePath, branch, pj.DefaultBranch, stdout, stderr); err != nil {
		// Roll back the status flip enterTerminal just wrote: the
		// remote merge didn't happen, so the run shouldn't be
		// "merged" on disk. Harvest commits and followups.md
		// rewrites stay; harvest is idempotent on retry.
		if rerr := revertTerminal(root, md, priorStatus); rerr != nil {
			moePrintf(stderr, "warning: revert run.json after ff-push failure: %v\n", rerr)
		}
		moePrintf(stderr, "%v\n", err)
		moePrintf(stderr, "       origin/%s may have advanced between the pre-push rebase and ff-push — re-run `moe %s push %s %s`\n",
			pj.DefaultBranch, md.Workflow, md.Project, md.ID)
		return 1
	}

	if err := deleteRemoteBranch(clonePath, branch, stdout, stderr); err != nil {
		// Merge already landed; warn but don't fail the command.
		moePrintf(stderr, "warning: %v\n", err)
	}

	msg := fmt.Sprintf(`push: %s/%s merged

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: push
MoE-Merged: %s
`, md.Project, md.ID, md.ID, md.Project, md.Workflow, tipSHA)
	err = withRepoLock(root, repolock.Options{
		Purpose: "push-merge",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		if err := releaseRunWorkspace(root, md); err != nil {
			moePrintf(stderr, "warning: release workspace: %v\n", err)
		}
		return run.StageAndCommit(root, msg, paths...)
	})
	if err != nil {
		moePrintf(stderr, "commit merge record: %v\n", err)
		return 1
	}
	moePrintf(stdout, "merged %s/%s at %s\n", md.Project, md.ID, git.ShortSHA(tipSHA))
	return 0
}

// ffPushToDefault fast-forwards the remote's default branch to the
// tip of moe/<run> via `git push origin <branch>:<default>`. The push
// is rejected server-side if it isn't a fast-forward — no --force, ever.
func ffPushToDefault(clonePath, branch, defaultBranch string, stdout, stderr io.Writer) error {
	cmd := exec.Command("git", "-C", clonePath, "push", "origin", branch+":"+defaultBranch)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: ff-merge %s into %s: %w", branch, defaultBranch, err)
	}
	return nil
}

// deleteRemoteBranch removes moe/<run> from origin. Non-fatal on
// failure — the merge has already landed; a stray remote branch is a
// minor cleanup issue, not worth rolling back the run for.
func deleteRemoteBranch(clonePath, branch string, stdout, stderr io.Writer) error {
	moePrintf(stdout, "deleting %s on origin...\n", branch)
	cmd := exec.Command("git", "-C", clonePath, "push", "origin", "--delete", branch)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: delete remote branch %s: %w", branch, err)
	}
	return nil
}

// mergedSHA returns the merge SHA recorded in the most recent
// MoE-Merged trailer for runID, or "" if none has been recorded.
func mergedSHA(root, runID string) string {
	return trailerValue(root, runID, "MoE-Merged")
}

// trailerValue pulls the value from the most recent `<trailer>:` line
// in any commit that also carries `MoE-Run: <runID>`. Returns "" when
// no such commit exists.
func trailerValue(root, runID, trailer string) string {
	cmd := exec.Command("git", "-C", root, "log",
		"--all-match",
		"--grep", "MoE-Run: "+runID,
		"--grep", trailer+":",
		"--format=%B", "-z",
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	prefix := trailer + ":"
	for _, body := range strings.Split(string(out), "\x00") {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

func checkCodeContent(root string, md *run.Metadata) error {
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("push: code document not written yet; run `moe %s code %s %s` first", md.Workflow, md.Project, md.ID)
		}
		return fmt.Errorf("push: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("push: code document is empty; run `moe %s code %s %s` and produce a PR body first", md.Workflow, md.Project, md.ID)
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

// checkCleanWorkTree refuses to push when the sandbox has uncommitted
// changes — staged, unstaged, or untracked. The agent is responsible for
// committing inside the sandbox before exiting; if it didn't, the loose
// edits would silently be left behind by the push and we'd ship a branch
// that doesn't reflect the agent's actual work. Better to surface it.
func checkCleanWorkTree(clonePath string) error {
	entries, err := git.Status(clonePath)
	if err != nil {
		return fmt.Errorf("push: git status in sandbox: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	return fmt.Errorf(`push: sandbox clone has %d uncommitted file(s) — the agent edited but did not commit
       sandbox: %s
       re-run `+"`moe <wf> code`"+` and ask the agent to commit, or commit manually in the sandbox`, len(entries), clonePath)
}

// checkBranchHasCommits confirms the sandbox clone has branch `branch`
// and that it's ahead of `base`. A branch at zero commits-ahead means the
// agent didn't actually commit anything.
func checkBranchHasCommits(clonePath, branch, base string) error {
	// First, does the branch exist?
	cmd := exec.Command("git", "-C", clonePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: branch %q does not exist in sandbox clone; run `moe <wf> code` and have the agent commit", branch)
	}
	// Then, is it ahead of base? Use `git rev-list --count base..branch`.
	// If base isn't a known ref, skip this check — we can't tell, but the
	// push itself will error clearly.
	out, err := exec.Command("git", "-C", clonePath, "rev-list", "--count", base+".."+branch).Output()
	if err != nil {
		return nil
	}
	count := strings.TrimSpace(string(out))
	if count == "0" {
		return fmt.Errorf("push: branch %q has no commits ahead of %q; nothing to push", branch, base)
	}
	return nil
}

// ensureOrigin makes sure origin in the sandbox clone points at the
// target project remote. Fresh clones have origin pointing at the local
// submodule path (the clone source), which cannot be pushed to GitHub.
func ensureOrigin(clonePath, remote string) error {
	out, err := exec.Command("git", "-C", clonePath, "remote", "get-url", "origin").Output()
	if err != nil {
		// No origin at all — add one.
		cmd := exec.Command("git", "-C", clonePath, "remote", "add", "origin", remote)
		if combined, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("push: add origin: %w (%s)", err, strings.TrimSpace(string(combined)))
		}
		return nil
	}
	current := strings.TrimSpace(string(out))
	if current == remote {
		return nil
	}
	cmd := exec.Command("git", "-C", clonePath, "remote", "set-url", "origin", remote)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push: set-url origin: %w (%s)", err, strings.TrimSpace(string(combined)))
	}
	return nil
}

// ghRepoSpec derives the owner/repo spec that `gh --repo` wants from the
// project's remote URL. Accepts HTTPS (https://github.com/owner/repo[.git])
// and SSH (git@github.com:owner/repo[.git]) forms.
func ghRepoSpec(remote string) (string, error) {
	s := strings.TrimSuffix(remote, ".git")
	// SSH form: git@host:owner/repo
	if i := strings.Index(s, "@"); i >= 0 {
		if j := strings.Index(s, ":"); j >= 0 && j > i {
			return s[j+1:], nil
		}
	}
	// HTTPS form: https://host/owner/repo
	if idx := strings.Index(s, "://"); idx >= 0 {
		after := s[idx+3:]
		if slash := strings.Index(after, "/"); slash >= 0 {
			return after[slash+1:], nil
		}
	}
	return "", fmt.Errorf("push: cannot derive owner/repo from remote %q", remote)
}

// findOpenPR returns (url, exists, err) for an open PR on branch in repo.
// Uses `gh pr list` rather than `gh pr view` because list returns an empty
// array on no-match (exit 0) while view exits non-zero in the same case,
// and distinguishing "no PR" from "gh failed" matters.
func findOpenPR(repo, branch string) (string, bool, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--repo", repo,
		"--head", branch,
		"--state", "open",
		"--json", "url",
		"--limit", "1",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", false, fmt.Errorf("push: gh CLI not found on PATH; install https://cli.github.com/")
		}
		return "", false, fmt.Errorf("push: gh pr list: %w", err)
	}
	var items []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		return "", false, fmt.Errorf("push: parse gh pr list output: %w", err)
	}
	if len(items) == 0 {
		return "", false, nil
	}
	return items[0].URL, true, nil
}

// createPR shells out to `gh pr create` and returns the URL printed on
// stdout. Errors from gh (auth, permissions, repo not found) propagate
// with their stderr attached.
func createPR(repo, head, base, title, bodyFile string, stderr io.Writer) (string, error) {
	cmd := exec.Command("gh", "pr", "create",
		"--repo", repo,
		"--head", head,
		"--base", base,
		"--title", title,
		"--body-file", bodyFile,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("push: gh CLI not found on PATH; install https://cli.github.com/")
		}
		return "", fmt.Errorf("push: gh pr create: %w", err)
	}
	// gh prints the PR URL on stdout, plus sometimes extra lines. Grab the
	// first https:// line.
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line, nil
		}
	}
	return "", fmt.Errorf("push: gh pr create succeeded but printed no URL: %q", out.String())
}
