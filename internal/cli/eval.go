package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

// `moe eval <project>/<run>` runs the cross-stage consistency judge:
// design canvas vs the code diff the run actually landed. The judge is
// an LLM one-shot that writes prose findings to the run's eval.md;
// this verb resolves the diff, assembles the prompt, parses the report
// the judge wrote, and commits it with MoE-Eval-* trailers. Evals are
// triage, not a gate — nothing here blocks a push or reacts to a
// score; the operator confirms or dismisses findings by editing the
// committed report.

// evalJudgeModel pins the judge to a fixed cheap/fast model. Pinning
// trades "judge improves with the newest model" for cross-run
// comparability — a trend over verdicts from drifting models measures
// the drift, not the guidance. Deliberately a dated snapshot id, not a
// floating alias, and deliberately ignoring the run's Agent setting:
// the judge is a measurement instrument, not a stage turn. Re-judging
// history after a deliberate upgrade is cheap (`--force`).
const evalJudgeModel = "claude-haiku-4-5-20251001"

// evalTimeout caps the judge's wall time. Reading a design doc plus a
// capped diff and writing one report is minutes of work; an open-ended
// invocation only helps a judge that has wandered off its lane.
const evalTimeout = 10 * time.Minute

// evalMaxDiffLines caps the diff fed to the judge so a big run can't
// blow the judge's context. The truncation is named in the prompt and
// the rubric tells the judge to report what it couldn't see under
// `## Not seen` — a capped eval says so instead of silently judging
// half a diff.
const evalMaxDiffLines = 5000

// evalBaseWalkLimit bounds the first-parent ancestry walk that looks
// for the previous run's merged tip. 500 commits of headroom between
// consecutive moe runs on one project is far beyond anything the
// journal has seen; past that, failing loudly beats diffing against an
// arbitrary ancient base.
const evalBaseWalkLimit = 500

func init() {
	Register(&Command{
		Name:    "eval",
		Summary: "judge a run's design ↔ code consistency",
		Run:     runEval,
		argKind: argProjectRun,
	})
}

// launchEvalJudge fires the one-shot consistency judge. Pinned to the
// claude backend for the same comparability reason the model is
// pinned. Function-typed var so tests swap in a stub that writes a
// canned report instead of spinning a real subprocess.
var launchEvalJudge = func(root, systemPrompt, userPrompt string, stdout, stderr io.Writer) error {
	a, err := agent.Get("claude")
	if err != nil {
		return err
	}
	_, err = a.ExecuteOneShot(agent.OneShotRequest{
		Root:       root,
		Prompt:     systemPrompt,
		UserPrompt: userPrompt,
		Model:      evalJudgeModel,
		Stdout:     stdout,
		Stderr:     stderr,
		Timeout:    evalTimeout,
	})
	return err
}

func runEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "overwrite an existing eval report (re-judge)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe eval [--force] <project>/<run>")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "eval: %v\n", err)
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "eval: no such run: %s/%s\n", projectID, runID)
			return 1
		}
		moePrintf(stderr, "eval: %v\n", err)
		return 1
	}
	rubric := moe.EvalRubric(md.Workflow)
	if rubric == "" {
		moePrintf(stderr, "eval: workflow %q has no eval rubric; only sdlc runs have a design ↔ diff consistency pair to judge\n", md.Workflow)
		return 1
	}
	evalRel := run.EvalPath(projectID, runID)
	// The exists/--force gate keys on committed state, not the working
	// tree: a leftover report in a resumed session worktree (or
	// root-tree litter from a failed pre-session-era run) must not
	// trip it — that is exactly the state a failed judge run leaves
	// behind for retry.
	if git.Probe(root, "cat-file", "-e", "HEAD:"+evalRel) && !*force {
		moePrintf(stderr, "eval: report already exists at %s; pass --force to re-judge (prior triage stays recoverable in git history)\n", evalRel)
		return 1
	}

	// The judge runs inside a session worktree, like every other
	// read-judge-write flow: auto-pull at open, isolation while the
	// judge writes, rebase + fast-forward + auto-push at close. A
	// failed run never litters the live root checkout.
	sess, closeSess, err := openWikiSession(root, wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runID,
		DocID:       "eval",
		LockPurpose: "eval",
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "eval: %v\n", err)
		return 1
	}

	designPath := filepath.Join(sess.WorktreePath, run.ContentPath(projectID, runID, "design"))
	design, err := os.ReadFile(designPath)
	if err != nil || len(bytes.TrimSpace(design)) == 0 {
		moePrintf(stderr, "eval: no design canvas at %s; nothing to judge against\n", designPath)
		return evalBail(sess, evalRel, stderr)
	}

	// Diff resolution stays root-based: merged-history reads hit the
	// root's project submodule (session worktrees don't populate
	// submodules) and live reads hit the run's sandbox clone. Both
	// are read-only and lock-free.
	d, err := resolveEvalDiff(root, md)
	if err != nil {
		moePrintf(stderr, "eval: %v\n", err)
		return evalBail(sess, evalRel, stderr)
	}

	// MoE-Guidance is stamped on close commits from phase 2 onward;
	// runs that predate stamping just elide the trailer rather than
	// carrying a guessed value.
	guidance := push.TrailerValue(root, runID, "MoE-Guidance")
	systemPrompt := rubric +
		"\n\n---\n\n# The stage guidance the implementer worked under\n\n" +
		moe.Stage(md.Workflow, "code")
	evalAbs := filepath.Join(sess.WorktreePath, evalRel)
	userPrompt := buildEvalUserPrompt(projectID+"/"+runID, evalAbs, string(design), d)

	moePrintf(stdout, "eval: judging %s/%s — %s, range %s..%s\n",
		projectID, runID, d.Source, git.ShortSHA(d.Base), git.ShortSHA(d.Tip))
	// The session worktree may carry a report at the target path — the
	// committed one under --force, or an uncommitted leftover from a
	// failed attempt being retried. Remove it so the judge writes fresh
	// instead of anchoring on the prior verdict; git history keeps all
	// committed triage.
	if err := os.Remove(evalAbs); err != nil && !errors.Is(err, os.ErrNotExist) {
		moePrintf(stderr, "eval: clear prior report: %v\n", err)
		return evalBail(sess, evalRel, stderr)
	}
	if err := launchEvalJudge(sess.WorktreePath, systemPrompt, userPrompt, stdout, stderr); err != nil {
		moePrintf(stderr, "eval: judge: %v\n", err)
		return evalBail(sess, evalRel, stderr)
	}
	body, err := os.ReadFile(evalAbs)
	if err != nil {
		moePrintf(stderr, "eval: judge exited without writing %s\n", evalRel)
		return evalBail(sess, evalRel, stderr)
	}
	findings, pass, total, err := parseEvalReport(string(body))
	if err != nil {
		moePrintf(stderr, "eval: %v\n", err)
		return evalBail(sess, evalRel, stderr)
	}

	block := trailers.Block{
		EvalOf:       projectID + "/" + runID,
		EvalFindings: strconv.Itoa(findings),
		EvalPass:     fmt.Sprintf("%d/%d", pass, total),
		EvalModel:    evalJudgeModel,
		EvalRubric:   moeRevision(),
		Guidance:     guidance,
	}
	msg := fmt.Sprintf("Eval %s/%s\n\n%s", projectID, runID, block.String())
	// Single-writer session branch: no repolock around the commit (the
	// open/close windows carry the locks, same as stage turns). A
	// byte-identical --force re-judge yields ErrNothingToCommit here
	// and CanvasUnchangedError at close — "nothing changed" is true,
	// the worktree stays, and the operator abandons it.
	if err := run.StageAndCommit(sess.WorktreePath, msg, evalRel); err != nil && !errors.Is(err, run.ErrNothingToCommit) {
		moePrintf(stderr, "eval: commit: %v\n", err)
		return evalBail(sess, evalRel, stderr)
	}
	if err := closeSess(true); err != nil {
		moePrintf(stderr, "eval: session close: %v\n", err)
		return 1
	}
	moePrintf(stdout, "eval: %d findings, %d/%d rubric pass — triage the report at %s\n",
		findings, pass, total, evalRel)
	return 0
}

