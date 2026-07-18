package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
)

// GitHub awareness for the pulse. The journal only sees what moved
// through moe, so work that lands elsewhere — a PR the operator merged
// by hand, a red CI run on the default branch — is invisible to a
// journal-only sweep. This file is the harness-side gather that closes
// that gap: two `gh` reads rendered as one context block appended to the
// survey kickoff, the same shape as the twin-observations line (compute
// what the agent can't cheaply or reliably derive, hand it in).
//
// Deliberately harness-side rather than "let the survey shell out to
// gh": the survey sandbox is read-only and its network access is not
// something the pulse should depend on, and plumbing burns survey
// tokens. The agent may still dig deeper with `gh` where the sandbox
// permits — nothing here depends on it.
//
// Every failure mode (no gh on PATH, offline, no remote, a project with
// no default branch on record) drops the affected part of the block with
// one stderr line. A quiet offline pulse is still a valid pulse.

// ghPRLimit and ghRunLimit bound the two gathers. The merged-PR list is
// filtered by time after the fetch, so the limit is a ceiling on how far
// back one pulse can see, not a display cap — a project that merged more
// than this between two pulses is already past the point where a survey
// reads every row. The run list is per-workflow-latest after collapsing,
// so it only needs enough rows to cover the branch's active workflows.
const (
	ghPRLimit  = 30
	ghRunLimit = 25
)

