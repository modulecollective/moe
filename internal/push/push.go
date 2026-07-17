// Package push implements the git-side primitives behind `moe <wf>
// push`: pre-flight checks on the sandbox clone, the
// rebase-onto-default sync used by the pre-push hook,
// fast-forward-merging the run branch into the target's default
// branch, deleting the remote branch on success, and the GitHub PR
// flow used when --pr is set.
//
// The entry-point handler (cli/push.go) keeps the bureaucracy-side
// glue (run.json status flips through enterTerminal, repolock around
// commits, the chain-back to a fresh code session on conflict). This
// package is the operations layer those steps delegate to: every
// function here takes explicit clonePath / branch / remote arguments
// and emits styled progress through cliout, so the same primitives
// can be reused by a future non-CLI caller (engine API, scheduler)
// without going through `cli.Run`.
package push

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/trailers"
)

// RebaseConflictError carries the failed rebase's diagnostic context
// from EnsureRebasedOntoDefault up to the caller, where it triggers
// the chain-back to a fresh code session. Populated only on a
// conflict; nil from EnsureRebasedOntoDefault means the run branch
// was already up to date or the rebase landed cleanly.
type RebaseConflictError struct {
	Branch        string
	DefaultBranch string
	Conflicts     []string
}

func (e *RebaseConflictError) Error() string {
	return fmt.Sprintf("push: rebase %s onto origin/%s hit conflicts in %d file(s); aborted",
		e.Branch, e.DefaultBranch, len(e.Conflicts))
}

// EnsureRebasedOntoDefault fetches origin's default branch, then
// rebases `branch` onto it if origin has moved past the merge-base.
// Returns nil when the branch was already up to date or after a clean
// rebase, and *RebaseConflictError on a rebase conflict (the rebase
// is aborted before returning so the clone is left clean for the
// chain-back).
func EnsureRebasedOntoDefault(clonePath, branch, defaultBranch string, stdout, stderr io.Writer) error {
	cliout.Printf(stdout, "fetching origin/%s...\n", defaultBranch)
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

	cliout.Printf(stdout, "rebasing %s onto origin/%s...\n", branch, defaultBranch)
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
		return &RebaseConflictError{
			Branch:        branch,
			DefaultBranch: defaultBranch,
			Conflicts:     conflicts,
		}
	}
	cliout.Printf(stdout, "rebased %s onto origin/%s\n", branch, defaultBranch)
	return nil
}

// DefaultAdvanced reports whether origin's default branch has moved
// past the local run branch — the one cause of a rejected ff-push that
// a retry can fix. It fetches origin/<defaultBranch>, then checks
// whether origin/<defaultBranch> is an ancestor of <branch>:
//
//   - not an ancestor → origin advanced past the branch tip → true.
//   - is an ancestor  → the branch still contains origin's tip, so the
//     ff-push rejection had another cause (auth, protected branch,
//     network) → false.
//
// This is the same fetch + is-ancestor probe EnsureRebasedOntoDefault
// opens with, run after a rejected ff-push to decide whether looping
// back through the pre-push hooks — which re-fetch and re-rebase — could
// turn the push into a fast-forward. Gating retry on it also stops an
// infinite loop on a persistent non-advance rejection: those return
// false and give up on the first attempt.
func DefaultAdvanced(clonePath, branch, defaultBranch string) (bool, error) {
	if out, err := git.Combined(clonePath, "fetch", "origin", defaultBranch); err != nil {
		return false, fmt.Errorf("push: fetch origin/%s: %w (%s)", defaultBranch, err, out)
	}
	originRef := "refs/remotes/origin/" + defaultBranch
	originSHA, err := git.RevParse(clonePath, originRef)
	if err != nil {
		return false, fmt.Errorf("push: resolve %s: %w", originRef, err)
	}
	branchSHA, err := git.RevParse(clonePath, "refs/heads/"+branch)
	if err != nil {
		return false, fmt.Errorf("push: resolve %s: %w", branch, err)
	}
	// origin/<default> already an ancestor of the branch → the branch
	// contains origin's tip → origin did not advance past it.
	if _, err := git.Combined(clonePath, "merge-base", "--is-ancestor", originSHA, branchSHA); err == nil {
		return false, nil
	}
	return true, nil
}

// PushBranch pushes `branch` to `remote` with -u so a fresh branch
// gets tracking set. Streams stdout/stderr so credential prompts and
// progress stay visible. force=true switches to --force-with-lease
// for the case where origin already has the branch and the local
// copy may have been rewritten (rebased) since the previous push;
// the lease still refuses to overwrite a concurrent third-party
// update.
func PushBranch(clonePath, branch, remote string, force bool, stdout, stderr io.Writer) error {
	cliout.Printf(stdout, "pushing %s to %s...\n", branch, remote)
	args := []string{"push", "-u"}
	if force {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "origin", branch)
	if err := git.Stream(clonePath, stdout, stderr, args...); err != nil {
		return fmt.Errorf("push: git push: %w", err)
	}
	return nil
}

