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
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
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
	// Cold submodule: materialize it before fetching. Without this,
	// sync silently no-ops on projects that no one has opened a stage
	// against yet, and the operator only finds out at first stage open.
	// project.EnsureMaterialized is the universal "ensure before touch"
	// gate (see internal/project/materialize.go); it short-circuits when
	// src is already populated.
	if id := ProjectIDForSubmodulePath(e.Path); id != "" {
		if err := project.EnsureMaterialized(root, id); err != nil {
			return nil, fmt.Errorf("moe sync: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(subAbs, ".git")); err != nil {
		// Not a projects/<id>/src layout (or materialize didn't take):
		// nothing to fast-forward.
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
	commitArgs := append([]string{"commit", "-m", pointerBumpCommitMessage(bumps), "--"}, paths...)
	if out, err := git.Combined(root, commitArgs...); err != nil {
		return fmt.Errorf("moe sync: git commit: %w (%s)", err, out)
	}
	cliout.Printf(stdout, "moe sync: bumped %d project pointer(s)\n", len(bumps))
	return nil
}

// BumpOne is the single-project variant of BumpProjectPointers: it
// brings projects/<projectID>/src up to its tracking branch on origin
// and, when the gitlink moved, commits a "sync: bump project pointers"
// record for just that project. Returns nil — without committing —
// when projectID is absent from .gitmodules or the bump is a no-op.
//
// Called from the push merge path so the gitlink advances in lockstep
// with the ff-push that just landed, without sweeping in unrelated
// submodules whose dirty or diverged state could shadow the bump for
// the project the operator actually shipped.
func BumpOne(root, projectID string, stdout, stderr io.Writer) error {
	entries, err := ParseGitmodules(filepath.Join(root, ".gitmodules"))
	if err != nil {
		return err
	}
	target := "projects/" + projectID + "/src"
	var entry *GitmoduleEntry
	for i := range entries {
		if entries[i].Path == target {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return nil
	}

	bump, err := AdvanceSubmodule(root, *entry, stdout, stderr)
	if err != nil {
		return err
	}
	if bump == nil {
		return nil
	}

	if out, err := git.Combined(root, "add", bump.Path); err != nil {
		return fmt.Errorf("moe sync: git add %s: %w (%s)", bump.Path, err, out)
	}
	commitArgs := []string{"commit", "-m", pointerBumpCommitMessage([]Bump{*bump}), "--", bump.Path}
	if out, err := git.Combined(root, commitArgs...); err != nil {
		return fmt.Errorf("moe sync: git commit: %w (%s)", err, out)
	}
	cliout.Printf(stdout, "moe sync: bumped %d project pointer(s)\n", 1)
	return nil
}

// pointerBumpCommitMessage formats a bump set as a sync commit body.
// Format:
//
//	sync: bump project pointers
//
//	moe: 4562047..d077102
//	…
func pointerBumpCommitMessage(bumps []Bump) string {
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

// RebaseInProgress reports whether a `git rebase` is paused in dir's
// worktree. True on either apply-style (`rebase-apply/`) or
// merge-style (`rebase-merge/`) rebases — `git pull --rebase` uses
// one or the other depending on backend. Goes through
// `git rev-parse --git-path` so it resolves correctly when .git is a
// file pointing into a shared gitdir (linked worktrees).
func RebaseInProgress(dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := git.Output(dir, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(out)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// RebaseRecoveryError builds the refuse-with-recovery message used
// when the bureaucracy worktree has a rebase paused — either because
// a just-run pull hit a conflict, or because a previous sync left
// one behind. Names any dirty/unmerged paths from `git status` so the
// operator knows exactly what to look at; resolution stays plain git.
func RebaseRecoveryError(root string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "moe sync: a git rebase is in progress in %s — refusing to sync.", root)
	if entries, err := git.Status(root); err == nil && len(entries) > 0 {
		sb.WriteString("\n")
		for _, e := range entries {
			fmt.Fprintf(&sb, "\n  %s %s", e.XY, e.Path)
		}
	}
	fmt.Fprintf(&sb, "\n\nRecovery:\n  cd %s\n  git status                # inspect conflicts\n  # resolve, then:\n  git rebase --continue     # or: git rebase --abort\n  moe sync                  # retry once the rebase is gone", root)
	return errors.New(sb.String())
}

// AutoPull is the session-open read-edge: pull bureaucracy's upstream
// onto local main before the operator's first edit lands on a stale
// canvas. Same pull command as `moe sync` (rebase + autostash, no
// submodule recursion — `BumpProjectPointers` is sync's job, not ours)
// but with a lighter failure policy. Rebase conflicts halt with the
// full recovery prose so the turn never starts on a half-rebased
// worktree; everything else (network, auth, dns) is best-effort —
// warn one line and return nil so the operator can keep working
// offline. The next clean auto-pull catches any divergence.
//
// No upstream configured is a silent no-op — brand-new branches with
// no @{u} are common during local-only setup.
func AutoPull(root string, stdout, stderr io.Writer) error {
	if RebaseInProgress(root) {
		return RebaseRecoveryError(root)
	}
	if !HasUpstream(root) {
		return nil
	}
	// -c advice.skippedCherryPicks=false: session-open auto-pull hits the
	// same patch-id dedup as `moe sync` when machine B opens a session
	// after machine A pushed a "sync: bump project pointers" commit — the
	// rebase drops the duplicate bump and git would otherwise print two
	// misleading --reapply-cherry-picks hints. Suppress the advice; keep
	// the bare warning. See doSync for the full rationale.
	if err := git.Stream(root, stdout, stderr, "-c", "advice.skippedCherryPicks=false", "pull", "--rebase", "--autostash", "--no-recurse-submodules"); err != nil {
		if RebaseInProgress(root) {
			return RebaseRecoveryError(root)
		}
		cliout.Printf(stderr, "[auto-sync skipped] git pull failed: %v — working offline\n", err)
		return nil
	}
	return nil
}

// AutoPush is the session-close write-edge: push the turn commit that
// just landed on local main so the other machine sees it before the
// operator switches over. Plain `git push` — no submodule recursion
// (auto-push never moves a gitlink, that's `moe sync`'s job) and no
// PR reconcile. Best-effort: a brand-new branch with no upstream is a
// silent no-op, and a network failure warns one line and returns nil
// so a turn can't fail just because origin is unreachable. The local
// commit is intact; the next session close retries.
func AutoPush(root string, stdout, stderr io.Writer) error {
	if !HasUpstream(root) {
		return nil
	}
	if err := git.Stream(root, stdout, stderr, "push"); err != nil {
		cliout.Printf(stderr, "[auto-sync skipped] git push failed: %v — working offline\n", err)
		return nil
	}
	return nil
}

// WithJournalPush is the write-edge for verbs that land commits on
// root main — direct journal commits and session-branch landings
// (`moe session resolve`) alike: take the repo lock, run fn, and on
// success race the commit to origin. Push failure never fails the verb
// (AutoPush warns and returns nil). Heartbeat is forced on because the
// lock now spans a network leg — without it, a slow push window would
// look stale to a contending acquirer. fn returning an error (including
// run.ErrNothingToCommit) skips the push and surfaces unchanged to
// callers that special-case it.
func WithJournalPush(root string, opts repolock.Options, stdout, stderr io.Writer, fn func() error) error {
	opts.Heartbeat = true
	return repolock.With(root, opts, func() error {
		if err := fn(); err != nil {
			return err
		}
		return AutoPush(root, stdout, stderr)
	})
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
