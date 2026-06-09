package cli

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func TestTriggeredDueChoresFiltersToTouchedChangedPathDueStates(t *testing.T) {
	states := []chore.State{
		{
			Definition: chore.Definition{Project: "tele", Name: "cadence"},
			Due:        true,
			Reasons:    []string{"cadence"},
		},
		{
			Definition: chore.Definition{Project: "tele", Name: "paths"},
			Due:        true,
			Reasons:    []string{"changed paths"},
		},
		{
			Definition: chore.Definition{Project: "tele", Name: "other"},
			Due:        true,
			Reasons:    []string{"changed paths"},
		},
		{
			Definition: chore.Definition{Project: "tele", Name: "not-due"},
			Due:        false,
			Reasons:    []string{"changed paths"},
		},
	}

	got := triggeredDueChores(states, []string{"tele/paths", "tele/not-due"})
	if len(got) != 1 || got[0].Definition.Key() != "tele/paths" {
		t.Fatalf("triggeredDueChores = %v, want only tele/paths", got)
	}
}

func TestChoreTouchedByPushUsesCurrentRunPushRecord(t *testing.T) {
	root := newTestBureaucracy(t)
	md := trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "push: other/fix-it merged", trailers.Block{
		Run:          "fix-it",
		Project:      "other",
		Workflow:     "sdlc",
		Document:     "push",
		ChoreTouched: []string{"other/chore"},
	}.String(), time.Time{})
	trailerstest.CommitTrailer(t, root, "push: tele/fix-it merged", trailers.Block{
		Run:          "fix-it",
		Project:      "tele",
		Workflow:     "sdlc",
		Document:     "push",
		ChoreTouched: []string{"tele/b", "tele/a"},
	}.String(), time.Time{})

	got, err := choreTouchedByPush(root, md)
	if err != nil {
		t.Fatalf("choreTouchedByPush: %v", err)
	}
	want := []string{"tele/a", "tele/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("choreTouchedByPush = %v, want %v", got, want)
	}
}

func TestSpliceChoreChainInsertsBetweenParentAndExistingChild(t *testing.T) {
	root := newTestBureaucracy(t)
	trailerstest.SeedRun(t, root, "tele", "parent", "sdlc", run.StatusMerged)
	trailerstest.SeedRun(t, root, "tele", "old-child", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "chore-run", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "chain: seed", trailers.Block{
		ChainedTo: []string{"tele/parent tele/old-child"},
	}.String(), time.Time{})

	if err := spliceChoreChain(root, "tele/parent", "tele/chore-run", io.Discard, io.Discard); err != nil {
		t.Fatalf("spliceChoreChain: %v", err)
	}

	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatalf("BuildJournalIndex: %v", err)
	}
	if got, want := idx.ChainedChild["tele/parent"], "tele/chore-run"; got != want {
		t.Fatalf("parent child = %q, want %q", got, want)
	}
	if got, want := idx.ChainedChild["tele/chore-run"], "tele/old-child"; got != want {
		t.Fatalf("chore child = %q, want %q", got, want)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, want := range []string{
		"MoE-Chained-To: tele/parent tele/chore-run",
		"MoE-Chained-To: tele/chore-run tele/old-child",
		"MoE-Chained-To-Removed: tele/parent tele/old-child",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("splice commit missing %q in:\n%s", want, msg)
		}
	}
}
