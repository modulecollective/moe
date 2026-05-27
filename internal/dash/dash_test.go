package dash

import (
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

func TestChainHintSameProjectBareSlug(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	child := &run.Metadata{ID: "next-fix", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/next-fix"}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent, "p/next-fix": child}
	if got, want := chainHint(idx, parent, byKey), " · chained → next-fix"; got != want {
		t.Errorf("same-project hint = %q, want %q", got, want)
	}
}

func TestChainHintCrossProjectQualified(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "a", Workflow: "sdlc", Status: run.StatusInProgress}
	child := &run.Metadata{ID: "next-fix", Project: "b", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"a/fix-bug": "b/next-fix"}}
	byKey := map[string]*run.Metadata{"a/fix-bug": parent, "b/next-fix": child}
	if got, want := chainHint(idx, parent, byKey), " · chained → b/next-fix"; got != want {
		t.Errorf("cross-project hint = %q, want %q", got, want)
	}
}

func TestChainHintSuppressesTerminalChild(t *testing.T) {
	// Decision 1: terminal children are filtered at read time. The
	// trailer still lives in history; the dash row must not advertise
	// a chain that wouldn't fire on the ride.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	for _, status := range []string{run.StatusClosed, run.StatusMerged, run.StatusPromoted, run.StatusPushed} {
		child := &run.Metadata{ID: "next-fix", Project: "p", Workflow: "sdlc", Status: status}
		idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/next-fix"}}
		byKey := map[string]*run.Metadata{"p/fix-bug": parent, "p/next-fix": child}
		if got := chainHint(idx, parent, byKey); got != "" {
			t.Errorf("terminal child (%s) hint = %q, want empty", status, got)
		}
	}
}

func TestChainHintNoEdge(t *testing.T) {
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("no edge: hint = %q, want empty", got)
	}
}

func TestChainHintClearedEdgeSuppressed(t *testing.T) {
	// A cleared edge pins ChainedChild[parent] = "" in the index
	// (so an older Chained-To can't re-assert it). The hint must
	// read empty, not show "chained → " with a dangling pointer.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": ""}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("cleared edge: hint = %q, want empty", got)
	}
}

func TestChainHintChildMissingFromDisk(t *testing.T) {
	// Trailer references a child that doesn't exist on disk (race
	// with delete, or a hand-edited trailer). Hint must read empty
	// rather than dangle.
	parent := &run.Metadata{ID: "fix-bug", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}
	idx := &run.JournalIndex{ChainedChild: map[string]string{"p/fix-bug": "p/ghost"}}
	byKey := map[string]*run.Metadata{"p/fix-bug": parent}
	if got := chainHint(idx, parent, byKey); got != "" {
		t.Errorf("ghost child: hint = %q, want empty", got)
	}
}
