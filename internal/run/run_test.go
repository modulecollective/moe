package run

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRoot initializes a throwaway git repo with scoped config so
// run.New can commit without touching ~/.gitconfig. Mirrors
// cli/stage_test.go#newTestBureaucracy.
func newTestRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}

func TestNewRequiresWorkflow(t *testing.T) {
	root := newTestRoot(t)
	// Register the project so New's "project registered" check passes.
	if err := os.MkdirAll(filepath.Join(root, "projects", "tele"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "projects", "tele", "project.json"),
		[]byte(`{"id":"tele"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	_, err := New(root, "tele", "fix it", Options{Workflow: ""})
	if err == nil {
		t.Fatal("expected error for empty workflow, got nil")
	}
	if !strings.Contains(err.Error(), "workflow is required") {
		t.Fatalf("error should name the required field, got: %v", err)
	}
}

// seedProject registers projectID and commits the project.json so
// run.New's "working tree clean" precondition passes.
func seedProject(t *testing.T, root, projectID string) {
	t.Helper()
	rel := filepath.Join("projects", projectID, "project.json")
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, rel),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "--", rel},
		{"commit", "-m", "register project " + projectID},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// TestNewDerivedSlugAutoSuffixesPastHistory covers the delete-then-reopen
// flow: a run gets created, its dir gets nuked, but the `Open run` commit
// still sits on main with the original slug's MoE-Project / MoE-Run
// trailers. A second run.New with the same title must auto-suffix past
// that history, not hand the new run the ghost of the old one.
func TestNewDerivedSlugAutoSuffixesPastHistory(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	first, err := New(root, "tele", "Fix it", Options{Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if first.ID != "fix-it" {
		t.Fatalf("first id = %q, want %q", first.ID, "fix-it")
	}

	// Operator deletes the run dir and commits the removal; the
	// Open run commit from earlier stays on main.
	deleteRunDir(t, root, "tele", "fix-it")

	second, err := New(root, "tele", "Fix it", Options{Workflow: "quick"})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if second.ID != "fix-it-2" {
		t.Fatalf("second id = %q, want %q (history check should push past deleted slug)", second.ID, "fix-it-2")
	}
}

// TestNewExplicitSlugRefusesHistoryWithSuggestion: --id is never
// auto-suffixed, but if the caller picks a slug that's already in
// history we should refuse loudly *and* hand back a free alternative
// so the operator doesn't have to play the suffix game by hand.
func TestNewExplicitSlugRefusesHistoryWithSuggestion(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")

	if _, err := New(root, "tele", "Fix it", Options{Workflow: "sdlc", ID: "fix-it"}); err != nil {
		t.Fatalf("first New: %v", err)
	}
	deleteRunDir(t, root, "tele", "fix-it")

	_, err := New(root, "tele", "Fix it", Options{Workflow: "quick", ID: "fix-it"})
	if err == nil {
		t.Fatal("expected error reusing a historical slug explicitly, got nil")
	}
	msg := err.Error()
	for _, want := range []string{`"fix-it"`, "tele", "fix-it-2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestNewSlugNotInOtherProject is the guard against over-eager
// uniqueness: a slug used in project A must still be usable in project
// B. The history check is per-project.
func TestNewSlugNotInOtherProject(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "a")
	seedProject(t, root, "b")

	if _, err := New(root, "a", "Fix it", Options{Workflow: "sdlc"}); err != nil {
		t.Fatalf("project a New: %v", err)
	}
	md, err := New(root, "b", "Fix it", Options{Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("project b New: %v", err)
	}
	if md.ID != "fix-it" {
		t.Fatalf("project b id = %q, want %q (cross-project slug reuse is legal)", md.ID, "fix-it")
	}
}

// deleteRunDir removes a run dir and commits the removal, so the
// working tree is clean again while the original `Open run` commit
// still sits in history — the state a manual `rm -rf` + commit leaves
// behind.
func deleteRunDir(t *testing.T, root, projectID, id string) {
	t.Helper()
	for _, args := range [][]string{
		{"rm", "-rf", "--", Dir(projectID, id)},
		{"commit", "-m", "delete run " + projectID + "/" + id},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestLoadRequiresWorkflow(t *testing.T) {
	root := newTestRoot(t)
	runDir := filepath.Join(root, Dir("tele", "fix-it"))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Intentionally omit the "workflow" key.
	md := map[string]any{
		"id":        "fix-it",
		"project":   "tele",
		"title":     "Fix it",
		"status":    StatusInProgress,
		"created":   "2026-04-01",
		"documents": map[string]any{},
	}
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Load(root, "tele", "fix-it")
	if err == nil {
		t.Fatal("expected error loading run.json without workflow key, got nil")
	}
	if !strings.Contains(err.Error(), "workflow is required") {
		t.Fatalf("error should name the required field, got: %v", err)
	}
}
