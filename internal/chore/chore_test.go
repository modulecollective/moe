package chore

import (
	"reflect"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

func TestEvaluateDueFromTouchedAfterCompletion(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	def := Definition{
		Project:  "moe",
		Name:     "readme-refresh",
		Trigger:  "README.md",
		Workflow: "sdlc",
	}
	mds := []*run.Metadata{{
		Project: "moe",
		ID:      "readme-refresh-2026-05-20",
		Status:  run.StatusMerged,
	}}
	idx := &run.JournalIndex{
		LastActivity: map[string]time.Time{
			"readme-refresh-2026-05-20": now.Add(-48 * time.Hour),
		},
		ChoreByRun: map[string]string{
			"moe/readme-refresh-2026-05-20": "moe/readme-refresh",
		},
		ChoreTouched: map[string]time.Time{
			"moe/readme-refresh": now.Add(-time.Hour),
		},
	}

	state := Evaluate(def, mds, idx, now)
	if !state.Due {
		t.Fatalf("expected chore due")
	}
	if got, want := state.Reasons, []string{"changed paths"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reasons=%v want %v", got, want)
	}
}

func TestEvaluateCooldownBlocksDue(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	def := Definition{
		Project:  "moe",
		Name:     "readme-refresh",
		Trigger:  "*",
		Workflow: "sdlc",
		Cooldown: 24 * time.Hour,
	}
	mds := []*run.Metadata{{Project: "moe", ID: "readme-refresh-2026-05-28", Status: run.StatusMerged}}
	idx := &run.JournalIndex{
		LastActivity: map[string]time.Time{"readme-refresh-2026-05-28": now.Add(-time.Hour)},
		ChoreByRun:   map[string]string{"moe/readme-refresh-2026-05-28": "moe/readme-refresh"},
		ChoreTouched: map[string]time.Time{"moe/readme-refresh": now},
	}

	state := Evaluate(def, mds, idx, now)
	if state.Due {
		t.Fatalf("cooldown should block due state")
	}
	if !state.CooldownBlocking {
		t.Fatalf("expected cooldown blocking")
	}
}

func TestEvaluateSkipClearsDue(t *testing.T) {
	// A chore due from a changed-path trigger drops off once a later
	// skip folds into LastCompleted: the touch no longer postdates it.
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	def := Definition{
		Project:  "moe",
		Name:     "readme-refresh",
		Trigger:  "README.md",
		Workflow: "sdlc",
	}
	idx := &run.JournalIndex{
		ChoreTouched: map[string]time.Time{"moe/readme-refresh": now.Add(-2 * time.Hour)},
		ChoreSkipped: map[string]time.Time{"moe/readme-refresh": now.Add(-time.Hour)},
	}

	state := Evaluate(def, nil, idx, now)
	if state.Due {
		t.Fatalf("skip after touch should clear due; reasons=%v", state.Reasons)
	}
	if !state.LastCompleted.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LastCompleted=%v, want skip time %v", state.LastCompleted, now.Add(-time.Hour))
	}
}

func TestEvaluateSkipImposesCooldown(t *testing.T) {
	// Decision 2: a skip behaves like a completion, so the cooldown
	// window holds from the skip time even with a fresh trigger.
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	def := Definition{
		Project:  "moe",
		Name:     "readme-refresh",
		Trigger:  "*",
		Workflow: "sdlc",
		Cooldown: 24 * time.Hour,
	}
	idx := &run.JournalIndex{
		ChoreTouched: map[string]time.Time{"moe/readme-refresh": now},
		ChoreSkipped: map[string]time.Time{"moe/readme-refresh": now.Add(-time.Hour)},
	}

	state := Evaluate(def, nil, idx, now)
	if state.Due {
		t.Fatalf("cooldown from skip should block due; reasons=%v", state.Reasons)
	}
	if !state.CooldownBlocking {
		t.Fatalf("expected cooldown blocking after skip")
	}
}

func TestEvaluateSkipDoesNotMaskNewerTouch(t *testing.T) {
	// A trigger landing after the skip re-surfaces the chore — the skip
	// only satisfies it up to the skip time.
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	def := Definition{
		Project:  "moe",
		Name:     "readme-refresh",
		Trigger:  "README.md",
		Workflow: "sdlc",
	}
	idx := &run.JournalIndex{
		ChoreSkipped: map[string]time.Time{"moe/readme-refresh": now.Add(-2 * time.Hour)},
		ChoreTouched: map[string]time.Time{"moe/readme-refresh": now.Add(-time.Hour)},
	}

	state := Evaluate(def, nil, idx, now)
	if !state.Due {
		t.Fatalf("touch after skip should re-surface the chore")
	}
}

func TestMatchChangedPaths(t *testing.T) {
	defs := []Definition{
		{Project: "moe", Name: "docs", Trigger: "docs/*.md"},
		{Project: "moe", Name: "any", Trigger: "*"},
		{Project: "other", Name: "docs", Trigger: "*"},
	}
	got := MatchChangedPaths(defs, "moe", []string{"docs/readme.md", "internal/x.go"})
	want := []string{"moe/any", "moe/docs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matches=%v want %v", got, want)
	}
}
