package cli

import (
	"slices"
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
	want := []string{"design", "build", "review", "push"}
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
	if g.Lookup("build") == nil {
		t.Fatal("sdlc group has no build command")
	}
	if g.Lookup("review") == nil {
		t.Fatal("sdlc group has no review command")
	}
	// Stages() order alone would survive a mis-registered prereq edge
	// (the topo sort has freedom when the DAG is under-constrained);
	// pin the edges themselves.
	for _, tc := range []struct{ stage, prereq string }{
		{"build", "design"},
		{"review", "build"},
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
// and the cascade's own "design, build, review, push" error text.
// Pin the stage verbs' relative order in the rendered summary.
func TestSdlcHelpListsStagesInLadderOrder(t *testing.T) {
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	summary := g.Command().Summary
	last := -1
	for _, stage := range []string{"design", "build", "review", "push"} {
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

// TestSdlcBuildIsDirectoryForm pins build's declaration: code.md then
// test.md, each seeded from the retired stage of the same name, and the
// retired stages readable as documents but never on the ladder.
func TestSdlcBuildIsDirectoryForm(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	files := wf.StageFiles("build")
	if len(files) != 2 || files[0].Name != "code.md" || files[0].LegacyDoc != "code" ||
		files[1].Name != "test.md" || files[1].LegacyDoc != "test" {
		t.Fatalf("build files = %+v", files)
	}
	if !wf.HasStageGate("build") {
		t.Fatal("a directory-form stage must report a gate so the cascade checks it")
	}
	for _, retired := range []string{"code", "test"} {
		if repl, ok := wf.RetiredStage(retired); !ok || repl != "build" {
			t.Fatalf("RetiredStage(%q) = %q, %v", retired, repl, ok)
		}
		if !slices.Contains(wf.Docs(), retired) {
			t.Fatalf("retired %s must stay a readable document: %v", retired, wf.Docs())
		}
	}
	if wf.StageFiles("review") != nil || wf.StageFiles("design") != nil {
		t.Fatal("only build is directory-form")
	}
}
