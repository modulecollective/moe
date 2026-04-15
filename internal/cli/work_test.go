package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/request"
)

// newTestBureaucracy initializes a throwaway git repo with scoped git config,
// so commits can happen without polluting ~/.gitconfig. Returns the root path.
func newTestBureaucracy(t *testing.T) string {
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

func writeStageDesign(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, "stages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "design.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// signStage records a MoE-Stage-Signed: <name> trailer for requestID, the same
// way `moe sign` does. Used to move the request past the design stage in tests.
func signStage(t *testing.T, root, requestID, name string) {
	t.Helper()
	msg := "sign: " + name + "\n\nMoE-Request: " + requestID + "\nMoE-Stage-Signed: " + name + "\n"
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", msg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func TestBuildSystemPromptInjectsDesignFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "# Stage: design\n\nresist over-specifying.\n")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "resist over-specifying.") {
		t.Fatalf("prompt missing design fragment:\n%s", got)
	}
	if !strings.Contains(got, "\n---\n") {
		t.Fatalf("prompt missing fragment separator:\n%s", got)
	}
}

func TestBuildSystemPromptOmitsFragmentAfterPRSigned(t *testing.T) {
	root := newTestBureaucracy(t)
	writeStageDesign(t, root, "design guidance body")
	signStage(t, root, "fix-it", "pr")

	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "design guidance body") {
		t.Fatalf("design fragment should drop out once pr is signed:\n%s", got)
	}
}

func TestBuildSystemPromptMissingFragmentIsNotAnError(t *testing.T) {
	root := newTestBureaucracy(t)
	// no stages/design.md written
	md := &request.Metadata{ID: "fix-it", Project: "tele", Title: "Fix it"}
	got, err := buildSystemPrompt(root, md, "spec", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "Your canvas for this document") {
		t.Fatalf("core prompt missing:\n%s", got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("no fragment, no separator expected:\n%s", got)
	}
}