// evalBail cleans up after a failed eval run and always returns exit
// code 1. A worktree holding a judge-written report is left intact for
// inspection (re-running `moe eval` resumes the session, clears the
// leftover, and the judge writes fresh; add --force when a committed
// report exists);
// a worktree with nothing worth keeping is abandoned so failed evals
// don't accumulate session branches.
func evalBail(sess *session.Session, evalRel string, stderr io.Writer) int {
	if _, err := os.Stat(filepath.Join(sess.WorktreePath, evalRel)); err == nil {
		moePrintf(stderr, "eval: report left uncommitted at %s for inspection; re-run `moe eval` to retry, or drop the session: moe session abandon %s\n",
			filepath.Join(sess.WorktreePath, evalRel), sess.Branch)
		return 1
	}
	err := repolock.With(sess.Root, repolock.Options{
		Purpose: "eval-abandon",
		Run:     sess.Project + "/" + sess.Run,
	}, func() error {
		return session.Abandon(sess)
	})
	if err != nil {
		moePrintf(stderr, "eval: abandon session: %v (clean up with: moe session abandon %s)\n", err, sess.Branch)
	}
	return 1
}

// evalDiff is the diff-shaped half of the judge's input: where the
// range came from, the range itself, the one-line commit log, the
// (possibly truncated) diff text, and any commits in the range the
// journal can't attribute to this run.
type evalDiff struct {
	Source       string // "merged history" | "live sandbox clone"
	Base, Tip    string
	Commits      string
	Diff         string
	TruncatedBy  int
	Unattributed []string
}

// resolveEvalDiff picks the judged range. A merged run is judged from
// durable history — the MoE-Merged tip chain in the project submodule
// — so evals are reproducible after the sandbox is gone and backfill
// over closed runs works. A still-open run is judged from its sandbox
// clone instead.
func resolveEvalDiff(root string, md *run.Metadata) (*evalDiff, error) {
	if tip := push.MergedSHA(root, md.ID); tip != "" {
		return mergedEvalDiff(root, md, tip)
	}
	return liveEvalDiff(root, md)
}

func mergedEvalDiff(root string, md *run.Metadata, tip string) (*evalDiff, error) {
	repo := filepath.Join(root, project.SubmoduleDir(md.Project))
	if !git.Probe(repo, "cat-file", "-e", tip+"^{commit}") {
		return nil, fmt.Errorf("merged tip %s is not present in %s; run `moe sync` (or fetch in the submodule) and retry",
			git.ShortSHA(tip), repo)
	}
	tips, err := mergedTips(root, md.Project)
	if err != nil {
		return nil, err
	}
	otherTips := make(map[string]string, len(tips))
	var windowEnd time.Time
	for _, mt := range tips {
		if mt.Run == md.ID {
			if mt.When.After(windowEnd) {
				windowEnd = mt.When
			}
			continue
		}
		otherTips[mt.SHA] = mt.Run
	}
	windowStart, journalEnd, err := runJournalWindow(root, md.ID)
	if err != nil {
		return nil, err
	}
	if windowEnd.IsZero() {
		windowEnd = journalEnd
	}
	base, err := findEvalBase(repo, tip, otherTips, windowStart)
	if err != nil {
		return nil, err
	}
	d := &evalDiff{Source: "merged history", Base: base, Tip: tip}
	if err := readEvalRange(repo, d); err != nil {
		return nil, err
	}
	d.Unattributed = unattributedCommits(repo, base, tip, windowStart, windowEnd)
	return d, nil
}

func liveEvalDiff(root string, md *run.Metadata) (*evalDiff, error) {
	clone, err := resolveRunWorkspacePath(root, md)
	if err != nil {
		return nil, err
	}
	pm, err := project.Load(root, md.Project)
	if err != nil {
		return nil, err
	}
	baseRef := pm.DefaultBranch
	if !git.HasRef(clone, baseRef) {
		baseRef = "origin/" + pm.DefaultBranch
	}
	base, err := git.Output(clone, "merge-base", "HEAD", baseRef)
	if err != nil {
		return nil, fmt.Errorf("merge-base HEAD %s in %s: %w", baseRef, clone, err)
	}
	tip, err := git.HEAD(clone)
	if err != nil {
		return nil, err
	}
	d := &evalDiff{Source: "live sandbox clone", Base: strings.TrimSpace(base), Tip: tip}
	if err := readEvalRange(clone, d); err != nil {
		return nil, err
	}
	return d, nil
}

