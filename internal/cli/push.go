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
	"github.com/modulecollective/moe/internal/sandbox"
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
		moePrintln(stderr, "usage: moe workflow <wf> push [--pr] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Default: push moe/<run>, fast-forward-merge it into the target repo's")
		moePrintln(stderr, "default branch, delete the remote branch, and remove the sandbox clone.")
		moePrintln(stderr, "--pr: push moe/<run> and open (or re-use) a PR; leave the sandbox in place.")
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
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

	if err := pushBranch(clonePath, branch, pj.Remote, stdout, stderr); err != nil {
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
// progress stay visible.
func pushBranch(clonePath, branch, remote string, stdout, stderr io.Writer) error {
	moePrintf(stdout, "pushing %s to %s...\n", branch, remote)
	cmd := exec.Command("git", "-C", clonePath, "push", "-u", "origin", branch)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: git push: %w", err)
	}
	return nil
}

// openPRPath is the --pr behavior: open (or re-use) a PR for the
// already-pushed branch and record the first push's state. The
// sandbox is intentionally left in place — iteration via
// `moe workflow <wf> code` stays a one-liner until the PR merges.
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
		moePrintf(stderr, "       default may have moved past the merge-base — rebase inside the sandbox or retry with --pr\n")
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
		if err := sandbox.Remove(root, md.Project, md.ID); err != nil {
			moePrintf(stderr, "warning: remove sandbox: %v\n", err)
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
			return fmt.Errorf("push: code document not written yet; run `moe workflow %s code %s %s` first", md.Workflow, md.Project, md.ID)
		}
		return fmt.Errorf("push: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("push: code document is empty; run `moe workflow %s code %s %s` and produce a PR body first", md.Workflow, md.Project, md.ID)
	}
	return nil
}

func sandboxClonePath(root string, md *run.Metadata) (string, error) {
	if !sandbox.Exists(root, md.Project, md.ID) {
		return "", fmt.Errorf("push: no sandbox clone for %s/%s; run `moe workflow %s code %s %s` first", md.Project, md.ID, md.Workflow, md.Project, md.ID)
	}
	return sandbox.Ensure(root, md.Project, md.ID)
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
       re-run `+"`moe workflow <wf> code`"+` and ask the agent to commit, or commit manually in the sandbox`, len(entries), clonePath)
}

// checkBranchHasCommits confirms the sandbox clone has branch `branch`
// and that it's ahead of `base`. A branch at zero commits-ahead means the
// agent didn't actually commit anything.
func checkBranchHasCommits(clonePath, branch, base string) error {
	// First, does the branch exist?
	cmd := exec.Command("git", "-C", clonePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: branch %q does not exist in sandbox clone; run `moe workflow <wf> code` and have the agent commit", branch)
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
