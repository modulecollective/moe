package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// stubEvalJudge swaps launchEvalJudge for a fake that records the
// prompts and writes report to the run's eval.md. Restores on cleanup.
func stubEvalJudge(t *testing.T, projectID, runID, report string) (gotSystem, gotUser *string) {
	t.Helper()
	var sys, user string
	old := launchEvalJudge
	launchEvalJudge = func(root, systemPrompt, userPrompt string, stdout, stderr io.Writer) error {
		sys, user = systemPrompt, userPrompt
		if report == "" {
			return nil // judge "forgot" to write the report
		}
		return os.WriteFile(filepath.Join(root, run.EvalPath(projectID, runID)), []byte(report), 0o644)
	}
	t.Cleanup(func() { launchEvalJudge = old })
	return &sys, &user
}

const wellFormedReport = `# Eval: tele/judged

## Verdict

Mostly consistent; one surface the design never names.

## Rubric

- R1 PASS: in-scope changes all present
- R2 FAIL: unmentioned --force flag
- R3 PASS: no unrelated subsystems
- R4 PASS: tests carried
- R5 PASS: no scaffolding
- R6 PASS: pre-registered decisions followed

## Findings

### F1: unmentioned flag

- Where: internal/cli/x.go:42
- Claim: the diff adds a flag the design never names
- Rubric: R2
- Triage:
  - [ ] confirmed
  - [ ] dismissed

## Not seen

Nothing.
`

// seedEvalFixture builds a merged sdlc run whose diff is recoverable
// from history: a target repo at projects/<p>/src with a backdated
// base commit and one run commit, a design canvas, and a journal
// carrying the MoE-Merged trailer pointing at the target tip.
func seedEvalFixture(t *testing.T, projectID, runID string) (root, tip string) {
	t.Helper()
	root = seedCloseFixture(t, projectID, runID, "sdlc", run.StatusMerged)

	canvasRel := run.ContentPath(projectID, runID, "design")
	gittest.WriteAndCommit(t, root, canvasRel, "# Design\n\nAdd the frobnicator.\n", "work: update design")

	repo := filepath.Join(root, "projects", projectID, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "add", "-A")
	gittest.RunWithEnv(t, repo, []string{
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}, "commit", "-m", "pre-run base")
	tip = gittest.WriteAndCommit(t, repo, "a.txt", "after\n", "add the frobnicator")

	trailerstest.CommitTrailer(t, root, "Merge run "+projectID+"/"+runID,
		"MoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Workflow: sdlc\nMoE-Merged: "+tip,
		time.Time{})
	return root, tip
}

// TestEvalMergedRunHappyPath drives the whole verb against a merged
// run: diff recovered from the MoE-Merged tip with the time-window
// base fallback, judge stubbed, report parsed and committed with the
// MoE-Eval-* trailer block — and no MoE-Run, so the eval commit stays
// invisible to the journal's run index.
func TestEvalMergedRunHappyPath(t *testing.T) {
	root, _ := seedEvalFixture(t, "tele", "judged")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	sys, user := stubEvalJudge(t, "tele", "judged", wellFormedReport)

	var out, errb bytes.Buffer
	code := Run([]string{"eval", "tele/judged"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "1 findings, 5/6 rubric pass") {
		t.Fatalf("missing summary line: %q", out.String())
	}

	if !strings.Contains(*sys, "R6:") {
		t.Fatalf("system prompt missing rubric items:\n%s", *sys)
	}
	if !strings.Contains(*sys, "# Stage: code") {
		t.Fatalf("system prompt missing code-stage guidance:\n%s", *sys)
	}
	for _, want := range []string{
		"Add the frobnicator.",         // design canvas
		"add the frobnicator",          // commit log of the judged range
		"--- BEGIN DIFF ---",           // diff payload
		"+after",                       // the actual change
		run.EvalPath("tele", "judged"), // report destination
	} {
		if !strings.Contains(*user, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, *user)
		}
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Eval tele/judged") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Eval-Of: tele/judged",
		"MoE-Eval-Findings: 1",
		"MoE-Eval-Pass: 5/6",
		"MoE-Eval-Model: " + evalJudgeModel,
		"MoE-Eval-Rubric: ",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
	if strings.Contains(head, "MoE-Run:") {
		t.Fatalf("eval commit must not carry MoE-Run:\n%s", head)
	}
}

// TestEvalRefusesWorkflowWithoutRubric: only workflows that ship an
// eval.md are judgeable; everything else refuses rather than guessing
// a consistency pair.
func TestEvalRefusesWorkflowWithoutRubric(t *testing.T) {
	root := seedCloseFixture(t, "tele", "kb-run", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"eval", "tele/kb-run"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no eval rubric") {
		t.Fatalf("stderr should name the missing rubric: %q", errb.String())
	}
}

// TestEvalRefusesWithoutDesignCanvas: no design, nothing to judge
// against.
func TestEvalRefusesWithoutDesignCanvas(t *testing.T) {
	root := seedCloseFixture(t, "tele", "no-design", "sdlc", run.StatusMerged)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"eval", "tele/no-design"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no design canvas") {
		t.Fatalf("stderr should name the missing canvas: %q", errb.String())
	}
}

