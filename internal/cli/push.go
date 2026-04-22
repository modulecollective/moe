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
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/sandbox"
)

var pushCmd = &Command{
	Name:    "push",
	Summary: "push the request's code branch and open (or update) a PR on the target repo",
	Run:     runPush,
}

func init() {
	Register(pushCmd)
}

const branchPrefix = "moe/"

// runPush pushes the request's sandbox branch to the target repo and, on
// first ship, opens a PR with the `code` document as its body. Safely
// re-runnable: a rerun after more `moe sdlc code` turns pushes the new
// commits to the same branch and prints the existing PR URL. The sandbox
// clone is deliberately NOT removed — iteration via `moe sdlc code` stays
// a one-liner.
func runPush(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe push <project> <request>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Pushes moe/<request> from the sandbox clone to the target repo and")
		moePrintln(stderr, "opens a PR if one isn't already open. Re-run after more `moe sdlc code`")
		moePrintln(stderr, "turns to update the branch; the sandbox stays in place.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, reqID := fs.Arg(0), fs.Arg(1)

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
	pj, err := project.Load(root, md.Project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if err := checkCodeContent(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := checkStaleness(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	clonePath, err := sandboxClonePath(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	branch := branchPrefix + md.ID
	if err := checkBranchHasCommits(clonePath, branch, pj.DefaultBranch); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if err := ensureOrigin(clonePath, pj.Remote); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	moePrintf(stdout, "pushing %s to %s...\n", branch, pj.Remote)
	if err := gitPush(clonePath, branch, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

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
		bodyPath := filepath.Join(root, request.ContentPath(md.Project, md.ID, "code"))
		url, err = createPR(ghRepo, branch, pj.DefaultBranch, md.Title, bodyPath, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		moePrintf(stdout, "opened PR: %s\n", url)
	}

	// Only the first push flips status and records the MoE-PR trailer.
	// Re-runs just pushed branch updates to an already-recorded PR.
	if md.Status != request.StatusPushed {
		md.Status = request.StatusPushed
		if err := request.Save(root, md); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		reqJSON := filepath.Join(request.RunDir(md.Project, md.ID), "request.json")
		msg := fmt.Sprintf(`push: %s/%s

MoE-Request: %s
MoE-Project: %s
MoE-PR: %s
`, md.Project, md.ID, md.ID, md.Project, url)
		if err := request.StageAndCommit(root, msg, reqJSON); err != nil {
			moePrintf(stderr, "commit push record: %v\n", err)
			return 1
		}
	}
	return 0
}

func checkCodeContent(root string, md *request.Metadata) error {
	path := filepath.Join(root, request.ContentPath(md.Project, md.ID, "code"))
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("push: code document not written yet; run `moe sdlc code %s %s` first", md.Project, md.ID)
		}
		return fmt.Errorf("push: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("push: code document is empty; run `moe sdlc code %s %s` and produce a PR body first", md.Project, md.ID)
	}
	return nil
}

// checkStaleness is the hard gate that signing's cascade used to provide:
// if any prereq of the code document was touched after the last code
// turn, refuse to ship until the operator re-runs `moe sdlc code` to
// reconcile. Prereqs come from the request's workflow so new stages
// slot in without editing this gate.
func checkStaleness(root string, md *request.Metadata) error {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return err
	}
	_, codeWhen, err := request.LatestWorkTurnSHA(root, md.ID, "code")
	if err != nil {
		return err
	}
	for _, dep := range wf.Prereqs("code") {
		_, depWhen, err := request.LatestWorkTurnSHA(root, md.ID, dep)
		if err != nil {
			return err
		}
		if !depWhen.IsZero() && depWhen.After(codeWhen) {
			return fmt.Errorf("push: %s has changed since your last `moe sdlc code` turn — run `moe sdlc code %s %s` to reconcile, then retry", dep, md.Project, md.ID)
		}
	}
	return nil
}

func sandboxClonePath(root string, md *request.Metadata) (string, error) {
	if !sandbox.Exists(root, md.Project, md.ID) {
		return "", fmt.Errorf("push: no sandbox clone for %s/%s; run `moe sdlc code %s %s` first", md.Project, md.ID, md.Project, md.ID)
	}
	return sandbox.Ensure(root, md.Project, md.ID)
}

// checkBranchHasCommits confirms the sandbox clone has branch `branch`
// and that it's ahead of `base`. A branch at zero commits-ahead means the
// agent didn't actually commit anything.
func checkBranchHasCommits(clonePath, branch, base string) error {
	// First, does the branch exist?
	cmd := exec.Command("git", "-C", clonePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: branch %q does not exist in sandbox clone; run `moe sdlc code` and have the agent commit", branch)
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

// gitPush runs `git push -u origin <branch>` in the clone, streaming
// stdout/stderr so the operator sees progress and any credential prompts.
func gitPush(clonePath, branch string, stdout, stderr io.Writer) error {
	cmd := exec.Command("git", "-C", clonePath, "push", "-u", "origin", branch)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push: git push: %w", err)
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
