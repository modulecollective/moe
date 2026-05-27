package runopen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestEditIdeaWritesCanvasAndCommits is the happy path: a fresh idea
// run, EditIdea overwrites the canvas, the commit lands with the
// expected trailer block.
func TestEditIdeaWritesCanvasAndCommits(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	if err := EditIdea(root, "alpha", "my-idea", "# my idea\n\nrefined.\n"); err != nil {
		t.Fatalf("EditIdea: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, run.ContentPath("alpha", "my-idea", "idea")))
	if err != nil {
		t.Fatalf("read canvas: %v", err)
	}
	if want := "# my idea\n\nrefined.\n"; string(got) != want {
		t.Fatalf("canvas body:\nwant: %q\ngot:  %q", want, string(got))
	}

	// HEAD commit carries the work: update idea subject + the trailers
	// the CLI's runIdeaEdit produces.
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"work: update idea",
		"MoE-Run: my-idea",
		"MoE-Project: alpha",
		"MoE-Workflow: idea",
		"MoE-Document: idea",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

// TestEditIdeaNothingToCommit: writing the same body the canvas already
// holds returns run.ErrNothingToCommit so the caller can treat it as a
// success-but-no-op.
func TestEditIdeaNothingToCommit(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# stays the same\n")

	err := EditIdea(root, "alpha", "my-idea", "# stays the same\n")
	if !errors.Is(err, run.ErrNothingToCommit) {
		t.Fatalf("want ErrNothingToCommit, got %v", err)
	}
}

// TestEditIdeaRefusesPromotedIdea: defence in depth — once an idea is
// promoted, the destination's design stage owns the canvas. EditIdea
// must refuse with ErrNotIdea.
func TestEditIdeaRefusesPromotedIdea(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	// Flip the run's status without going through the proper promote
	// path — we just need EditIdea to read status==promoted from disk.
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusPromoted
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	err = EditIdea(root, "alpha", "my-idea", "# rewrite\n")
	if !errors.Is(err, ErrNotIdea) {
		t.Fatalf("want ErrNotIdea, got %v", err)
	}
}

// TestEditIdeaRefusesNonIdeaWorkflow: a sdlc run gets ErrNotIdea even
// when status is in_progress. The destination's design stage owns the
// canvas through its agent session.
func TestEditIdeaRefusesNonIdeaWorkflow(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-run", "# placeholder\n")
	// Re-save as a sdlc run.
	md, err := run.Load(root, "alpha", "my-run")
	if err != nil {
		t.Fatal(err)
	}
	md.Workflow = "sdlc"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	err = EditIdea(root, "alpha", "my-run", "# rewrite\n")
	if !errors.Is(err, ErrNotIdea) {
		t.Fatalf("want ErrNotIdea, got %v", err)
	}
}

// TestEditIdeaMissingRun: a slug that doesn't exist on disk produces
// run.ErrRunNotFound so the serve handler can map it to 404.
func TestEditIdeaMissingRun(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")

	err := EditIdea(root, "ghost", "ghost", "# nope\n")
	if !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

// newIdeaBureaucracy stands up an initialized git repo with one open
// idea run for projectID/slug, canvas seeded with body. Returns the
// repo root.
func newIdeaBureaucracy(t *testing.T, projectID, slug, body string) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")

	// Minimal project.json (run.Save / run.Load don't require it but
	// future extensions / dash gather would).
	projectDir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project.json"),
		[]byte(`{"id":"`+projectID+`","remote":"git@example.test:p.git","default_branch":"main","submodule":"m/p"}`),
		0o644); err != nil {
		t.Fatal(err)
	}

	md := &run.Metadata{
		ID:        slug,
		Project:   projectID,
		Status:    run.StatusInProgress,
		Workflow:  dash.IdeaWorkflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	canvasDir := filepath.Join(root, run.DocDir(projectID, slug, "idea"))
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "content.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Commit the open-run state so EditIdea's StageAndCommit has a
	// clean baseline.
	gittest.Commit(t, root, "seed idea "+projectID+"/"+slug)
	return root
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCloseIdeaBumpsStatusAndCommits(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	if err := CloseIdea(root, "alpha", "my-idea"); err != nil {
		t.Fatalf("CloseIdea: %v", err)
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want %q", md.Status, run.StatusClosed)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"Close idea alpha/my-idea",
		"MoE-Run: my-idea",
		"MoE-Project: alpha",
		"MoE-Workflow: idea",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

func TestCloseIdeaRefusesNonIdeaAndTerminalIdea(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workflow string
		status   string
	}{
		{name: "non-idea", workflow: "sdlc", status: run.StatusInProgress},
		{name: "closed", workflow: dash.IdeaWorkflow, status: run.StatusClosed},
		{name: "promoted", workflow: dash.IdeaWorkflow, status: run.StatusPromoted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
			setRunFields(t, root, "alpha", "my-idea", tc.workflow, tc.status)
			if err := CloseIdea(root, "alpha", "my-idea"); !errors.Is(err, ErrNotIdea) {
				t.Fatalf("want ErrNotIdea, got %v", err)
			}
		})
	}
}

func TestReopenIdeaReopensClosedIdea(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
	setRunFields(t, root, "alpha", "my-idea", dash.IdeaWorkflow, run.StatusClosed)
	gittest.Commit(t, root, "close fixture")

	if err := ReopenIdea(root, "alpha", "my-idea"); err != nil {
		t.Fatalf("ReopenIdea: %v", err)
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Fatalf("status=%q, want %q", md.Status, run.StatusInProgress)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"Reopen idea alpha/my-idea",
		"MoE-Run: my-idea",
		"MoE-Project: alpha",
		"MoE-Workflow: idea",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

func TestReopenIdeaPreservesPromotedDestinationClosedPath(t *testing.T) {
	root := promotedIdeaFixture(t, run.StatusClosed, "alpha/my-idea-dest")

	if err := ReopenIdea(root, "alpha", "my-idea"); err != nil {
		t.Fatalf("ReopenIdea: %v", err)
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Fatalf("status=%q, want %q", md.Status, run.StatusInProgress)
	}
}

func TestReopenIdeaRefusesInvalidStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workflow  string
		status    string
		dest      string
		destState string
	}{
		{name: "non-idea", workflow: "sdlc", status: run.StatusClosed},
		{name: "in-progress", workflow: dash.IdeaWorkflow, status: run.StatusInProgress},
		{name: "pushed-destination", workflow: dash.IdeaWorkflow, status: run.StatusPromoted, dest: "alpha/my-idea-dest", destState: run.StatusPushed},
		{name: "merged-destination", workflow: dash.IdeaWorkflow, status: run.StatusPromoted, dest: "alpha/my-idea-dest", destState: run.StatusMerged},
		{name: "malformed-destination", workflow: dash.IdeaWorkflow, status: run.StatusPromoted, dest: "not/a/pair", destState: run.StatusClosed},
		{name: "missing-destination", workflow: dash.IdeaWorkflow, status: run.StatusPromoted, dest: "alpha/missing", destState: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
			setRunFields(t, root, "alpha", "my-idea", tc.workflow, tc.status)
			if tc.dest != "" {
				if tc.destState != "" && tc.dest == "alpha/my-idea-dest" {
					seedRunMetadata(t, root, "alpha", "my-idea-dest", "sdlc", tc.destState)
				}
				gittest.Commit(t, root, "promote fixture\n\nMoE-Run: my-idea\nMoE-Project: alpha\nMoE-Workflow: idea\nMoE-Promoted-To: "+tc.dest)
			}
			if err := ReopenIdea(root, "alpha", "my-idea"); !errors.Is(err, ErrNotReopenableIdea) {
				t.Fatalf("want ErrNotReopenableIdea, got %v", err)
			}
		})
	}
}

func promotedIdeaFixture(t *testing.T, destStatus, promotedTo string) string {
	t.Helper()
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
	setRunFields(t, root, "alpha", "my-idea", dash.IdeaWorkflow, run.StatusPromoted)
	seedRunMetadata(t, root, "alpha", "my-idea-dest", "sdlc", destStatus)
	gittest.Commit(t, root, "Promote idea alpha/my-idea\n\nMoE-Run: my-idea\nMoE-Project: alpha\nMoE-Workflow: idea\nMoE-Promoted-To: "+promotedTo)
	return root
}

func setRunFields(t *testing.T, root, projectID, slug, workflow, status string) {
	t.Helper()
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	md.Workflow = workflow
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

func seedRunMetadata(t *testing.T, root, projectID, slug, workflow, status string) {
	t.Helper()
	md := &run.Metadata{
		ID:        slug,
		Project:   projectID,
		Status:    status,
		Workflow:  workflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

func TestIdeaTransitionsReturnRunNotFoundForMissingRuns(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	if err := CloseIdea(root, "ghost", "ghost"); !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("CloseIdea: want ErrRunNotFound, got %v", err)
	}
	if err := ReopenIdea(root, "ghost", "ghost"); !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("ReopenIdea: want ErrRunNotFound, got %v", err)
	}
}
