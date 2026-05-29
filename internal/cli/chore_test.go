package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// seedChoreRoot builds a minimal bureaucracy with one project and one
// trigger-only chore, then makes the chore due by stamping a
// MoE-Chore-Touched commit. MOE_HOME points the command's root
// discovery at it.
func seedChoreRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bureaucracy.conf", "")
	write("projects/moe/project.json", `{"id":"moe"}`)
	write("projects/moe/chores/readme-refresh/trigger", "README.md\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed bureaucracy")
	// Make the chore due: a changed-path touch with no completion yet.
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"work: touch\n\nMoE-Chore-Touched: moe/readme-refresh\n")
	t.Setenv("MOE_HOME", root)
	return root
}

func choreDue(t *testing.T, root, name string) bool {
	t.Helper()
	states, err := gatherChoreStates(root, "moe")
	if err != nil {
		t.Fatalf("gatherChoreStates: %v", err)
	}
	for _, s := range states {
		if s.Definition.Name == name {
			return s.Due
		}
	}
	t.Fatalf("chore %q not found in states", name)
	return false
}

func TestRunChoreSkipClearsDueChore(t *testing.T) {
	root := seedChoreRoot(t)
	if !choreDue(t, root, "readme-refresh") {
		t.Fatalf("precondition: chore should be due before skip")
	}

	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"moe/readme-refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runChoreSkip = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("skipped chore moe/readme-refresh")) {
		t.Errorf("stdout missing confirmation: %q", stdout.String())
	}

	// The skip commit's MoE-Chore-Skipped trailer must round-trip
	// through the index and Evaluate to drop the row.
	if choreDue(t, root, "readme-refresh") {
		t.Errorf("chore still due after skip")
	}
	// The marker is an empty commit carrying the trailer.
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !bytes.Contains([]byte(body), []byte("MoE-Chore-Skipped: moe/readme-refresh")) {
		t.Errorf("HEAD commit missing skip trailer:\n%s", body)
	}
}

func TestRunChoreSkipUnknownChore(t *testing.T) {
	seedChoreRoot(t)
	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"moe/nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("runChoreSkip = %d, want 1", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("not found")) {
		t.Errorf("stderr should report not found: %q", stderr.String())
	}
}

func TestRunChoreSkipBadArg(t *testing.T) {
	seedChoreRoot(t)
	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"no-slash"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runChoreSkip = %d, want 2 for malformed arg", code)
	}
}