// OriginHasBranch returns true when origin currently advertises
// refs/heads/<branch> — i.e. a prior `--pr` cycle already pushed this
// branch and the upcoming push needs --force-with-lease (when paired
// with a rebase). Uses `git ls-remote` because the clone may not have
// a remote-tracking ref for the run branch.
func OriginHasBranch(clonePath, branch string) bool {
	out, err := git.Output(clonePath, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		// Treat ls-remote failure as "unknown" → fall back to plain push.
		// A real network failure will surface again on the actual push.
		return false
	}
	return strings.TrimSpace(out) != ""
}

// FastForwardToDefault fast-forwards the remote's default branch to
// the tip of `branch` via `git push origin <branch>:<default>`. The
// push is rejected server-side if it isn't a fast-forward — no
// --force, ever.
func FastForwardToDefault(clonePath, branch, defaultBranch string, stdout, stderr io.Writer) error {
	if err := git.Stream(clonePath, stdout, stderr, "push", "origin", branch+":"+defaultBranch); err != nil {
		return fmt.Errorf("push: ff-merge %s into %s: %w", branch, defaultBranch, err)
	}
	return nil
}

// DeleteRemoteBranch removes `branch` from origin. The caller decides
// whether failure is fatal — the merge path treats it as a soft
// warning because the merge has already landed by then.
func DeleteRemoteBranch(clonePath, branch string, stdout, stderr io.Writer) error {
	cliout.Printf(stdout, "deleting %s on origin...\n", branch)
	if err := git.Stream(clonePath, stdout, stderr, "push", "origin", "--delete", branch); err != nil {
		return fmt.Errorf("push: delete remote branch %s: %w", branch, err)
	}
	return nil
}

// CheckCleanWorkTree refuses to push when the sandbox has uncommitted
// changes — staged, unstaged, or untracked. The agent is responsible
// for committing inside the sandbox before exiting; if it didn't, the
// loose edits would silently be left behind by the push and we'd ship
// a branch that doesn't reflect the agent's actual work. `workflow` is
// threaded in only to render a runnable command in the error message;
// the caller (cli/push.go) already has it on md.Workflow.
//
// Bind-mount character devices (e.g. fly.io / sandbox runtimes that
// shadow host config files like `.bashrc` / `.gitconfig` with
// `/dev/null`) are filtered out: git status reports them as untracked
// because the basename exists on disk, but they aren't agent edits —
// the agent can't even remove them ("Device or resource busy") — and
// refusing the push on their account would block every ship under
// the new plain-clone primitive that places the clone in the runtime's
// bind-mount scope.
func CheckCleanWorkTree(clonePath, workflow string) error {
	entries, err := git.Status(clonePath)
	if err != nil {
		return fmt.Errorf("push: git status in sandbox: %w", err)
	}
	entries = filterSandboxBindMounts(clonePath, entries)
	if len(entries) == 0 {
		return nil
	}
	return fmt.Errorf(`push: sandbox clone has %d uncommitted file(s) — the agent edited but did not commit
       sandbox: %s
       re-run `+"`moe %s code`"+` and ask the agent to commit, or commit manually in the sandbox`, len(entries), clonePath, workflow)
}

// filterSandboxBindMounts removes status entries whose on-disk shape
// is a character device — the sandbox runtime's bind-mounted /dev/null
// stand-ins for host config files. A real agent edit is always a
// regular file (or a deleted regular file, which `os.Stat` reports as
// missing — the stat-error branch keeps those in the slice). A
// missing-or-error stat keeps the entry: better to refuse the push on
// an ambiguous record than silently let an agent edit slip through.
func filterSandboxBindMounts(clonePath string, entries []git.StatusEntry) []git.StatusEntry {
	out := entries[:0]
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(clonePath, e.Path))
		if err == nil && info.Mode()&os.ModeDevice != 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