// ghMergedPR is the projection of `gh pr list --state merged` the block
// renders. Field names match gh's --json keys.
type ghMergedPR struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	MergedAt    time.Time `json:"mergedAt"`
	HeadRefName string    `json:"headRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
}

// foreign reports whether this PR came from outside moe — its head
// branch is not a moe/<run> branch. Foreign merges are the interesting
// rows: moe-run merges the journal already shows, so the gather earns
// its keep on the human-driven landings.
func (p ghMergedPR) foreign() bool {
	return !strings.HasPrefix(p.HeadRefName, branchPrefix)
}

// ghCIRun is the projection of `gh run list --branch <default>`.
type ghCIRun struct {
	WorkflowName string    `json:"workflowName"`
	Conclusion   string    `json:"conclusion"`
	Status       string    `json:"status"`
	URL          string    `json:"url"`
	CreatedAt    time.Time `json:"createdAt"`
}

// runGH shells out to the `gh` CLI and returns its stdout. It is a var
// so tests can drive the renderers without a real GitHub (or a fake gh
// on PATH). Stderr is kept separate from stdout — unlike sync.PRStateOf,
// which shares one buffer — so gh chatter can't corrupt the JSON parse.
var runGH = func(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("gh CLI not found on PATH; install https://cli.github.com/")
		}
		return nil, fmt.Errorf("gh %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// gatherMergedPRs lists PRs merged into repo since the given time.
// `gh pr list` has no merged-since filter, so the window is applied
// here against each row's mergedAt. A zero `since` (no previous pulse
// on record) keeps every row the limit returned — a first pulse gets
// the recent history rather than nothing.
func gatherMergedPRs(repo string, since time.Time) ([]ghMergedPR, error) {
	raw, err := runGH("pr", "list", "--repo", repo,
		"--state", "merged", "--limit", fmt.Sprint(ghPRLimit),
		"--json", "number,title,url,mergedAt,headRefName,author")
	if err != nil {
		return nil, err
	}
	var prs []ghMergedPR
	if err := json.Unmarshal(raw, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list output: %w", err)
	}
	out := prs[:0]
	for _, pr := range prs {
		if !since.IsZero() && !pr.MergedAt.After(since) {
			continue
		}
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MergedAt.After(out[j].MergedAt) })
	return out, nil
}

// gatherCIStatus returns the latest run per workflow on branch, newest
// first. gh returns runs newest-first, so the first row seen for a
// workflow name is that workflow's current verdict.
func gatherCIStatus(repo, branch string) ([]ghCIRun, error) {
	raw, err := runGH("run", "list", "--repo", repo,
		"--branch", branch, "--limit", fmt.Sprint(ghRunLimit),
		"--json", "workflowName,conclusion,status,url,createdAt")
	if err != nil {
		return nil, err
	}
	var runs []ghCIRun
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, fmt.Errorf("parse gh run list output: %w", err)
	}
	seen := map[string]bool{}
	var latest []ghCIRun
	for _, r := range runs {
		if seen[r.WorkflowName] {
			continue
		}
		seen[r.WorkflowName] = true
		latest = append(latest, r)
	}
	sort.SliceStable(latest, func(i, j int) bool { return latest[i].WorkflowName < latest[j].WorkflowName })
	return latest, nil
}

// pulseGitHubContext renders the GitHub context block for the survey
// kickoff, or "" when there is nothing to say (no remote we can address,
// both gathers failed, and no rows). Each gather fails independently:
// a red-CI read that works still renders even if the PR list didn't.
//
// currentRunID is the survey's own run, excluded from the "since last
// pulse" bound — it is already open by the time the kickoff is built.
func pulseGitHubContext(root, projectID, currentRunID string, stderr io.Writer) string {
	pj, err := project.Load(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: github context: load project %s: %v\n", projectID, err)
		return ""
	}
	repo, err := push.GHRepoSpec(pj.Remote)
	if err != nil {
		moePrintf(stderr, "pulse: github context: %v\n", err)
		return ""
	}

	since := lastPulseAt(root, projectID, currentRunID, stderr)

	var sections []string
	if prs, err := gatherMergedPRs(repo, since); err != nil {
		moePrintf(stderr, "pulse: github context: merged PRs: %v\n", err)
	} else {
		sections = append(sections, renderMergedPRs(prs, since))
	}

	switch {
	case pj.DefaultBranch == "":
		moePrintf(stderr, "pulse: github context: project %s has no default_branch on record; skipping CI status\n", projectID)
	default:
		if runs, err := gatherCIStatus(repo, pj.DefaultBranch); err != nil {
			moePrintf(stderr, "pulse: github context: CI status: %v\n", err)
		} else {
			sections = append(sections, renderCIStatus(pj.DefaultBranch, runs))
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return "GitHub context (gathered by the harness — the journal does not see any of this):\n\n" +
		strings.Join(sections, "\n")
}

// renderMergedPRs renders the merged-PR half of the block. Foreign
// merges — head branches that aren't moe/<run> — are marked, because
// they are the ones "What landed" would otherwise miss entirely.
func renderMergedPRs(prs []ghMergedPR, since time.Time) string {
	window := "recently"
	if !since.IsZero() {
		window = "since the last pulse (" + since.UTC().Format("2006-01-02 15:04Z") + ")"
	}
	if len(prs) == 0 {
		return fmt.Sprintf("Merged PRs %s: none.\n", window)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Merged PRs %s:\n", window)
	for _, pr := range prs {
		mark := ""
		if pr.foreign() {
			mark = " [landed outside moe — not a moe/<run> branch]"
		}
		author := pr.Author.Login
		if author == "" {
			author = "unknown"
		}
		fmt.Fprintf(&sb, "- #%d %s — @%s, merged %s, %s%s\n",
			pr.Number, pr.Title, author, pr.MergedAt.UTC().Format("2006-01-02 15:04Z"), pr.URL, mark)
	}
	return sb.String()
}

// renderCIStatus renders the default-branch CI half of the block. A
// failure carries its URL — a red default branch is the flagship input
// to a high-confidence fix proposal, and the agent needs somewhere to
// look.
func renderCIStatus(branch string, runs []ghCIRun) string {
	if len(runs) == 0 {
		return fmt.Sprintf("CI on the default branch (%s): no runs on record.\n", branch)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CI on the default branch (%s), latest run per workflow:\n", branch)
	for _, r := range runs {
		verdict := r.Conclusion
		if verdict == "" {
			// An in-flight run has no conclusion yet; its status ("queued",
			// "in_progress") is the honest thing to report.
			verdict = r.Status + " (still running)"
		}
		if r.Conclusion == "success" {
			fmt.Fprintf(&sb, "- %s: %s\n", r.WorkflowName, verdict)
			continue
		}
		fmt.Fprintf(&sb, "- %s: %s — %s\n", r.WorkflowName, verdict, r.URL)
	}
	return sb.String()
}

// lastPulseAt is the time bound the merged-PR gather filters against:
// the last journal activity on the most recent prior pulse run for this
// project. Returns the zero time when there is no prior pulse (a first
// sweep) or the journal can't be read — both mean "no bound", which the
// renderer states as "recently" rather than pretending to a window it
// doesn't have.
func lastPulseAt(root, projectID, currentRunID string, stderr io.Writer) time.Time {
	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "pulse: github context: scan runs: %v\n", err)
		return time.Time{}
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "pulse: github context: build journal index: %v\n", err)
		return time.Time{}
	}
	var latest time.Time
	for _, md := range mds {
		if md.Workflow != pulseWorkflow || md.Project != projectID || md.ID == currentRunID {
			continue
		}
		if when := idx.LastActivity[md.Project+"/"+md.ID]; when.After(latest) {
			latest = when
		}
	}
	return latest
}