// readEvalRange fills Commits/Diff/TruncatedBy for the d.Base..d.Tip
// range in repo.
func readEvalRange(repo string, d *evalDiff) error {
	commits, err := git.Output(repo, "log", "--date=short", "--format=%h %ad %s", d.Base+".."+d.Tip)
	if err != nil {
		return fmt.Errorf("log %s..%s in %s: %w", git.ShortSHA(d.Base), git.ShortSHA(d.Tip), repo, err)
	}
	diff, err := git.Output(repo, "diff", d.Base+".."+d.Tip)
	if err != nil {
		return fmt.Errorf("diff %s..%s in %s: %w", git.ShortSHA(d.Base), git.ShortSHA(d.Tip), repo, err)
	}
	d.Commits = strings.TrimSpace(commits)
	d.Diff = diff
	if lines := strings.Split(diff, "\n"); len(lines) > evalMaxDiffLines {
		d.TruncatedBy = len(lines) - evalMaxDiffLines
		d.Diff = strings.Join(lines[:evalMaxDiffLines], "\n")
	}
	return nil
}

// mergedTip is one MoE-Merged record from the bureaucracy journal:
// which run merged, the target-repo tip SHA it recorded, and when the
// journal commit landed.
type mergedTip struct {
	Run  string
	SHA  string
	When time.Time
}

// mergedTips collects every MoE-Merged trailer for the project from
// the journal. The SHA set is the base chain for merged-run diffs;
// the timestamps double as window ends for the unattributed-commit
// detector.
func mergedTips(root, projectID string) ([]mergedTip, error) {
	out, err := git.Output(root, "log",
		"--all-match",
		"--grep", "^MoE-Merged: ",
		"--grep", "^MoE-Project: "+projectID+"$",
		"--format=%ct%x00%B%x1e")
	if err != nil {
		return nil, fmt.Errorf("journal merged-tip scan: %w", err)
	}
	var tips []mergedTip
	for _, record := range strings.Split(out, "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		ct, body, ok := strings.Cut(record, "\x00")
		if !ok {
			continue
		}
		epoch, err := strconv.ParseInt(ct, 10, 64)
		if err != nil {
			continue
		}
		var mt mergedTip
		mt.When = time.Unix(epoch, 0)
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok && mt.Run == "" {
				mt.Run = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "MoE-Merged:"); ok && mt.SHA == "" {
				mt.SHA = strings.TrimSpace(v)
			}
		}
		if mt.Run != "" && mt.SHA != "" {
			tips = append(tips, mt)
		}
	}
	return tips, nil
}

// runJournalWindow returns the time span of the run's journal activity
// — first to last commit carrying its MoE-Run trailer. Zero times with
// nil error mean the run has no journal commits (a hand-seeded run);
// callers degrade by skipping time-based checks.
func runJournalWindow(root, runID string) (start, end time.Time, err error) {
	out, err := git.Output(root, "log", "--grep", "^MoE-Run: "+runID+"$", "--format=%ct")
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("journal window scan: %w", err)
	}
	for _, line := range strings.Fields(out) {
		epoch, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		when := time.Unix(epoch, 0)
		if start.IsZero() || when.Before(start) {
			start = when
		}
		if when.After(end) {
			end = when
		}
	}
	return start, end, nil
}

// findEvalBase picks the diff base for a merged run: the nearest
// first-parent ancestor of tip that is another run's MoE-Merged tip.
// History on moe-managed projects is linear by construction (rebase
// onto default, then fast-forward), so consecutive runs' tips chain
// parent-to-parent and a plain ancestry walk finds the previous one.
// When no other merged tip is an ancestor (the project's first judged
// run, or pre-moe history), fall back to the newest commit that
// predates the run's journal window — everything after it is the run's
// own work plus whatever the unattributed detector flags.
func findEvalBase(repo, tip string, otherTips map[string]string, windowStart time.Time) (string, error) {
	out, err := git.Output(repo, "log", "--first-parent",
		"--format=%H %ct", "-n", strconv.Itoa(evalBaseWalkLimit), tip)
	if err != nil {
		return "", fmt.Errorf("ancestry walk from %s in %s: %w", git.ShortSHA(tip), repo, err)
	}
	timeFallback := ""
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // the tip itself
		}
		sha, ctStr, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if _, isTip := otherTips[sha]; isTip {
			return sha, nil
		}
		if timeFallback == "" && !windowStart.IsZero() {
			if epoch, err := strconv.ParseInt(ctStr, 10, 64); err == nil &&
				time.Unix(epoch, 0).Before(windowStart) {
				timeFallback = sha
			}
		}
	}
	if timeFallback != "" {
		return timeFallback, nil
	}
	return "", fmt.Errorf("no diff base found within %d first-parent ancestors of %s: no other run's MoE-Merged tip and no commit predating the run's journal window",
		evalBaseWalkLimit, git.ShortSHA(tip))
}

