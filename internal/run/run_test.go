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
