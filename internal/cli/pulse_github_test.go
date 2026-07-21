package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// stubGH replaces runGH for the duration of the test with a table keyed
// by the gh subcommand ("pr" / "run"). A missing key returns an error,
// which is what an offline or gh-less box looks like.
func stubGH(t *testing.T, byVerb map[string]any) {
	t.Helper()
	orig := runGH
	runGH = func(args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("no args")
		}
		payload, ok := byVerb[args[0]]
		if !ok {
			return nil, fmt.Errorf("gh %s: stub has no answer", args[0])
		}
		if err, isErr := payload.(error); isErr {
			return nil, err
		}
		return json.Marshal(payload)
	}
	t.Cleanup(func() { runGH = orig })
}

// TestRenderMergedPRsMarksForeignLandings pins the whole point of the
// merged-PR gather: a PR whose head branch isn't moe/<run> landed
// outside moe, so the journal never saw it and the block has to say so.
func TestRenderMergedPRsMarksForeignLandings(t *testing.T) {
	since := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	prs := []ghMergedPR{
		{Number: 9, Title: "Hand-merged hotfix", URL: "https://x/9",
			MergedAt: since.Add(2 * time.Hour), HeadRefName: "hotfix/tls"},
		{Number: 8, Title: "Landed by moe", URL: "https://x/8",
			MergedAt: since.Add(time.Hour), HeadRefName: branchPrefix + "some-run"},
	}
	prs[0].Author.Login = "someone"
	prs[1].Author.Login = "someone"

	got := renderMergedPRs(prs, since)
	if !strings.Contains(got, "#9 Hand-merged hotfix") {
		t.Fatalf("foreign PR missing from block:\n%s", got)
	}
	foreignLine, moeLine := lineContaining(got, "#9"), lineContaining(got, "#8")
	if !strings.Contains(foreignLine, "landed outside moe") {
		t.Errorf("foreign PR not marked: %q", foreignLine)
	}
	if strings.Contains(moeLine, "landed outside moe") {
		t.Errorf("moe-run PR wrongly marked foreign: %q", moeLine)
	}
	if !strings.Contains(got, "2026-07-17 12:00Z") {
		t.Errorf("since-bound not stated in the block:\n%s", got)
	}
}

// TestGatherMergedPRsFiltersBySince: gh has no merged-since filter, so
// the window is applied here. Rows at or before the bound are dropped.
func TestGatherMergedPRsFiltersBySince(t *testing.T) {
	since := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	stubGH(t, map[string]any{"pr": []ghMergedPR{
		{Number: 1, MergedAt: since.Add(-time.Hour), HeadRefName: "a"},
		{Number: 2, MergedAt: since, HeadRefName: "b"},
		{Number: 3, MergedAt: since.Add(time.Hour), HeadRefName: "c"},
	}})

	got, err := gatherMergedPRs("o/r", since)
	if err != nil {
		t.Fatalf("gatherMergedPRs: %v", err)
	}
	if len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("got %+v, want only PR #3 (strictly after the bound)", got)
	}
}

