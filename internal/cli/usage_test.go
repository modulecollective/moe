package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/transcript"
	"github.com/modulecollective/moe/internal/usage"
)

// The aggregator's own tests live in internal/usage. What stays here is
// the verb — flag handling — plus the two render branches whose wording
// the report's honesty depends on.

// TestUsageCommandEmptyBureaucracy: the verb exits clean and says so
// when there is nothing in the window, rather than printing a bare
// header over an empty table.
func TestUsageCommandEmptyBureaucracy(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := Run([]string{"usage"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "No stage transcripts in the window.") {
		t.Errorf("stdout = %q, want the empty case named", out.String())
	}
}

// TestUsageCommandRejectsBadSince keeps the flag honest — a typo'd
// window should be a usage error, not a silent zero-length window that
// reports nothing.
func TestUsageCommandRejectsBadSince(t *testing.T) {
	root := newTestBureaucracy(t)
	t.Setenv("MOE_HOME", root)
	var out, errb bytes.Buffer
	if code := Run([]string{"usage", "--since", "banana"}, &out, &errb); code != 2 {
		t.Fatalf("exit=%d, want 2 for a malformed --since", code)
	}
}

// TestRenderUsageNamesMissingPrices: a model with no price on record has
// to be named in the output, and a wholly-unpriced total rendered as a
// dash rather than a confident $0.00.
func TestRenderUsageNamesMissingPrices(t *testing.T) {
	rep := usage.Report{
		Buckets: []usage.Bucket{{
			Workflow: "sdlc", Stage: "design", Model: "some-unlisted-model", Runs: 1,
			Usage: transcript.ModelUsage{Steps: 1, Output: 30},
		}},
		Runs: []usage.Run{{
			Project: "tele", ID: "ship-it", Workflow: "sdlc", Stages: 1,
			Usage: transcript.ModelUsage{Steps: 1, Output: 30}, Unpriced: 30,
		}},
		ByDay:       map[string]usage.Day{},
		Total:       transcript.ModelUsage{Steps: 1, Output: 30},
		Unpriced:    map[string]int64{"some-unlisted-model": 30},
		Transcripts: 1,
	}
	var buf bytes.Buffer
	renderUsage(&buf, rep, "", "30d")
	got := buf.String()
	if !strings.Contains(got, "no price on record for some-unlisted-model") {
		t.Errorf("render = %q, want the missing price surfaced", got)
	}
	if !strings.Contains(got, "—") || strings.Contains(got, "$0.00*") {
		t.Errorf("render = %q, want wholly unpriced totals rendered as a dash", got)
	}
}

// TestRenderUsageStarsPartialTotals: mixed priced and unpriced tokens
// star every aggregate and explain the star once.
func TestRenderUsageStarsPartialTotals(t *testing.T) {
	mixed := transcript.ModelUsage{Steps: 2, Output: 2_000_000}
	rep := usage.Report{
		Buckets: []usage.Bucket{
			{Workflow: "sdlc", Stage: "design", Model: "claude-fable-5", Runs: 1,
				Usage: transcript.ModelUsage{Steps: 1, Output: 1_000_000}},
			{Workflow: "sdlc", Stage: "design", Model: "some-unlisted-model", Runs: 1,
				Usage: transcript.ModelUsage{Steps: 1, Output: 1_000_000}},
		},
		Runs: []usage.Run{{
			Project: "tele", ID: "mixed", Workflow: "sdlc", Stages: 1,
			Usage: mixed, Dollars: 50, Unpriced: 1_000_000,
		}},
		ByDay: map[string]usage.Day{
			"2026-09-01": {Usage: mixed, Dollars: 50, Unpriced: 1_000_000},
		},
		Total:       mixed,
		Dollars:     50,
		Unpriced:    map[string]int64{"some-unlisted-model": 1_000_000},
		Transcripts: 2,
	}
	var buf bytes.Buffer
	renderUsage(&buf, rep, "", "24h")
	got := buf.String()
	if count := strings.Count(got, "$50.00*"); count < 4 {
		t.Errorf("render = %q, want starred aggregate, window, run, and day totals", got)
	}
	if !strings.Contains(got, "* starred totals exclude tokens from models with no price on record") {
		t.Errorf("render = %q, want the star explained", got)
	}
}

// TestRenderUsageByRunLeadsWithCost: the report arrives in run order for
// the page; the terminal table still leads with the most expensive run.
func TestRenderUsageByRunLeadsWithCost(t *testing.T) {
	rep := usage.Report{
		ByDay: map[string]usage.Day{},
		Runs: []usage.Run{
			{Project: "tele", ID: "cheap", Workflow: "sdlc", Stages: 1, Dollars: 1,
				Usage: transcript.ModelUsage{Steps: 1, Output: 10}},
			{Project: "tele", ID: "dear", Workflow: "sdlc", Stages: 1, Dollars: 90,
				Usage: transcript.ModelUsage{Steps: 1, Output: 900}},
		},
		Total:       transcript.ModelUsage{Steps: 2, Output: 910},
		Dollars:     91,
		Unpriced:    map[string]int64{},
		Transcripts: 2,
	}
	var buf bytes.Buffer
	renderUsage(&buf, rep, "", "30d")
	got := buf.String()
	if strings.Index(got, "tele/dear") > strings.Index(got, "tele/cheap") {
		t.Errorf("render = %q, want the dearer run first in BY RUN", got)
	}
	if rep.Runs[0].ID != "cheap" {
		t.Error("renderUsage reordered the caller's slice; it must sort a copy")
	}
}
