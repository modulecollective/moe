package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

func init() {
	Register(&Command{
		Name:    "sync",
		Summary: "sync the bureaucracy repo with origin (git pull --ff-only, bump project pointers, then push)",
		Run:     runSync,
	})
}

func runSync(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe sync")
		return 2
	}
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

	// sync is one logical bureaucracy mutation (pull → pointer bumps →
	// reconcile pushed runs → push). Hold the repo lock for the whole
	// sequence so two syncs don't clobber each other mid-flight.
	// Heartbeat is on because sync can sit on the network for a while.
	err = withRepoLock(root, repolock.Options{
		Purpose:   "sync",
		Budget:    repolock.CronBudget,
		Heartbeat: true,
	}, func() error {
		return doSync(root, stdout, stderr)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// doSync runs the sync pipeline under an already-held repo lock.
func doSync(root string, stdout, stderr io.Writer) error {
	// --ff-only so a divergence surfaces instead of silently rebasing.
	// Skipped on a brand-new branch with no upstream — nothing to pull from.
	if hasUpstream(root) {
		pull := exec.Command("git", "pull", "--ff-only", "--recurse-submodules")
		pull.Dir = root
		pull.Stdout = stdout
		pull.Stderr = stderr
		if err := pull.Run(); err != nil {
			return fmt.Errorf("git pull: %w", err)
		}
	}

	// Walk each project submodule, fast-forward its tracking branch from
	// origin, and bump the gitlink in bureaucracy if the submodule moved.
	// Done after the pull so we're working from the latest bureaucracy state,
	// and before the push so the bump goes out in the same round trip.
	if err := bumpProjectPointers(root, stdout, stderr); err != nil {
		return err
	}

	// Reconcile any pushed runs: if GitHub says the PR merged or
	// closed, flip the run's status and clean up the branch + sandbox
	// so the end state matches the direct-merge path.
	if err := reconcilePushedRuns(root, stdout, stderr); err != nil {
		return err
	}

	// If the current branch has no upstream configured, push with -u so the
	// first push sets one. After that, plain `git push` is correct and keeps
	// whatever upstream the operator chose.
	pushArgs := []string{"push", "--recurse-submodules=on-demand"}
	if !hasUpstream(root) {
		pushArgs = []string{"push", "--recurse-submodules=on-demand", "-u", "origin", "HEAD"}
	}
	cmd := exec.Command("git", pushArgs...)
	cmd.Dir = root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// git already printed the details; wrap in a bare error.
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

func hasUpstream(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// pointerBump records one submodule whose gitlink we're about to advance.
type pointerBump struct {
	path    string // submodule path relative to bureaucracy root, e.g. projects/moe/src
	fromSHA string
	toSHA   string
}

// bumpProjectPointers walks every [submodule] in .gitmodules, brings its
// tracking branch up to origin, and stages the gitlink update in
// bureaucracy when the submodule moved. If anything was staged, commits
// it with a message listing what advanced. Aborts on the first failure
// without committing or mutating further submodules.
func bumpProjectPointers(root string, stdout, stderr io.Writer) error {
	entries, err := readGitmoduleEntries(filepath.Join(root, ".gitmodules"))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	var bumps []pointerBump
	for _, e := range entries {
		bump, err := advanceSubmodule(root, e, stdout, stderr)
		if err != nil {
			return err
		}
		if bump != nil {
			bumps = append(bumps, *bump)
		}
	}
	if len(bumps) == 0 {
		return nil
	}

	// Stage every bumped submodule, then commit once. Stage here rather
	// than inside the per-submodule loop so a late abort can't leave
	// bureaucracy with a half-applied index.
	paths := make([]string, 0, len(bumps))
	for _, b := range bumps {
		if out, err := runGitCaptured(root, "add", b.path); err != nil {
			return fmt.Errorf("moe sync: git add %s: %w (%s)", b.path, err, out)
		}
		paths = append(paths, b.path)
	}
	// Scope the commit to just the gitlink paths so any unrelated
	// changes the operator already had staged don't get swept into a
	// "sync: bump project pointers" commit by accident.
	commitArgs := append([]string{"commit", "-m", pointerBumpCommitMessage(bumps), "--"}, paths...)
	if out, err := runGitCaptured(root, commitArgs...); err != nil {
		return fmt.Errorf("moe sync: git commit: %w (%s)", err, out)
	}
	moePrintf(stdout, "moe sync: bumped %d project pointer(s)\n", len(bumps))
	return nil
}

// advanceSubmodule fetches origin, fast-forwards the tracking branch,
// and returns a pointerBump if the submodule's HEAD now differs from
// the gitlink bureaucracy has recorded. Returns (nil, nil) when already
// caught up.
func advanceSubmodule(root string, e gitmoduleEntry, stdout, stderr io.Writer) (*pointerBump, error) {
	subAbs := filepath.Join(root, e.path)
	if _, err := os.Stat(filepath.Join(subAbs, ".git")); err != nil {
		// Submodule not checked out. Skip — `git submodule update --init`
		// is the operator's move, not ours.
		return nil, nil
	}

	if dirty, status, err := submoduleDirty(subAbs); err != nil {
		return nil, fmt.Errorf("moe sync: inspect %s: %w", e.path, err)
	} else if dirty {
		return nil, fmt.Errorf(
			"moe sync: %s has uncommitted changes — refusing to sync.\n\n%s\nRecovery:\n  cd %s\n  git status              # see what's there\n  git stash               # or commit, or restore\n  cd -\n  moe sync                # retry",
			e.path, status, e.path,
		)
	}

	branch := resolveTrackingBranch(root, e)

	moePrintf(stdout, "moe sync: fetching %s\n", e.path)
	if out, err := runGitCaptured(subAbs, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("moe sync: fetch %s: %w (%s)", e.path, err, out)
	}

	// Ensure origin has the tracking branch at all — otherwise we'd confuse
	// "nothing to merge" with "wrong branch name".
	if out, err := runGitCaptured(subAbs, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err != nil {
		return nil, fmt.Errorf("moe sync: %s has no origin/%s (%s)", e.path, branch, strings.TrimSpace(out))
	}

	// Leave detached HEAD — common after `git submodule update` — behind
	// by checking out the branch. A no-op if already on it.
	if out, err := runGitCaptured(subAbs, "checkout", branch); err != nil {
		return nil, fmt.Errorf("moe sync: checkout %s in %s: %w (%s)", branch, e.path, err, out)
	}

	localSHA, err := gitHeadSHA(subAbs)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of %s: %w", e.path, err)
	}
	remoteSHA, err := gitRevParse(subAbs, "refs/remotes/origin/"+branch)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of origin/%s in %s: %w", branch, e.path, err)
	}
	if localSHA != remoteSHA {
		// If we're already an ancestor of origin, a fast-forward will work.
		// Otherwise we've diverged — bail and point the operator at the
		// commits that are local-only.
		if out, err := runGitCaptured(subAbs, "merge-base", "--is-ancestor", localSHA, remoteSHA); err != nil {
			_ = out
			return nil, fmt.Errorf(
				"moe sync: %s/%s has diverged from origin — refusing to sync.\n\nRecovery:\n  git -C %s log origin/%s..HEAD   # see local-only commits\n  # decide whether to push, reset, or stash, then retry moe sync",
				e.path, branch, e.path, branch,
			)
		}
		if out, err := runGitCaptured(subAbs, "merge", "--ff-only", "refs/remotes/origin/"+branch); err != nil {
			return nil, fmt.Errorf("moe sync: ff %s in %s: %w (%s)", branch, e.path, err, out)
		}
	}

	newHead, err := gitHeadSHA(subAbs)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of %s: %w", e.path, err)
	}
	linkedSHA, err := gitlinkSHA(root, e.path)
	if err != nil {
		return nil, fmt.Errorf("moe sync: gitlink of %s: %w", e.path, err)
	}
	if newHead == linkedSHA {
		return nil, nil
	}
	return &pointerBump{path: e.path, fromSHA: linkedSHA, toSHA: newHead}, nil
}

// submoduleDirty reports whether the submodule working tree has any
// uncommitted changes (modified, staged, or untracked). The second
// return is the raw `git status --porcelain` output, useful to echo
// verbatim in the abort message.
func submoduleDirty(subAbs string) (bool, string, error) {
	out, err := exec.Command("git", "-C", subAbs, "status", "--porcelain").Output()
	if err != nil {
		return false, "", fmt.Errorf("git status: %w", err)
	}
	s := string(out)
	return strings.TrimSpace(s) != "", s, nil
}

// resolveTrackingBranch picks the branch we'll advance the submodule
// to. Precedence: project.json default_branch → .gitmodules branch key
// → "main". Using project.json first because it was resolved from the
// remote's symbolic HEAD at registration time and is the most
// authoritative answer.
func resolveTrackingBranch(root string, e gitmoduleEntry) string {
	if id := projectIDForSubmodulePath(e.path); id != "" {
		if b := readProjectDefaultBranch(filepath.Join(root, "projects", id, "project.json")); b != "" {
			return b
		}
	}
	if e.branch != "" {
		return e.branch
	}
	return "main"
}

// projectIDForSubmodulePath extracts "<id>" from "projects/<id>/src".
// Returns "" if the path doesn't match the convention.
func projectIDForSubmodulePath(subPath string) string {
	parts := strings.Split(filepath.ToSlash(subPath), "/")
	if len(parts) == 3 && parts[0] == "projects" && parts[2] == "src" {
		return parts[1]
	}
	return ""
}

// readProjectDefaultBranch returns project.json's default_branch field,
// or "" if the file is missing / unreadable / has no value. Kept
// forgiving so sync never fails on a project.json hiccup — the
// .gitmodules fallback picks up the slack.
func readProjectDefaultBranch(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var md struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(b, &md); err != nil {
		return ""
	}
	return md.DefaultBranch
}

// pointerBumpCommitMessage is:
//
//	sync: bump project pointers
//
//	moe: 4562047..d077102
//	…
func pointerBumpCommitMessage(bumps []pointerBump) string {
	sort.Slice(bumps, func(i, j int) bool { return bumps[i].path < bumps[j].path })
	var sb strings.Builder
	sb.WriteString("sync: bump project pointers\n\n")
	for _, b := range bumps {
		id := projectIDForSubmodulePath(b.path)
		if id == "" {
			id = b.path
		}
		fmt.Fprintf(&sb, "%s: %s..%s\n", id, shortSHA(b.fromSHA), shortSHA(b.toSHA))
	}
	return sb.String()
}

func shortSHA(s string) string {
	if len(s) < 7 {
		return s
	}
	return s[:7]
}

// gitmoduleEntry is the parsed shape of one [submodule "..."] stanza in
// a .gitmodules file. branch is a fallback for sync when project.json
// is unavailable.
type gitmoduleEntry struct {
	name   string
	path   string
	url    string
	branch string
}

func readGitmoduleEntries(path string) ([]gitmoduleEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("moe sync: open %s: %w", path, err)
	}
	defer f.Close()

	var entries []gitmoduleEntry
	var cur *gitmoduleEntry
	flush := func() {
		if cur != nil && cur.path != "" && cur.url != "" {
			entries = append(entries, *cur)
		}
		cur = nil
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			header := strings.TrimSpace(line[1 : len(line)-1])
			if strings.HasPrefix(header, "submodule ") {
				name := strings.Trim(strings.TrimPrefix(header, "submodule "), "\"")
				cur = &gitmoduleEntry{name: name}
			}
			continue
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "path":
			cur.path = strings.TrimSpace(val)
		case "url":
			cur.url = strings.TrimSpace(val)
		case "branch":
			cur.branch = strings.TrimSpace(val)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("moe sync: scan %s: %w", path, err)
	}
	return entries, nil
}

func gitHeadSHA(dir string) (string, error) {
	return gitRevParse(dir, "HEAD")
}

func gitRevParse(dir, ref string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitlinkSHA reads the gitlink that bureaucracy's HEAD commit records
// for submodule at subPath.
func gitlinkSHA(root, subPath string) (string, error) {
	out, err := exec.Command("git", "-C", root, "ls-tree", "HEAD", subPath).Output()
	if err != nil {
		return "", fmt.Errorf("git ls-tree HEAD %s: %w", subPath, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("no gitlink for %s", subPath)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "160000" {
		return "", fmt.Errorf("unexpected ls-tree output %q", line)
	}
	return fields[2], nil
}

// runGitCaptured runs git in dir capturing stdout+stderr together, for
// cases where we want the error message in our own prose rather than
// streaming raw git output to the terminal.
func runGitCaptured(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// reconcilePushedRuns walks every run in StatusPushed, asks GitHub
// what state its PR is in, and — when the PR has been merged or
// closed — flips the run's status, tears down the branch and sandbox,
// and records a closing trailer. Open PRs are a silent no-op; sync
// prints exactly one line per transition and nothing for runs that
// didn't move.
func reconcilePushedRuns(root string, stdout, stderr io.Writer) error {
	mds, err := run.Scan(root)
	if err != nil {
		return fmt.Errorf("moe sync: scan runs: %w", err)
	}
	// Deterministic order so transition lines come out the same way
	// across invocations — helps when the operator is scanning output
	// and makes test assertions stable.
	sort.Slice(mds, func(i, j int) bool {
		if mds[i].Project != mds[j].Project {
			return mds[i].Project < mds[j].Project
		}
		return mds[i].ID < mds[j].ID
	})
	for _, md := range mds {
		if md.Status != run.StatusPushed {
			continue
		}
		if err := reconcileOnePushedRun(root, md, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// prViewState is the subset of `gh pr view --json state,mergeCommit`
// output that we care about. state is "OPEN", "MERGED", or "CLOSED".
type prViewState struct {
	State       string `json:"state"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

func reconcileOnePushedRun(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	prURL := trailerValue(root, md.ID, "MoE-PR")
	if prURL == "" {
		// No MoE-PR trailer on record despite StatusPushed. Flag and
		// skip rather than guess — the operator can untangle by hand.
		moePrintf(stderr, "moe sync: %s/%s is pushed but has no MoE-PR trailer; skipping\n", md.Project, md.ID)
		return nil
	}
	state, err := ghPRState(prURL)
	if err != nil {
		moePrintf(stderr, "moe sync: %s/%s: %v; skipping\n", md.Project, md.ID, err)
		return nil
	}
	switch strings.ToUpper(state.State) {
	case "OPEN":
		return nil
	case "MERGED":
		mergeSHA := state.MergeCommit.OID
		if mergeSHA == "" {
			moePrintf(stderr, "moe sync: %s/%s merged but gh returned no mergeCommit; skipping\n", md.Project, md.ID)
			return nil
		}
		if err := finalizePushedRun(root, md, run.StatusMerged, "MoE-Merged", mergeSHA, stderr); err != nil {
			return err
		}
		moePrintf(stdout, "%s: pushed -> merged (%s)\n", md.ID, shortSHA(mergeSHA))
	case "CLOSED":
		if err := finalizePushedRun(root, md, run.StatusClosed, "MoE-Closed", prURL, stderr); err != nil {
			return err
		}
		moePrintf(stdout, "%s: pushed -> closed\n", md.ID)
	default:
		moePrintf(stderr, "moe sync: %s/%s has unexpected PR state %q; skipping\n", md.Project, md.ID, state.State)
	}
	return nil
}

// ghPRState shells out to `gh pr view <url> --json state,mergeCommit`
// and decodes the response.
func ghPRState(prURL string) (*prViewState, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "state,mergeCommit")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("gh CLI not found on PATH; install https://cli.github.com/")
		}
		return nil, fmt.Errorf("gh pr view %s: %w (%s)", prURL, err, strings.TrimSpace(out.String()))
	}
	var s prViewState
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		return nil, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return &s, nil
}

// finalizePushedRun flips md.Status, deletes the remote branch and
// the sandbox clone, and commits run.json with the closing trailer.
// The cleanup mirrors the direct-merge path so the end state is
// indistinguishable regardless of how the run reached a terminal
// status. Branch/sandbox deletion failures are warned but non-fatal —
// the reconciliation has otherwise succeeded and a stray branch or
// clone is a cleanup nuisance, not a correctness bug.
func finalizePushedRun(root string, md *run.Metadata, status, trailer, value string, stderr io.Writer) error {
	md.Status = status
	if err := run.Save(root, md); err != nil {
		return fmt.Errorf("moe sync: save run.json for %s/%s: %w", md.Project, md.ID, err)
	}
	if err := deleteRemoteBranchForRun(root, md); err != nil {
		moePrintf(stderr, "warning: %s/%s: %v\n", md.Project, md.ID, err)
	}
	if err := sandbox.Remove(root, md.Project, md.ID); err != nil {
		moePrintf(stderr, "warning: %s/%s: remove sandbox: %v\n", md.Project, md.ID, err)
	}
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf(`sync: %s/%s %s

MoE-Run: %s
MoE-Project: %s
MoE-Document: push
%s: %s
`, md.Project, md.ID, strings.ToLower(status), md.ID, md.Project, trailer, value)
	if err := run.StageAndCommit(root, msg, runJSON); err != nil {
		return fmt.Errorf("moe sync: commit %s for %s/%s: %w", strings.ToLower(status), md.Project, md.ID, err)
	}
	return nil
}

// deleteRemoteBranchForRun asks GitHub to drop moe/<run> from the
// project's remote. Uses `gh api DELETE /repos/.../git/refs/heads/<branch>`
// so we don't need a working clone in hand — the submodule may be
// detached, mid-sync, or absent. A 422 on a branch GitHub already
// deleted (common after merge with auto-delete enabled) is treated
// as success.
func deleteRemoteBranchForRun(root string, md *run.Metadata) error {
	pj, err := project.Load(root, md.Project)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	repo, err := ghRepoSpec(pj.Remote)
	if err != nil {
		return err
	}
	branch := branchPrefix + md.ID
	cmd := exec.Command("gh", "api", "--method", "DELETE",
		"/repos/"+repo+"/git/refs/heads/"+branch,
		"--silent",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("gh CLI not found on PATH")
		}
		msg := strings.TrimSpace(out.String())
		// A 422 with "Reference does not exist" means someone (GitHub
		// auto-delete, an earlier merge) already removed it. No-op.
		if strings.Contains(msg, "Reference does not exist") {
			return nil
		}
		return fmt.Errorf("delete remote %s: %w (%s)", branch, err, msg)
	}
	return nil
}