// TestGatherMergedPRsZeroSinceKeepsEverything: a first pulse has no
// prior run to bound against, and should see recent history rather than
// an empty block.
func TestGatherMergedPRsZeroSinceKeepsEverything(t *testing.T) {
	stubGH(t, map[string]any{"pr": []ghMergedPR{
		{Number: 1, MergedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Number: 2, MergedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}})
	got, err := gatherMergedPRs("o/r", time.Time{})
	if err != nil {
		t.Fatalf("gatherMergedPRs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 — a zero bound filters nothing", len(got))
	}
	if got[0].Number != 2 {
		t.Errorf("rows not newest-first: %+v", got)
	}
}

// TestGatherCIStatusCollapsesToLatestPerWorkflow: gh returns runs
// newest-first, and the survey wants one current verdict per workflow,
// not the whole history.
func TestGatherCIStatusCollapsesToLatestPerWorkflow(t *testing.T) {
	stubGH(t, map[string]any{"run": []ghCIRun{
		{WorkflowName: "test", Conclusion: "failure", URL: "https://x/3"},
		{WorkflowName: "build", Conclusion: "success", URL: "https://x/2"},
		{WorkflowName: "test", Conclusion: "success", URL: "https://x/1"},
	}})
	got, err := gatherCIStatus("o/r", "main")
	if err != nil {
		t.Fatalf("gatherCIStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (one per workflow): %+v", len(got), got)
	}
	byName := map[string]ghCIRun{}
	for _, r := range got {
		byName[r.WorkflowName] = r
	}
	if byName["test"].Conclusion != "failure" {
		t.Errorf("test workflow collapsed to the stale success, want the newest failure: %+v", byName["test"])
	}
}

// TestRenderCIStatusCarriesURLOnlyOnTrouble: a green workflow is one
// word; a red one carries the URL, because a red default branch is the
// flagship input to a fix proposal and the agent needs somewhere to look.
func TestRenderCIStatusCarriesURLOnlyOnTrouble(t *testing.T) {
	got := renderCIStatus("main", []ghCIRun{
		{WorkflowName: "build", Conclusion: "success", URL: "https://x/ok"},
		{WorkflowName: "test", Conclusion: "failure", URL: "https://x/bad"},
		{WorkflowName: "nightly", Status: "in_progress", URL: "https://x/wip"},
	})
	if strings.Contains(lineContaining(got, "- build:"), "https://x/ok") {
		t.Errorf("green workflow should not carry a URL: %q", lineContaining(got, "- build:"))
	}
	if !strings.Contains(lineContaining(got, "- test:"), "https://x/bad") {
		t.Errorf("red workflow must carry its URL: %q", lineContaining(got, "- test:"))
	}
	if !strings.Contains(lineContaining(got, "- nightly:"), "still running") {
		t.Errorf("in-flight run should report its status honestly: %q", lineContaining(got, "- nightly:"))
	}
}

// TestPulseGitHubContextDropsBlockWhenGHFails: no gh, offline, or a
// repo we can't address drops the block with a stderr line and nothing
// else. A quiet offline pulse is still a valid pulse.
func TestPulseGitHubContextDropsBlockWhenGHFails(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	stubGH(t, map[string]any{}) // every verb errors

	var errb bytes.Buffer
	if got := pulseGitHubContext(mustPulseScan(t, root), "moe", "pulse-x", &errb); got != "" {
		t.Fatalf("block rendered despite both gathers failing:\n%s", got)
	}
	if !strings.Contains(errb.String(), "github context") {
		t.Errorf("stderr=%q, want a github-context warning line", errb.String())
	}
}

// TestPulseGitHubContextRendersIndependentHalves: one gather failing
// must not take the other down with it.
func TestPulseGitHubContextRendersIndependentHalves(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	stubGH(t, map[string]any{"run": []ghCIRun{
		{WorkflowName: "test", Conclusion: "failure", URL: "https://x/bad"},
	}})

	var errb bytes.Buffer
	got := pulseGitHubContext(mustPulseScan(t, root), "moe", "pulse-x", &errb)
	if !strings.Contains(got, "https://x/bad") {
		t.Fatalf("CI half missing though its gather succeeded:\n%s", got)
	}
	if strings.Contains(got, "Merged PRs") {
		t.Errorf("PR half rendered though its gather failed:\n%s", got)
	}
	if !strings.Contains(errb.String(), "merged PRs") {
		t.Errorf("stderr=%q, want the failed PR gather warned", errb.String())
	}
}

// TestPulseKickoffCarriesGitHubBlock pins the wiring: the block reaches
// the survey's first turn, not just the renderer's return value.
func TestPulseKickoffCarriesGitHubBlock(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	stubGH(t, map[string]any{"run": []ghCIRun{
		{WorkflowName: "test", Conclusion: "failure", URL: "https://x/bad"},
	}})

	got := pulseKickoffWithContext(root, "moe", "pulse-x", io.Discard)
	if !strings.HasPrefix(got, pulseKickoff) {
		t.Fatal("kickoff no longer leads with the static prompt")
	}
	if !strings.Contains(got, "GitHub context") || !strings.Contains(got, "https://x/bad") {
		t.Errorf("GitHub block missing from the kickoff:\n%s", got)
	}
}

// seedGitHubProject registers a project whose project.json carries the
// remote and default branch the GitHub gather reads. trailerstest's
// SeedProject writes neither, and project.Load refuses a remote-less
// project — so the gather needs its own seed.
func seedGitHubProject(t *testing.T, root, projectID string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"` + projectID + `","remote":"https://github.com/owner/repo.git","default_branch":"main"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "register project "+projectID)
}

// lineContaining returns the first line of body holding needle, or "".
func lineContaining(body, needle string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
