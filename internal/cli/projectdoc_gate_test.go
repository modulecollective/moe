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

// TestCommitTouchedProjectDocsTrigger pins the gate's trigger
// precision: it fires off the turn's own commit, so a turn that wrote
// elsewhere pays nothing even when the project has both trees on disk,
// and a turn that wrote one tree scans only that one.
func TestCommitTouchedProjectDocsTrigger(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeTwinDoc(t, root, "tele", "vision.md", "# Vision\n\nA thing.\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed both trees")
	if got := commitTouchedProjectDocs(root, "tele"); len(got) != 2 {
		t.Fatalf("a commit touching both trees should report both, got %v", got)
	}
	if got := commitTouchedProjectDocs(root, "other"); len(got) != 0 {
		t.Fatalf("another project's trees must not read as touched, got %v", got)
	}

	// A commit that touches only the twin scans only the twin.
	writeTwinDoc(t, root, "tele", "vision.md", "# Vision\n\nA revised thing.\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "twin only")
	if got := commitTouchedProjectDocs(root, "tele"); len(got) != 1 || got[0] != "digital-twin" {
		t.Fatalf("twin-only commit should report just the twin, got %v", got)
	}

	// A later commit elsewhere leaves both alone.
	if err := os.WriteFile(filepath.Join(root, "unrelated.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "unrelated")
	if got := commitTouchedProjectDocs(root, "tele"); len(got) != 0 {
		t.Fatalf("a commit that touched neither tree must not trigger the gate, got %v", got)
	}
}

// writeTwinDoc seeds a managed doc under projects/<p>/digital-twin/.
func writeTwinDoc(t *testing.T, root, projectID, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "digital-twin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnforceProjectDocHygieneCleanTreeIsSilent(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n\n- [DNS](topics/dns.md)\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "design", stageSessionOpts{}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean tree: exit=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("clean tree should print nothing; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// Interactive: the commit already landed, so the gate refuses the stage
// exit rather than dropping the agent's work, and prints the findings
// verbatim so the operator knows what to have the agent fix.
func TestEnforceProjectDocHygieneInteractiveRefusesWithFindings(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "design", stageSessionOpts{}, &stdout, &stderr)
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
func TestEnforceProjectDocHygieneHeadlessFixTurnClears(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	var dispatches int
	var gotOpts stageSessionOpts
	stubProjectDocFixTurn(t, func(projectID, runID, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		dispatches++
		gotOpts = opts
		// The fix the agent would make: index the orphan.
		writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n\n- [DNS](topics/dns.md)\n")
		return 0
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "design", stageSessionOpts{Headless: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("a fix turn that cleared the findings should pass, got exit=%d stderr=%q", code, stderr.String())
	}
	if dispatches != 1 {
		t.Fatalf("fix turn dispatched %d times, want exactly 1", dispatches)
	}
	if !gotOpts.projectDocFixTurn {
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
func TestEnforceProjectDocHygieneHeadlessParksAfterOneAttempt(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	var dispatches int
	stubProjectDocFixTurn(t, func(string, string, string, stageSessionOpts, io.Writer, io.Writer) int {
		dispatches++
		return 0 // turn "succeeded" but fixed nothing
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "design", stageSessionOpts{Headless: true}, &stdout, &stderr); code != 1 {
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
func TestEnforceProjectDocHygieneHeadlessPropagatesFixTurnFailure(t *testing.T) {
	root := newTestBureaucracy(t)
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	stubProjectDocFixTurn(t, func(string, string, string, stageSessionOpts, io.Writer, io.Writer) int {
		return 7
	})

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "design", stageSessionOpts{Headless: true}, &stdout, &stderr); code != 7 {
		t.Fatalf("fix-turn exit should propagate, got %d", code)
	}
}

// stubProjectDocFixTurn swaps the gate's re-dispatch seam for the
// duration of the test, so the recovery path runs without a real
// session worktree.
func stubProjectDocFixTurn(t *testing.T, fn func(projectID, runID, docID string, opts stageSessionOpts, stdout, stderr io.Writer) int) {
	t.Helper()
	prev := dispatchProjectDocFixTurn
	dispatchProjectDocFixTurn = fn
	t.Cleanup(func() { dispatchProjectDocFixTurn = prev })
}

// The twin is the second scanner behind the same gate. A dangling
// cross-reference — a citation naming a heading that doesn't exist in
// the doc it cites — is the finding class the reflect ladder's
// post-flight scan used to catch, and it has to survive the fold.
func TestEnforceProjectDocHygieneScansTheTwin(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTwin(t, root, "tele")
	writeTwinDoc(t, root, "tele", "architecture.md",
		"# Architecture\n\nThe seam is described in patterns.md \"No Such Heading\".\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	code := enforceProjectDocHygiene(root, md, []string{"digital-twin"}, "code", stageSessionOpts{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("a dangling xref should refuse the close, got exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"projects/tele/digital-twin", "No Such Heading", "Re-run this stage"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal missing %q:\n%s", want, stderr.String())
		}
	}
}

// Both trees touched, both broken: one gate, one refusal, both blocks
// rendered and both counted. The point of routing through one gate
// rather than two is that the operator reads one report.
func TestEnforceProjectDocHygieneRendersBothScanners(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTwin(t, root, "tele")
	writeTwinDoc(t, root, "tele", "architecture.md",
		"# Architecture\n\nSee patterns.md \"No Such Heading\".\n")
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir, "digital-twin"}, "code", stageSessionOpts{}, &stdout, &stderr); code != 1 {
		t.Fatalf("want a refusal, got exit=%d", code)
	}
	for _, want := range []string{
		"2 structural finding",
		"projects/tele/knowledge and projects/tele/digital-twin",
		"Knowledge tree findings",
		"topics/dns.md",
		"No Such Heading",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("combined report missing %q:\n%s", want, stderr.String())
		}
	}
}

// A knowledge-only turn scans only knowledge: a twin left structurally
// broken by some earlier run must not fail a turn that never touched
// it. Trigger precision is what keeps the gate from being a tax.
func TestEnforceProjectDocHygieneScansOnlyWhatWasTouched(t *testing.T) {
	root := newTestBureaucracy(t)
	seedTwin(t, root, "tele")
	writeTwinDoc(t, root, "tele", "architecture.md",
		"# Architecture\n\nSee patterns.md \"No Such Heading\".\n")
	writeKnowledge(t, root, "tele", "index.md", "# Knowledge\n\n- [DNS](topics/dns.md)\n")
	writeKnowledge(t, root, "tele", filepath.Join("topics", "dns.md"), "# DNS\n\nresolvers.\n")

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow}
	var stdout, stderr bytes.Buffer
	if code := enforceProjectDocHygiene(root, md, []string{knowledgeSubdir}, "code", stageSessionOpts{}, &stdout, &stderr); code != 0 {
		t.Fatalf("a knowledge-only turn must not be failed by the twin: exit=%d stderr=%q", code, stderr.String())
	}
}

// seedTwin stubs the five managed docs so a scan finds no missing docs
// and no empty stubs — the baseline a targeted breakage builds on.
func seedTwin(t *testing.T, root, projectID string) {
	t.Helper()
	for _, d := range twinManagedDocs {
		writeTwinDoc(t, root, projectID, d.Filename,
			"# "+d.Title+"\n\nPlaceholder prose so the doc isn't an empty stub.\n")
	}
}