// RebaseInProgress reports whether the sandbox clone is sitting
// mid-rebase — a `.git/rebase-merge` or `.git/rebase-apply` directory
// left by a rebase that stopped (conflict) and was never finished or
// aborted. MoE's own rebase (EnsureRebasedOntoDefault) always aborts on
// conflict, so the only thing that leaves this state is a recovery turn
// whose agent resolved the conflict but couldn't finalize
// `git rebase --continue` — the wedge codex-rebase-weirdness fixes. The
// resolution is staged-but-uncommitted and can't be stashed out of, so
// CheckCleanWorkTree would misreport it as "agent edited but did not
// commit." Probed deterministically here (Go, outside the agent
// sandbox) so the push gate fails loud with the right manual commands
// instead. The clone is always a plain clone, so `.git` is a real
// directory; a missing-or-error stat reads as "not mid-rebase" —
// false negatives fall through to the existing clean-tree check.
func RebaseInProgress(clonePath string) bool {
	gitDir := filepath.Join(clonePath, ".git")
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		if info, err := os.Stat(filepath.Join(gitDir, name)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// CheckBranchHasCommits confirms the sandbox clone has `branch` and
// that it's ahead of `base`. A branch at zero commits-ahead means the
// agent didn't actually commit anything. `workflow` is the run's
// workflow name, used to render a runnable command in the error.
func CheckBranchHasCommits(clonePath, branch, base, workflow string) error {
	if !git.HasRef(clonePath, "refs/heads/"+branch) {
		return fmt.Errorf("push: branch %q does not exist in sandbox clone; run `moe %s code` and have the agent commit", branch, workflow)
	}
	// If AheadOf can't compute a count — an unknown base ref (a sandbox
	// clone of a detached submodule has no local `main`), or any other
	// rev-list failure — deliberately skip this guard. Blocking a push
	// on a count we couldn't compute is the wrong-refusal bug; the push
	// itself surfaces any real error a moment later.
	n, err := git.AheadOf(clonePath, base, branch)
	if err != nil {
		return nil
	}
	if n == 0 {
		return fmt.Errorf("push: branch %q has no commits ahead of %q; nothing to push", branch, base)
	}
	return nil
}

// EnsureOrigin makes sure origin in the sandbox clone points at
// `remote`. Fresh clones have origin pointing at the local submodule
// path (the clone source), which cannot be pushed to GitHub.
func EnsureOrigin(clonePath, remote string) error {
	out, err := git.Output(clonePath, "remote", "get-url", "origin")
	if err != nil {
		if combined, err := git.Combined(clonePath, "remote", "add", "origin", remote); err != nil {
			return fmt.Errorf("push: add origin: %w (%s)", err, combined)
		}
		return nil
	}
	current := strings.TrimSpace(out)
	if current == remote {
		return nil
	}
	if combined, err := git.Combined(clonePath, "remote", "set-url", "origin", remote); err != nil {
		return fmt.Errorf("push: set-url origin: %w (%s)", err, combined)
	}
	return nil
}

// GHRepoSpec derives the owner/repo spec that `gh --repo` wants from a
// project's remote URL. Accepts HTTPS
// (https://github.com/owner/repo[.git]) and SSH
// (git@github.com:owner/repo[.git]) forms.
func GHRepoSpec(remote string) (string, error) {
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

// FindOpenPR returns (url, exists, err) for an open PR on `branch` in
// `repo`. Uses `gh pr list` rather than `gh pr view` because list
// returns an empty array on no-match (exit 0) while view exits non-zero
// in the same case, and distinguishing "no PR" from "gh failed" matters.
func FindOpenPR(repo, branch string) (string, bool, error) {
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

// CreatePR shells out to `gh pr create` and returns the URL printed on
// stdout. Errors from gh (auth, permissions, repo not found) propagate
// with their stderr attached.
func CreatePR(repo, head, base, title, bodyFile string, stderr io.Writer) (string, error) {
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
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line, nil
		}
	}
	return "", fmt.Errorf("push: gh pr create succeeded but printed no URL: %q", out.String())
}

// MergedSHA returns the merge SHA recorded in the most recent
// MoE-Merged trailer for the run, or "" if none has been recorded.
func MergedSHA(root, projectID, runID string) string {
	return TrailerValue(root, projectID, runID, "MoE-Merged")
}

// TrailerValue pulls the value from the most recent `<trailer>:` line
// in any commit scoped to the run identified by (projectID, runID).
// Returns "" when no such commit exists.
//
// This is the branch-deleting path — reconcileOnePushedRun reads MoE-PR
// then MoE-Merged through here, and a wrong read can flip a still-open
// run to merged and delete its remote branch. So it is doubly guarded:
// the greps are anchored and project-scoped (a prefix-extending sibling
// like `auth-2`, or the same slug in another project, can't match), and
// because the bodies are already in hand each record is re-parsed and
// its exact MoE-Run / MoE-Project trailers checked before the requested
// value is extracted, which also rejects a commit that merely quotes
// another run's trailer line.
func TrailerValue(root, projectID, runID, trailer string) string {
	out, err := git.Output(root, "log",
		"--all-match",
		"--grep", trailers.GrepPattern("MoE-Run", runID),
		"--grep", trailers.GrepPattern("MoE-Project", projectID),
		"--grep", trailer+":",
		"--format=%B", "-z",
	)
	if err != nil {
		return ""
	}
	prefix := trailer + ":"
	for _, body := range strings.Split(out, "\x00") {
		// Re-verify identity from the body before trusting the record —
		// grep anchoring narrows the candidate set, this makes the match
		// exact.
		var gotRun, gotProject, value string
		haveValue := false
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok && gotRun == "" {
				gotRun = strings.TrimSpace(v)
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Project:"); ok && gotProject == "" {
				gotProject = strings.TrimSpace(v)
				continue
			}
			if !haveValue && strings.HasPrefix(line, prefix) {
				value = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				haveValue = true
			}
		}
		if gotRun == runID && gotProject == projectID && haveValue {
			return value
		}
	}
	return ""
}

// unmergedPaths reports the files git left in a conflicted (UU/AA/...)
// state — the same set `git status -s` would show with U/A markers.
// Used to name conflicting paths in the chain-back kickoff after the
// rebase has been --abort'd.
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
