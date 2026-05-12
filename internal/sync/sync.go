// Package sync implements the bureaucracy-side primitives behind
// `moe sync`: walking .gitmodules, fast-forwarding each project
// submodule's tracking branch from its origin, computing the gitlink
// bumps that need to be staged, and querying GitHub for the state of
// open PRs so pushed runs can be reconciled to merged/closed.
//
// The cli/sync.go entry-point handler keeps the bureaucracy-side
// orchestration (repolock around the pull → bump → reconcile → push
// pipeline; enterTerminal-driven status flips on PR transitions).
// Functions here are the operations layer those steps delegate to:
// pure side-effecting work scoped to a clone path or a remote URL,
// so a non-CLI caller can compose them without going through
// `cli.Run`.
package sync

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

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/git"
)

// GitmoduleEntry is the parsed shape of one [submodule "..."] stanza
// in a .gitmodules file. Branch is a fallback for sync when
// project.json is unavailable.
type GitmoduleEntry struct {
	Name   string
	Path   string
	URL    string
	Branch string
}

// Bump records one submodule whose gitlink bureaucracy is about to
// advance. Path is repo-relative; FromSHA / ToSHA bound the move.
type Bump struct {
	Path    string
	FromSHA string
	ToSHA   string
}

// PRState is the subset of `gh pr view --json state,mergeCommit`
// output that reconciliation cares about. State is "OPEN", "MERGED",
// or "CLOSED" (case as gh returns it).
type PRState struct {
	State       string `json:"state"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

// HasUpstream reports whether the branch checked out in dir has an
// upstream configured. False on a brand-new branch with no @{u}, true
// otherwise. Thin wrapper around git.Upstream that swallows the error
// — any failure here means "no upstream" by convention.
func HasUpstream(dir string) bool {
	u, _ := git.Upstream(dir)
	return u != ""
}

// ParseGitmodules reads .gitmodules at path and returns one entry per
// [submodule "..."] stanza. A missing file returns (nil, nil).
func ParseGitmodules(path string) ([]GitmoduleEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("moe sync: open %s: %w", path, err)
	}
	defer f.Close()

	var entries []GitmoduleEntry
	var cur *GitmoduleEntry
	flush := func() {
		if cur != nil && cur.Path != "" && cur.URL != "" {
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
				cur = &GitmoduleEntry{Name: name}
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
			cur.Path = strings.TrimSpace(val)
		case "url":
			cur.URL = strings.TrimSpace(val)
		case "branch":
			cur.Branch = strings.TrimSpace(val)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("moe sync: scan %s: %w", path, err)
	}
	return entries, nil
}

// ProjectIDForSubmodulePath extracts "<id>" from "projects/<id>/src".
// Returns "" if the path doesn't match the convention.
func ProjectIDForSubmodulePath(subPath string) string {
	parts := strings.Split(filepath.ToSlash(subPath), "/")
	if len(parts) == 3 && parts[0] == "projects" && parts[2] == "src" {
		return parts[1]
	}
	return ""
}

// resolveTrackingBranch picks the branch to advance the submodule to.
// Precedence: project.json default_branch → .gitmodules branch key →
// "main". project.json wins because it was resolved from the remote's
// symbolic HEAD at registration time and is the most authoritative
// answer.
func resolveTrackingBranch(root string, e GitmoduleEntry) string {
	if id := ProjectIDForSubmodulePath(e.Path); id != "" {
		if b := readProjectDefaultBranch(filepath.Join(root, "projects", id, "project.json")); b != "" {
			return b
		}
	}
	if e.Branch != "" {
		return e.Branch
	}
	return "main"
}

// readProjectDefaultBranch returns project.json's default_branch field
// or "" if unreadable / absent. Forgiving so sync never fails on a
// project.json hiccup — the .gitmodules fallback picks up the slack.
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

// HeadSHA is git rev-parse HEAD scoped to dir.
func HeadSHA(dir string) (string, error) {
	return git.RevParse(dir, "HEAD")
}

// GitlinkSHA reads the gitlink that bureaucracy's HEAD commit
// records for the submodule at subPath.
func GitlinkSHA(root, subPath string) (string, error) {
	out, err := git.Output(root, "ls-tree", "HEAD", subPath)
	if err != nil {
		return "", fmt.Errorf("git ls-tree HEAD %s: %w", subPath, err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", fmt.Errorf("no gitlink for %s", subPath)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "160000" {
		return "", fmt.Errorf("unexpected ls-tree output %q", line)
	}
	return fields[2], nil
}

// AdvanceSubmodule fetches origin, fast-forwards the tracking branch,
// and returns a Bump if the submodule's HEAD now differs from the
// gitlink bureaucracy has recorded. Returns (nil, nil) when already
// caught up. Refuses to touch a submodule with uncommitted changes
// or with local commits diverged from origin.
func AdvanceSubmodule(root string, e GitmoduleEntry, stdout, stderr io.Writer) (*Bump, error) {
	subAbs := filepath.Join(root, e.Path)
	if _, err := os.Stat(filepath.Join(subAbs, ".git")); err != nil {
		// Submodule not checked out. Skip — `git submodule update --init`
		// is the operator's move, not ours.
		return nil, nil
	}

	entries, err := git.Status(subAbs)
	if err != nil {
		return nil, fmt.Errorf("moe sync: inspect %s: %w", e.Path, err)
	}
	if len(entries) > 0 {
		var lines []string
		for _, en := range entries {
			lines = append(lines, fmt.Sprintf("%s %s", en.XY, en.Path))
		}
		return nil, fmt.Errorf(
			"moe sync: %s has uncommitted changes — refusing to sync.\n\n%s\n\nRecovery:\n  cd %s\n  git status              # see what's there\n  git stash               # or commit, or restore\n  cd -\n  moe sync                # retry",
			e.Path, strings.Join(lines, "\n"), e.Path,
		)
	}

	branch := resolveTrackingBranch(root, e)

	cliout.Printf(stdout, "moe sync: fetching %s\n", e.Path)
	if out, err := git.Combined(subAbs, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("moe sync: fetch %s: %w (%s)", e.Path, err, out)
	}

	if out, err := git.Combined(subAbs, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err != nil {
		return nil, fmt.Errorf("moe sync: %s has no origin/%s (%s)", e.Path, branch, strings.TrimSpace(out))
	}

	// Leave detached HEAD — common after `git submodule update` —
	// behind by checking out the branch. A no-op if already on it.
	if out, err := git.Combined(subAbs, "checkout", branch); err != nil {
		return nil, fmt.Errorf("moe sync: checkout %s in %s: %w (%s)", branch, e.Path, err, out)
	}

	localSHA, err := HeadSHA(subAbs)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of %s: %w", e.Path, err)
	}
	remoteSHA, err := git.RevParse(subAbs, "refs/remotes/origin/"+branch)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of origin/%s in %s: %w", branch, e.Path, err)
	}
	if localSHA != remoteSHA {
		if out, err := git.Combined(subAbs, "merge-base", "--is-ancestor", localSHA, remoteSHA); err != nil {
			_ = out
			return nil, fmt.Errorf(
				"moe sync: %s %s has diverged from origin — refusing to sync.\n\nRecovery:\n  git -C %s log origin/%s..HEAD   # see local-only commits\n  # decide whether to push, reset, or stash, then retry moe sync",
				e.Path, branch, e.Path, branch,
			)
		}
		if out, err := git.Combined(subAbs, "merge", "--ff-only", "refs/remotes/origin/"+branch); err != nil {
			return nil, fmt.Errorf("moe sync: ff %s in %s: %w (%s)", branch, e.Path, err, out)
		}
	}

	newHead, err := HeadSHA(subAbs)
	if err != nil {
		return nil, fmt.Errorf("moe sync: head of %s: %w", e.Path, err)
	}
	linkedSHA, err := GitlinkSHA(root, e.Path)
	if err != nil {
		return nil, fmt.Errorf("moe sync: gitlink of %s: %w", e.Path, err)
	}
	if newHead == linkedSHA {
		return nil, nil
	}
	return &Bump{Path: e.Path, FromSHA: linkedSHA, ToSHA: newHead}, nil
}

// BumpProjectPointers walks every [submodule] in .gitmodules, brings
// its tracking branch up to origin, and stages the gitlink update in
// bureaucracy when the submodule moved. If anything was staged,
// commits it with a message listing what advanced. Aborts on the
// first failure without committing or mutating further submodules.
func BumpProjectPointers(root string, stdout, stderr io.Writer) error {
	entries, err := ParseGitmodules(filepath.Join(root, ".gitmodules"))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	var bumps []Bump
	for _, e := range entries {
		bump, err := AdvanceSubmodule(root, e, stdout, stderr)
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

	// Stage every bumped submodule, then commit once. Stage here
	// rather than inside the per-submodule loop so a late abort can't
	// leave bureaucracy with a half-applied index.
	paths := make([]string, 0, len(bumps))
	for _, b := range bumps {
		if out, err := git.Combined(root, "add", b.Path); err != nil {
			return fmt.Errorf("moe sync: git add %s: %w (%s)", b.Path, err, out)
		}
		paths = append(paths, b.Path)
	}
	// Scope the commit to just the gitlink paths so any unrelated
	// changes the operator already had staged don't get swept into a
	// "sync: bump project pointers" commit by accident.
	commitArgs := append([]string{"commit", "-m", PointerBumpCommitMessage(bumps), "--"}, paths...)
	if out, err := git.Combined(root, commitArgs...); err != nil {
		return fmt.Errorf("moe sync: git commit: %w (%s)", err, out)
	}
	cliout.Printf(stdout, "moe sync: bumped %d project pointer(s)\n", len(bumps))
	return nil
}

// PointerBumpCommitMessage formats a bump set as a sync commit body.
// Format:
//
//	sync: bump project pointers
//
//	moe: 4562047..d077102
//	…
func PointerBumpCommitMessage(bumps []Bump) string {
	sort.Slice(bumps, func(i, j int) bool { return bumps[i].Path < bumps[j].Path })
	var sb strings.Builder
	sb.WriteString("sync: bump project pointers\n\n")
	for _, b := range bumps {
		id := ProjectIDForSubmodulePath(b.Path)
		if id == "" {
			id = b.Path
		}
		fmt.Fprintf(&sb, "%s: %s..%s\n", id, git.ShortSHA(b.FromSHA), git.ShortSHA(b.ToSHA))
	}
	return sb.String()
}

// PRStateOf shells out to `gh pr view <url> --json state,mergeCommit`
// and decodes the response.
func PRStateOf(prURL string) (*PRState, error) {
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
	var s PRState
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		return nil, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return &s, nil
}

// DeleteRemoteBranch asks GitHub to drop refs/heads/<branch> from
// repo via `gh api DELETE`. A 422 with "Reference does not exist" is
// treated as success (someone — auto-delete on merge, an earlier
// reconcile — already removed it).
func DeleteRemoteBranch(repo, branch string) error {
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
		if strings.Contains(msg, "Reference does not exist") {
			return nil
		}
		return fmt.Errorf("delete remote %s: %w (%s)", branch, err, msg)
	}
	return nil
}
