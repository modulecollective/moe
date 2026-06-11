package cli

import (
	"testing"
)

// TestSdlcRegistersReviewStage: review sits between code and test in
// the sdlc workflow ladder, with a registered runnable command.
func TestSdlcRegistersReviewStage(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	stages := wf.Stages()
	want := []string{"design", "code", "review", "test", "push"}
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
}