// unattributedCommits lists commits in base..tip whose author date
// falls outside the run's journal window — interleaved operator
// commits that would otherwise be silently judged as the run's work.
// They are listed in the report as context, not judged (design open
// question 1: detect, don't guess). Best-effort: a zero window or a
// failed log just yields none.
func unattributedCommits(repo, base, tip string, start, end time.Time) []string {
	if start.IsZero() || end.IsZero() {
		return nil
	}
	out, err := git.Output(repo, "log", "--format=%h%x00%at%x00%an%x00%s", base+".."+tip)
	if err != nil {
		return nil
	}
	var flagged []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\x00", 4)
		if len(parts) != 4 {
			continue
		}
		epoch, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		when := time.Unix(epoch, 0)
		if when.Before(start) || when.After(end) {
			flagged = append(flagged, fmt.Sprintf("%s (%s, %s) %s",
				parts[0], when.Format("2006-01-02"), parts[2], parts[3]))
		}
	}
	return flagged
}

// buildEvalUserPrompt assembles the judge's single user turn: the
// report path, the design canvas, the commit list, and the diff. The
// diff rides between explicit BEGIN/END sentinels rather than a
// markdown fence because diffs of markdown files routinely contain
// fences themselves.
func buildEvalUserPrompt(projectRun, reportAbs, design string, d *evalDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Judge run %s for design ↔ code consistency.\n\n", projectRun)
	fmt.Fprintf(&b, "Write the report to exactly this path, in the format the rubric mandates:\n\n    %s\n\n", reportAbs)
	fmt.Fprintf(&b, "Diff source: %s, range %s..%s.\n",
		d.Source, git.ShortSHA(d.Base), git.ShortSHA(d.Tip))
	if d.TruncatedBy > 0 {
		fmt.Fprintf(&b, "The diff below was truncated: %d trailing lines were dropped. Name this under `## Not seen`.\n", d.TruncatedBy)
	}
	if len(d.Unattributed) > 0 {
		b.WriteString("\nCommits in the range NOT attributed to this run (authored outside its journal window). List them under `## Not seen`; do not judge them:\n")
		for _, c := range d.Unattributed {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	b.WriteString("\n## Design canvas\n\n")
	b.WriteString(design)
	b.WriteString("\n\n## Commits in the judged range\n\n")
	if d.Commits == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(d.Commits)
		b.WriteString("\n")
	}
	b.WriteString("\n## Diff\n\n--- BEGIN DIFF ---\n")
	b.WriteString(d.Diff)
	b.WriteString("\n--- END DIFF ---\n")
	return b.String()
}

var (
	evalRubricLine  = regexp.MustCompile(`(?m)^- R\d+ (PASS|FAIL):`)
	evalFindingHead = regexp.MustCompile(`(?m)^### F\d+:`)
)

// parseEvalReport extracts the trailer-bound numbers from the report
// the judge wrote: finding count and rubric pass/total. A report with
// no parseable rubric lines is a judge that ignored the format — the
// caller refuses to commit it rather than stamping garbage trailers.
func parseEvalReport(body string) (findings, pass, total int, err error) {
	for _, m := range evalRubricLine.FindAllStringSubmatch(body, -1) {
		total++
		if m[1] == "PASS" {
			pass++
		}
	}
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("report has no parseable rubric lines (`- R<n> PASS:` / `- R<n> FAIL:`); the judge did not follow the report format")
	}
	findings = len(evalFindingHead.FindAllString(body, -1))
	return findings, pass, total, nil
}
