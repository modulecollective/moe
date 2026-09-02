package runopen

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

// TestEditCaptureWritesCanvasAndCommits is the happy path: a fresh idea
// run, EditCapture overwrites the canvas, the commit lands with the
// expected trailer block.
func TestEditCaptureWritesCanvasAndCommits(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	if err := EditCapture(root, "alpha", "my-idea", "# my idea\n\nrefined.\n", io.Discard, io.Discard); err != nil {
		t.Fatalf("EditCapture: %v", err)
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

// TestEditCaptureNothingToCommit: writing the same body the canvas already
// holds returns run.ErrNothingToCommit so the caller can treat it as a
// success-but-no-op.
func TestEditCaptureNothingToCommit(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# stays the same\n")

	err := EditCapture(root, "alpha", "my-idea", "# stays the same\n", io.Discard, io.Discard)
	if !errors.Is(err, run.ErrNothingToCommit) {
		t.Fatalf("want ErrNothingToCommit, got %v", err)
	}
}

// TestEditCaptureRefusesPromotedIdea: defence in depth — once an idea is
// promoted, the destination's design stage owns the canvas. EditCapture
// must refuse with ErrNotCapture.
func TestEditCaptureRefusesPromotedIdea(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	// Flip the run's status without going through the proper promote
	// path — we just need EditCapture to read status==promoted from disk.
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusPromoted
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	err = EditCapture(root, "alpha", "my-idea", "# rewrite\n", io.Discard, io.Discard)
	if !errors.Is(err, ErrNotCapture) {
		t.Fatalf("want ErrNotCapture, got %v", err)
	}
}

// TestEditCaptureRefusesNonIdeaWorkflow: a sdlc run gets ErrNotCapture even
// when status is in_progress. The destination's design stage owns the
// canvas through its agent session.
func TestEditCaptureRefusesNonIdeaWorkflow(t *testing.T) {
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

	err = EditCapture(root, "alpha", "my-run", "# rewrite\n", io.Discard, io.Discard)
	if !errors.Is(err, ErrNotCapture) {
		t.Fatalf("want ErrNotCapture, got %v", err)
	}
}

// TestEditCaptureMissingRun: a slug that doesn't exist on disk produces
// run.ErrRunNotFound so the serve handler can map it to 404.
func TestEditCaptureMissingRun(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")

	err := EditCapture(root, "ghost", "ghost", "# nope\n", io.Discard, io.Discard)
	if !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

// newIdeaBureaucracy stands up an initialized git repo with one open
// idea run for projectID/slug, canvas seeded with body. Returns the
// repo root.
func newIdeaBureaucracy(t *testing.T, projectID, slug, body string) string {
	t.Helper()
	return newCaptureBureaucracy(t, dash.IdeaWorkflow, projectID, slug, body)
}

// newCaptureBureaucracy is the same seed for either capture workflow —
// the intent tests below reuse it with dash.IntentWorkflow.
func newCaptureBureaucracy(t *testing.T, workflow, projectID, slug, body string) string {
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
		Workflow:  workflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	docID, ok := dash.CaptureDocID(workflow)
	if !ok {
		t.Fatalf("newCaptureBureaucracy: %q is not a capture workflow", workflow)
	}
	canvasDir := filepath.Join(root, run.DocDir(projectID, slug, docID))
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "content.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Commit the open-run state so EditCapture's StageAndCommit has a
	// clean baseline.
	gittest.Commit(t, root, "seed "+workflow+" "+projectID+"/"+slug)
	return root
}

// TestEditCaptureWritesIntentCanvas: the intent side of the same
// helper. The commit must be byte-shape-identical to what `moe intent
// edit` writes — subject `work: update intent`, workflow and document
// trailers both naming intent, canvas under the intent doc dir.
func TestEditCaptureWritesIntentCanvas(t *testing.T) {
	root := newCaptureBureaucracy(t, dash.IntentWorkflow, "alpha", "ship-faster", "# ship faster\n")

	if err := EditCapture(root, "alpha", "ship-faster", "# ship faster\n\nsharpened\n", io.Discard, io.Discard); err != nil {
		t.Fatalf("EditCapture: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, run.ContentPath("alpha", "ship-faster", dash.IntentDocID)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# ship faster\n\nsharpened\n" {
		t.Fatalf("canvas=%q", got)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"work: update intent",
		"MoE-Run: ship-faster",
		"MoE-Project: alpha",
		"MoE-Workflow: intent",
		"MoE-Document: intent",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

// TestCloseCaptureClosesIntent: close derives its subject from the
// run's own workflow, so an intent close lands the same `Close intent
// <p>/<r>` subject `moe intent close` writes.
func TestCloseCaptureClosesIntent(t *testing.T) {
	root := newCaptureBureaucracy(t, dash.IntentWorkflow, "alpha", "ship-faster", "# ship faster\n")

	if err := CloseCapture(root, "alpha", "ship-faster", io.Discard, io.Discard); err != nil {
		t.Fatalf("CloseCapture: %v", err)
	}
	md, err := run.Load(root, "alpha", "ship-faster")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want closed", md.Status)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%s%n%b")
	for _, want := range []string{
		"Close intent alpha/ship-faster",
		"MoE-Workflow: intent",
	} {
		if !contains(msg, want) {
			t.Errorf("commit message missing %q\n%s", want, msg)
		}
	}
}

// TestCaptureHelpersRefuseTerminalIntent: an already-closed intent is
// past the edit/close window — a replayed POST must bounce, not
// resurrect it.
func TestCaptureHelpersRefuseTerminalIntent(t *testing.T) {
	root := newCaptureBureaucracy(t, dash.IntentWorkflow, "alpha", "ship-faster", "# ship faster\n")
	setRunFields(t, root, "alpha", "ship-faster", dash.IntentWorkflow, run.StatusClosed)

	if err := EditCapture(root, "alpha", "ship-faster", "x\n", io.Discard, io.Discard); !errors.Is(err, ErrNotCapture) {
		t.Errorf("EditCapture: want ErrNotCapture, got %v", err)
	}
	if err := CloseCapture(root, "alpha", "ship-faster", io.Discard, io.Discard); !errors.Is(err, ErrNotCapture) {
		t.Errorf("CloseCapture: want ErrNotCapture, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCloseCaptureBumpsStatusAndCommits(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")

	if err := CloseCapture(root, "alpha", "my-idea", io.Discard, io.Discard); err != nil {
		t.Fatalf("CloseCapture: %v", err)
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

func TestCloseCaptureRefusesNonIdeaAndTerminalIdea(t *testing.T) {
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
			if err := CloseCapture(root, "alpha", "my-idea", io.Discard, io.Discard); !errors.Is(err, ErrNotCapture) {
				t.Fatalf("want ErrNotCapture, got %v", err)
			}
		})
	}
}

func TestReopenIdeaReopensClosedIdea(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
	setRunFields(t, root, "alpha", "my-idea", dash.IdeaWorkflow, run.StatusClosed)
	gittest.Commit(t, root, "close fixture")

	if err := ReopenIdea(root, "alpha", "my-idea", io.Discard, io.Discard); err != nil {
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

	if err := ReopenIdea(root, "alpha", "my-idea", io.Discard, io.Discard); err != nil {
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
			if err := ReopenIdea(root, "alpha", "my-idea", io.Discard, io.Discard); !errors.Is(err, ErrNotReopenableIdea) {
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
	if err := CloseCapture(root, "ghost", "ghost", io.Discard, io.Discard); !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("CloseCapture: want ErrRunNotFound, got %v", err)
	}
	if err := ReopenIdea(root, "ghost", "ghost", io.Discard, io.Discard); !errors.Is(err, run.ErrRunNotFound) {
		t.Fatalf("ReopenIdea: want ErrRunNotFound, got %v", err)
	}
}

// A promote carries the idea's undelivered operator input onto the run
// it opened, in its own commit after the status bump. Without this the
// entries strand: a promoted idea is terminal, so input.Scan drops it
// and no turn ever renders them.
func TestPromoteCarriesTheIdeasUndeliveredInput(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
	if _, err := input.Add(root, "alpha", "my-idea", "Do the design pass and park.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	var stdout strings.Builder
	promoted, err := Promote(root, "alpha", "my-idea", PromoteOptions{
		Workflow:   "sdlc",
		FirstStage: "design",
		Consent:    "dynamic",
	}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.MarkErr != nil {
		t.Fatalf("MarkErr = %v", promoted.MarkErr)
	}
	if !strings.Contains(stdout.String(), "carried 1 operator input entry from idea alpha/my-idea") {
		t.Fatalf("promote said nothing about the carry: %q", stdout.String())
	}

	dst, err := input.Load(root, "alpha", promoted.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dst.Notes) != 1 || dst.Notes[0].Text != "Do the design pass and park." || dst.Notes[0].Delivered() {
		t.Fatalf("destination record = %+v, want the note pending", dst.Notes)
	}
	src, err := input.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Notes) != 1 || src.Notes[0].Delivered() {
		t.Fatalf("source record = %+v, want it untouched", src.Notes)
	}

	// The carry is its own commit, and it lands after the status bump —
	// while the idea is still in progress both records would list the
	// same entries.
	if got := gittest.Output(t, root, "log", "-1", "--format=%s"); !strings.Contains(got, "input: carried alpha/my-idea#1") {
		t.Fatalf("HEAD = %q, want the carry commit", got)
	}
	if got := gittest.Output(t, root, "log", "-1", "--skip=1", "--format=%s"); !strings.Contains(got, "Promote idea alpha/my-idea") {
		t.Fatalf("HEAD~1 = %q, want the promote commit", got)
	}
}

// A carry that fails must not fail the promote: the entries stay where
// they already were, which is the behaviour that shipped before the
// carry existed, and the operator gets a warning instead of a rollback.
func TestPromoteSurvivesAFailedCarry(t *testing.T) {
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# my idea\n")
	// A machine-written record that violates its own invariants is
	// ErrMalformed, never repaired in place — the one failure mode the
	// carry can hit with the destination freshly opened.
	if err := os.WriteFile(filepath.Join(root, input.Path("alpha", "my-idea")),
		[]byte(`{"notes":[{"id":7,"text":"stranded"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "seed malformed input record")

	var stderr strings.Builder
	promoted, err := Promote(root, "alpha", "my-idea", PromoteOptions{
		Workflow:   "sdlc",
		FirstStage: "design",
	}, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.MarkErr != nil {
		t.Fatalf("MarkErr = %v, want the promote to have completed", promoted.MarkErr)
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusPromoted {
		t.Fatalf("idea status = %q, want promoted despite the carry failure", md.Status)
	}
	if !strings.Contains(stderr.String(), "could not carry its operator input") {
		t.Fatalf("failed carry went unreported: %q", stderr.String())
	}
}
