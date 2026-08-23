package cli

import (
	"strings"
	"testing"
)

// TestSdlcRegistersReviewStage: review sits between test and push in
// the sdlc workflow ladder — it is the last judgment before the ship
// gate — with a registered runnable command. The prereq edges are
// asserted directly so a reordering of RegisterStage lands here first.
func TestSdlcRegistersReviewStage(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	stages := wf.Stages()
	want := []string{"design", "code", "test", "review", "push"}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i, s := range stages {
		if s != want[i] {
			t.Fatalf("stages[%d] = %q, want %q", i, s, want[i])
		}
	}
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	if g.Lookup("test") == nil {
		t.Fatal("sdlc group has no test command")
	}
	if g.Lookup("review") == nil {
		t.Fatal("sdlc group has no review command")
	}
	// Stages() order alone would survive a mis-registered prereq edge
	// (the topo sort has freedom when the DAG is under-constrained);
	// pin the edges themselves.
	for _, tc := range []struct{ stage, prereq string }{
		{"code", "design"},
		{"test", "code"},
		{"review", "test"},
		{"push", "review"},
	} {
		prereqs := wf.Prereqs(tc.stage)
		if len(prereqs) != 1 || prereqs[0] != tc.prereq {
			t.Fatalf("Prereqs(%q) = %v, want [%s]", tc.stage, prereqs, tc.prereq)
		}
	}
}

// TestSdlcHelpListsStagesInLadderOrder: the `moe sdlc` usage listing and
// the top-level `moe help` summary both print subcommands in
// registration order, so a ladder reorder that misses the Register
// calls leaves the operator-facing help contradicting docs/reference.md
// and the cascade's own "design, code, test, review, push" error text.
// Pin the stage verbs' relative order in the rendered summary.
func TestSdlcHelpListsStagesInLadderOrder(t *testing.T) {
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	summary := g.Command().Summary
	last := -1
	for _, stage := range []string{"design", "code", "test", "review", "push"} {
		at := strings.Index(summary, stage)
		if at < 0 {
			t.Fatalf("sdlc summary %q does not list %q", summary, stage)
		}
		if at < last {
			t.Fatalf("sdlc summary %q lists %q out of ladder order", summary, stage)
		}
		last = at
	}
}
