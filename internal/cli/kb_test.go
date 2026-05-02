package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestKBRegistered is the partner to TestSDLCRegistered: it guards
// against init() ordering / registration drift silently dropping the
// kb workflow's top-level entry.
func TestKBRegistered(t *testing.T) {
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	if wf.Summary == "" {
		t.Fatal("kb workflow summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{"kb"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "research", "summarize", "close", "lint"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("kb usage missing subcommand %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "shelve") {
		t.Fatalf("kb usage still lists removed `shelve` subcommand: %q", out.String())
	}
}

// TestKBLintIsFacadeNotStage guards the design choice that lint is
// out-of-band relative to runs: it must dispatch as a workflow
// subcommand but must not appear in the stage ladder (otherwise
// `moe dash` and the next-stage prompt would treat it as a step to
// satisfy on every run).
func TestKBLintIsFacadeNotStage(t *testing.T) {
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range wf.Stages() {
		if s == "lint" {
			t.Fatalf("lint must be a facade, but it appeared in the stage ladder: %v", wf.Stages())
		}
	}
}

// TestKBLintRequiresProjectArg confirms the dispatch path reaches
// runLintSession's flag parser. The actual run-against-a-real-project
// path requires a live `claude` binary and a registered project, so
// it's not exercised here — we just verify the usage line lands when
// arguments are missing.
func TestKBLintRequiresProjectArg(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"kb", "lint"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit on missing project arg, got 0\nstderr: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "usage: moe kb lint <project>") {
		t.Fatalf("missing usage line in stderr:\n%s", errb.String())
	}
}

// TestKBWorkflowStageOrder confirms the workflow's stage ladder is
// research → summarize and that `new` is a facade, not a stage. The
// wiki-engine reshape collapses the old `shelve` stage; summarize is
// now the terminal ingest stage.
func TestKBWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{"research", "summarize"}
	if len(got) != len(want) {
		t.Fatalf("stages=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stages[%d]=%q want %q", i, got[i], want[i])
		}
	}
	for _, s := range got {
		if s == "new" {
			t.Fatalf("`new` leaked into Stages(): %v", got)
		}
		if s == "shelve" {
			t.Fatalf("`shelve` should be removed: %v", got)
		}
	}
}

// TestKBWorkflowNextWalksStages mirrors TestWorkflowNextWalksStages
// for the kb ladder: no turns → research → summarize → done.
func TestKBWorkflowNextWalksStages(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "kb", Status: run.StatusInProgress}

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "research" {
		t.Fatalf("no turns: expected stage research, got kind=%v name=%v", kind, nameOrNil(next))
	}

	t0 := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "p", "r", "kb", "research", t0)
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "summarize" {
		t.Fatalf("after research: expected stage summarize, got kind=%v name=%v", kind, nameOrNil(next))
	}

	commitWorkTurnAt(t, root, "p", "r", "kb", "summarize", t0.Add(time.Hour))
	_, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindDone {
		t.Fatalf("after summarize: expected done, got kind=%v", kind)
	}
}

// TestBuildSystemPromptInjectsKBResearchFragment is the wiring check
// that stages/kb/research.md actually lands in the prompt when the
// run names the kb workflow. Matches the sdlc fragment injection
// tests — a broken embed directive or a moved file becomes a failing
// test here rather than a silent prompt regression.
func TestBuildSystemPromptInjectsKBResearchFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Title: "DNS basics", Workflow: "kb"}
	got, err := buildSystemPrompt(root, md, "research", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: research") {
		t.Fatalf("prompt missing research fragment heading:\n%s", got)
	}
}

func TestBuildSystemPromptInjectsKBSummarizeFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Title: "DNS basics", Workflow: "kb"}
	got, err := buildSystemPrompt(root, md, "summarize", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: summarize") {
		t.Fatalf("prompt missing summarize fragment heading:\n%s", got)
	}
}

// TestKBWikiBuilderPopulatesPromptAndPaths is the integration check
// that ties the kb workflow to the wiki engine: building the kb wiki
// config and feeding it through buildSystemPrompt should produce the
// engine's wiki-section header, the open-schema rules, the resolved
// knowledge/ path, and the topics/<topic>.md on-disk-shape note.
// Catches a wiring regression where the engine stops landing in the
// prompt without anything else exploding.
func TestKBWikiBuilderPopulatesPromptAndPaths(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Title: "DNS basics", Workflow: "kb"}
	cfg, err := kbWikiBuilder(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("kbWikiBuilder returned nil config")
	}
	got, err := buildSystemPrompt(root, md, "summarize", "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Wiki: kb (open-schema)",
		"projects/tele/knowledge",
		"topics/<topic>.md",
		"split, merge, rename, retire",
		"open-schema knowledge base",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}
