package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestPdlcRegistered partners with TestSDLCRegistered / TestAuditRegistered:
// a registration drift in init() ordering would silently drop the pdlc
// workflow. Walking the typed CLI to print the group's usage is the
// cheapest integration check that both the CommandGroup and the
// Workflow registry hold the wiring.
func TestPdlcRegistered(t *testing.T) {
	if _, err := LookupWorkflow(pdlcWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(pdlcWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("pdlc group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{pdlcWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "frame", "prd", "chunk", "close", "cat", "log"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("pdlc usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestPdlcWorkflowStageOrder pins the three-stage ladder and its prereq
// edges. frame → prd → chunk is the contract; adding or reordering
// stages should be a deliberate edit that updates this test.
func TestPdlcWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow(pdlcWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{pdlcFrameDoc, pdlcPrdDoc, pdlcChunkDoc}
	if len(got) != len(want) {
		t.Fatalf("stages=%v want=%v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Fatalf("stages=%v want=%v", got, want)
		}
	}
	if pre := wf.Prereqs(pdlcFrameDoc); len(pre) != 0 {
		t.Fatalf("frame prereqs=%v want empty", pre)
	}
	if pre := wf.Prereqs(pdlcPrdDoc); len(pre) != 1 || pre[0] != pdlcFrameDoc {
		t.Fatalf("prd prereqs=%v want [%s]", pre, pdlcFrameDoc)
	}
	if pre := wf.Prereqs(pdlcChunkDoc); len(pre) != 1 || pre[0] != pdlcPrdDoc {
		t.Fatalf("chunk prereqs=%v want [%s]", pre, pdlcPrdDoc)
	}
	// chunk is the terminal stage — its chain prompt is the per-sitting
	// harvest offer, not a successor.
	if succ := wf.Successor(pdlcChunkDoc); succ != "" {
		t.Fatalf("chunk successor=%q want empty (terminal stage)", succ)
	}
}

// TestBuildSystemPromptInjectsPdlcFragments is the wiring check that
// each workflows/pdlc/<stage>.md fragment lands in the prompt for its
// stage. Sentinels on the stage headings plus one load-bearing phrase
// each: frame must teach that its canvas is scaffolding, prd that its
// headings are load-bearing, chunk the three-source reconcile read.
func TestBuildSystemPromptInjectsPdlcFragments(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "grow-the-thing", Project: "moe", Workflow: pdlcWorkflow}
	for _, tc := range []struct {
		stage    string
		sentinel string
	}{
		{pdlcFrameDoc, "scaffolding"},
		{pdlcPrdDoc, "load-bearing"},
		{pdlcChunkDoc, "reconcile read"},
	} {
		got, err := buildSystemPrompt(root, md, tc.stage, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "# Stage: "+tc.stage) {
			t.Fatalf("prompt missing %s fragment heading:\n%s", tc.stage, got)
		}
		if !strings.Contains(got, tc.sentinel) {
			t.Fatalf("%s.md missing sentinel %q:\n%s", tc.stage, tc.sentinel, got)
		}
	}
}

// TestPdlcPrdSkeletonHeadings pins the PRD skeleton's heading set. The
// chunk stage's reconcile read keys off these headings, so renaming one
// is a cross-stage contract change, not a wording tweak.
func TestPdlcPrdSkeletonHeadings(t *testing.T) {
	for _, want := range []string{
		"## Problem",
		"## Scope",
		"## Out of scope",
		"## Shipped / remaining / changed",
	} {
		if !strings.Contains(pdlcPrdCanvasSkeleton, want) {
			t.Fatalf("prd skeleton missing heading %q:\n%s", want, pdlcPrdCanvasSkeleton)
		}
	}
}

// TestPromptPdlcHarvestSkipsSilentlyWhenNothingPending pins the
// no-ceremony exit: a chunk sitting that emitted no followups (or whose
// entries are all harvested already) ends without any prompt or nudge.
func TestPromptPdlcHarvestSkipsSilentlyWhenNothingPending(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "plan", Project: "moe", Workflow: pdlcWorkflow, Status: run.StatusInProgress}

	var out, errb bytes.Buffer
	if code := promptPdlcHarvest(root, md, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("expected silence, got stdout=%q stderr=%q", out.String(), errb.String())
	}

	// All-checked file: same silence — checked entries are the audit
	// trail of past harvests, not pending work.
	rel := run.FollowupsPath(md.Project, md.ID)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Follow-ups\n\n- [x] `done-thing` — Already harvested\n"
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := promptPdlcHarvest(root, md, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("expected silence, got stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestPromptPdlcHarvestNonTTYNudges pins the anti-silent-action rule:
// with unchecked entries pending and no terminal on stdin (go test's
// steady state), the offer degrades to a print-only nudge — harvest
// needs $EDITOR, which a non-TTY caller can't host.
func TestPromptPdlcHarvestNonTTYNudges(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "plan", Project: "moe", Workflow: pdlcWorkflow, Status: run.StatusInProgress}
	rel := run.FollowupsPath(md.Project, md.ID)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Follow-ups\n\n- [ ] `pending-thing` — Not yet harvested\n"
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := promptPdlcHarvest(root, md, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "follow-ups pending") {
		t.Fatalf("expected pending nudge, got stdout=%q", out.String())
	}
}

// TestPdlcChunkChainPromptRoutesToHarvest pins the terminal-stage
// special case in promptNextStageOverride: after a chunk sitting the
// chain prompt offers harvest, never the close nudge — a plan is a run
// that stays open, and close-on-reflex-Enter is the failure mode this
// routing exists to prevent.
func TestPdlcChunkChainPromptRoutesToHarvest(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "plan", Project: "moe", Workflow: pdlcWorkflow, Status: run.StatusInProgress}
	rel := run.FollowupsPath(md.Project, md.ID)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Follow-ups\n\n- [ ] `pending-thing` — Not yet harvested\n"
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := promptNextStage(root, md, pdlcChunkDoc, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "follow-ups pending") {
		t.Fatalf("expected harvest nudge after chunk, got stdout=%q", out.String())
	}
	if strings.Contains(out.String(), "mark the run terminal") {
		t.Fatalf("chunk chain prompt must not fall through to the close nudge, got stdout=%q", out.String())
	}
}
