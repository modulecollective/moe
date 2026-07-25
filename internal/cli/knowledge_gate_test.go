package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// writeKnowledge seeds a file under projects/<p>/knowledge/ and returns
// the knowledge dir.
func writeKnowledge(t *testing.T, root, projectID, rel, body string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "knowledge")
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCommitTouchedKnowledgeTrigger pins the gate's trigger precision:
// it fires off the turn's own commit, so a turn that wrote elsewhere
// pays nothing even when the project has a knowledge tree on disk.
func TestCommitTouchedKnowledgeTrigger(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed knowledge")
	if !commitTouchedKnowledge(root, "tele") {
		t.Fatal("a commit that added a knowledge file must read as touched")
	}
	if commitTouchedKnowledge(root, "other") {
		t.Fatal("another project's knowledge tree must not read as touched")
	}

	// A later commit elsewhere leaves knowledge alone.
	if err := os.WriteFile(filepath.Join(root, "unrelated.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "unrelated")
	if commitTouchedKnowledge(root, "tele") {
		t.Fatal("a commit that touched nothing under knowledge/ must not trigger the gate")
	}
}

func TestEnforceKnowledgeHygieneCleanTreeIsSilent(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n\n- [DNS](topics/dns.md)\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceKnowledgeHygiene(root, md, "design", stageSessionOpts{}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean tree: exit=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("clean tree should print nothing; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// Interactive: the commit already landed, so the gate refuses the stage
// exit rather than dropping the agent's work, and prints the findings
// verbatim so the operator knows what to have the agent fix.
func TestEnforceKnowledgeHygieneInteractiveRefusesWithFindings(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	code := enforceKnowledgeHygiene(root, md, "design", stageSessionOpts{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("orphaned topic should refuse the close, got exit=%d", code)
	}
	for _, want := range []string{
		"projects/tele/knowledge",
		"1 structural finding",
		"topics/dns.md",
		"Re-run this stage",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal missing %q:\n%s", want, stderr.String())
		}
	}
}

// Headless: one bounded fix turn, then the gate re-scans. A fix that
// lands clears the turn; the recovery never runs twice.
func TestEnforceKnowledgeHygieneHeadlessFixTurnClears(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	var dispatches int
	var gotOpts stageSessionOpts
	stubKnowledgeFixTurn(t, func(projectID, runID, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		dispatches++
		gotOpts = opts
		// The fix the agent would make: index the orphan.
		writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n\n- [DNS](topics/dns.md)\n")
		return 0
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	code := enforceKnowledgeHygiene(root, md, "design", stageSessionOpts{Headless: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a fix turn that cleared the findings should pass, got exit=%d stderr=%q", code, stderr.String())
	}
	if dispatches != 1 {
		t.Fatalf("fix turn dispatched %d times, want exactly 1", dispatches)
	}
	if !gotOpts.knowledgeFixTurn {
		t.Error("fix turn must be marked so the gate doesn't recurse into itself")
	}
	if !gotOpts.Headless {
		t.Error("fix turn must stay headless")
	}
	if gotOpts.InitialPromptBuilder != nil {
		t.Error("fix turn must not carry a prompt builder — the findings are the prompt")
	}
	for _, want := range []string{"topics/dns.md", "add one line to your canvas"} {
		if !strings.Contains(gotOpts.InitialPrompt, want) {
			t.Errorf("fix kickoff missing %q:\n%s", want, gotOpts.InitialPrompt)
		}
	}
}

// Headless, unbounded-recovery guard: a fix turn that doesn't stick
// parks the run instead of trying again.
func TestEnforceKnowledgeHygieneHeadlessParksAfterOneAttempt(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	var dispatches int
	stubKnowledgeFixTurn(t, func(string, string, string, stageSessionOpts, io.Writer, io.Writer) int {
		dispatches++
		return 0 // turn "succeeded" but fixed nothing
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceKnowledgeHygiene(root, md, "design", stageSessionOpts{Headless: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("surviving findings should park the run, got exit=%d", code)
	}
	if dispatches != 1 {
		t.Fatalf("recovery must stay bounded at one attempt, got %d", dispatches)
	}
	if !strings.Contains(stderr.String(), "survived the fix turn") {
		t.Errorf("park message should name what happened:\n%s", stderr.String())
	}
}

// A failing fix turn propagates its own exit code — the gate doesn't
// mask a broken agent turn behind its own refusal.
func TestEnforceKnowledgeHygieneHeadlessPropagatesFixTurnFailure(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	stubKnowledgeFixTurn(t, func(string, string, string, stageSessionOpts, io.Writer, io.Writer) int {
		return 7
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceKnowledgeHygiene(root, md, "design", stageSessionOpts{Headless: true}, &stdout, &stderr); code != 7 {
		t.Fatalf("fix-turn exit should propagate, got %d", code)
	}
}

// stubKnowledgeFixTurn swaps the gate's re-dispatch seam for the
// duration of the test, so the recovery path runs without a real
// session worktree.
func stubKnowledgeFixTurn(t *testing.T, fn func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int) {
	t.Helper()
	prev := dispatchKnowledgeFixTurn
	dispatchKnowledgeFixTurn = fn
	t.Cleanup(func() { dispatchKnowledgeFixTurn = prev })
}
