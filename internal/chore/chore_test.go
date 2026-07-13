package chore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// writeChore lays down projects/<proj>/chores/<name>/ with the given
// files under root, creating parents as needed.
func writeChore(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllReadsChoreJSON(t *testing.T) {
	root := t.TempDir()
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	writeChore(t, root, "projects/moe/chores/bump-deps/chore.json",
		`{"trigger":"go.mod","workflow":"sdlc","cadence":"720h","cooldown":"48h"}`)
	writeChore(t, root, "projects/moe/chores/bump-deps/prompt.md", "bump the deps\n")

	defs, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("len(defs)=%d, want 1", len(defs))
	}
	got := defs[0]
	want := Definition{
		Project:  "moe",
		Name:     "bump-deps",
		Trigger:  "go.mod",
		Workflow: "sdlc",
		Cadence:  720 * time.Hour,
		Cooldown: 48 * time.Hour,
		Prompt:   "bump the deps\n",
	}
	got.EditedAt = time.Time{} // git log on a non-repo temp dir; not under test
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("def=%+v\nwant=%+v", got, want)
	}
}

func TestLoadAllDefaultsWorkflowAndDayShorthand(t *testing.T) {
	root := t.TempDir()
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	// workflow omitted -> DefaultWorkflow; cooldown uses the d shorthand.
	writeChore(t, root, "projects/moe/chores/readme/chore.json", `{"trigger":"*","cooldown":"7d"}`)

	defs, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("len(defs)=%d, want 1", len(defs))
	}
	if got := defs[0].Workflow; got != DefaultWorkflow {
		t.Errorf("Workflow=%q, want %q", got, DefaultWorkflow)
	}
	if got := defs[0].Cooldown; got != 7*24*time.Hour {
		t.Errorf("Cooldown=%v, want %v", got, 7*24*time.Hour)
	}
}

func TestLoadAllMissingChoreJSON(t *testing.T) {
	root := t.TempDir()
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	// A chore directory with no chore.json is a load error, not silent zeros.
	if err := os.MkdirAll(filepath.Join(root, "projects/moe/chores/orphan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAll(root); err == nil {
		t.Fatal("LoadAll: expected error for missing chore.json, got nil")
	}
}

func TestLoadAllMalformedChoreJSON(t *testing.T) {
	root := t.TempDir()
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	writeChore(t, root, "projects/moe/chores/broken/chore.json", `{not json`)
	if _, err := LoadAll(root); err == nil {
		t.Fatal("LoadAll: expected error for malformed chore.json, got nil")
	}
}

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
			"moe/readme-refresh-2026-05-20": now.Add(-48 * time.Hour),
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
		LastActivity: map[string]time.Time{"moe/readme-refresh-2026-05-28": now.Add(-time.Hour)},
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
