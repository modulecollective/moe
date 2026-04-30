package cli

import (
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/wiki"
)

func TestReflectKickoffRendersHistorySummary(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	summary := "The twin was seeded in 2026-Q1; auth rewrite landed in 2026-Q2."
	got := reflectKickoff(cfg, summary, "")
	if !strings.Contains(got, "## History summary") {
		t.Errorf("kickoff missing history summary heading:\n%s", got)
	}
	if !strings.Contains(got, summary) {
		t.Errorf("kickoff missing summary body:\n%s", got)
	}
	if !strings.Contains(got, "updated `history-summary.md`") {
		t.Errorf("kickoff missing closing instruction asking the agent to update the summary:\n%s", got)
	}
}

// When the summary is absent (fresh wiki, or migration from a wiki
// that has a checkpoint but no summary file) the kickoff should still
// render the heading and prompt the agent to seed the file from the
// events block at end of pass.
func TestReflectKickoffFreshSummaryFraming(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got := reflectKickoff(cfg, "", "## Events since last reflect\n\n- abc1234 first commit\n")
	if !strings.Contains(got, "## History summary") {
		t.Errorf("kickoff missing history summary heading:\n%s", got)
	}
	if !strings.Contains(got, "no rolling summary yet") {
		t.Errorf("kickoff missing fresh-summary framing:\n%s", got)
	}
	if !strings.Contains(got, "seed `history-summary.md`") {
		t.Errorf("kickoff should tell the agent to seed history-summary.md:\n%s", got)
	}
	// Events block still rendered alongside the empty summary.
	if !strings.Contains(got, "abc1234 first commit") {
		t.Errorf("kickoff missing events body:\n%s", got)
	}
}
