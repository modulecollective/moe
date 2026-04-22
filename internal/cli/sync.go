package cli

import (
	"bufio"
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

	// --ff-only so a divergence surfaces instead of silently rebasing.
	// Skipped on a brand-new branch with no upstream — nothing to pull from.
	if hasUpstream(root) {
		pull := exec.Command("git", "pull", "--ff-only", "--recurse-submodules")
		pull.Dir = root
		pull.Stdout = stdout
		pull.Stderr = stderr
		if err := pull.Run(); err != nil {
			return 1
		}
	}

	// Walk each project submodule, fast-forward its tracking branch from
	// origin, and bump the gitlink in bureaucracy if the submodule moved.
	// Done after the pull so we're working from the latest bureaucracy state,
	// and before the push so the bump goes out in the same round trip.
	if err := bumpProjectPointers(root, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
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
		// git already printed the details; just propagate non-zero.
		return 1
	}
	return 0
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

// gitmoduleEntry mirrors the one in internal/managed/submodules.go but
// adds `branch`, which sync needs as a fallback when project.json is
// unavailable. Kept local to cli/sync.go so managed's INI parser stays
// focused on its own concerns.
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
// for submodule at subPath. Mirrors managed.pinnedSHA; kept local to
// keep sync's dependencies tight.
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