// TestEvalRefusesExistingReportWithoutForce: the committed report is
// the triage record; clobbering it silently would erase unharvested
// operator judgment.
func TestEvalRefusesExistingReportWithoutForce(t *testing.T) {
	root, _ := seedEvalFixture(t, "tele", "rejudge")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := os.WriteFile(filepath.Join(root, run.EvalPath("tele", "rejudge")), []byte("prior"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"eval", "tele/rejudge"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "--force") {
		t.Fatalf("stderr should point at --force: %q", errb.String())
	}

	// With --force the same invocation re-judges.
	stubEvalJudge(t, "tele", "rejudge", wellFormedReport)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"eval", "--force", "tele/rejudge"}, &out, &errb); code != 0 {
		t.Fatalf("forced re-judge exit=%d stderr=%q", code, errb.String())
	}
}

// TestEvalRefusesToCommitUnparseableReport: a judge that ignored the
// report format must not produce garbage trailers. The report stays on
// disk uncommitted for inspection.
func TestEvalRefusesToCommitUnparseableReport(t *testing.T) {
	root, _ := seedEvalFixture(t, "tele", "garbled")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEvalJudge(t, "tele", "garbled", "free-form musings with no rubric lines\n")
	before := gittest.HeadSHA(t, root)

	var out, errb bytes.Buffer
	if code := Run([]string{"eval", "tele/garbled"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if got := gittest.HeadSHA(t, root); got != before {
		t.Fatalf("unparseable report must not be committed: HEAD moved %s -> %s", before, got)
	}
	if _, err := os.Stat(filepath.Join(root, run.EvalPath("tele", "garbled"))); err != nil {
		t.Fatalf("report should stay on disk for inspection: %v", err)
	}
}

// TestEvalJudgeWroteNothing: the one-shot returning cleanly without a
// report is a failure, not an empty success.
func TestEvalJudgeWroteNothing(t *testing.T) {
	root, _ := seedEvalFixture(t, "tele", "silent")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEvalJudge(t, "tele", "silent", "")

	var out, errb bytes.Buffer
	if code := Run([]string{"eval", "tele/silent"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "without writing") {
		t.Fatalf("stderr should say the judge wrote nothing: %q", errb.String())
	}
}

// TestFindEvalBasePrefersNearestMergedTip: with another run's merged
// tip in the ancestry, the chain rule wins over the time fallback.
func TestFindEvalBasePrefersNearestMergedTip(t *testing.T) {
	repo := t.TempDir()
	gittest.InitAt(t, repo)
	gittest.Commit(t, repo, "ancient")
	prevTip := gittest.Commit(t, repo, "previous run tip")
	gittest.Commit(t, repo, "this run c1")
	tip := gittest.Commit(t, repo, "this run c2")

	base, err := findEvalBase(repo, tip, map[string]string{prevTip: "prev-run"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if base != prevTip {
		t.Fatalf("base=%s want previous tip %s", base, prevTip)
	}
}

// TestFindEvalBaseTimeFallback: no other merged tips (first judged run
// on the project) — the newest commit predating the run's journal
// window becomes the base.
func TestFindEvalBaseTimeFallback(t *testing.T) {
	repo := t.TempDir()
	gittest.InitAt(t, repo)
	gittest.RunWithEnv(t, repo, []string{
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}, "commit", "--allow-empty", "-m", "pre-run history")
	old := gittest.HeadSHA(t, repo)
	tip := gittest.Commit(t, repo, "run work")

	base, err := findEvalBase(repo, tip, nil, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if base != old {
		t.Fatalf("base=%s want backdated commit %s", base, old)
	}
}

// TestFindEvalBaseNoBaseErrors: nothing chains and nothing predates
// the window — fail loudly rather than diffing against an arbitrary
// ancestor.
func TestFindEvalBaseNoBaseErrors(t *testing.T) {
	repo := t.TempDir()
	gittest.InitAt(t, repo)
	gittest.Commit(t, repo, "c1")
	tip := gittest.Commit(t, repo, "c2")

	if _, err := findEvalBase(repo, tip, nil, time.Time{}); err == nil {
		t.Fatal("want error when no base is identifiable")
	}
}

func TestParseEvalReport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		findings    int
		pass, total int
		wantErr     bool
	}{
		{
			name:     "well formed",
			body:     wellFormedReport,
			findings: 1, pass: 5, total: 6,
		},
		{
			name: "no findings",
			body: "## Rubric\n- R1 PASS: a\n- R2 PASS: b\n\n## Findings\n\n_No findings._\n",
			pass: 2, total: 2,
		},
		{
			name:    "no rubric lines",
			body:    "### F1: looks like a finding\nbut no rubric",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, pass, total, err := parseEvalReport(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if findings != tc.findings || pass != tc.pass || total != tc.total {
				t.Fatalf("got findings=%d pass=%d total=%d, want %d %d %d",
					findings, pass, total, tc.findings, tc.pass, tc.total)
			}
		})
	}
}

// TestEvalRubricEmbedded: the sdlc rubric ships in the binary; an
// embed-directive typo becomes a failing test, not a runtime refusal.
func TestEvalRubricEmbedded(t *testing.T) {
	r := moe.EvalRubric("sdlc")
	if r == "" {
		t.Fatal("sdlc eval rubric missing from embed")
	}
	for _, want := range []string{"- R1", "- R6", "### F", "PASS", "FAIL"} {
		if !strings.Contains(r, want) {
			t.Fatalf("rubric missing %q", want)
		}
	}
}
