package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestCommitSessionStartCarriesWorkflowTrailer guards the MoE-Workflow
// trailer added to the eager start-session commit. Paired with
// TestCommitTurnCarriesWorkflowTrailer, they make a no-design `quick`
// run distinguishable in merged history without re-loading run.json —
// the same discriminator also lands on sdlc runs, for symmetry.
func TestCommitSessionStartCarriesWorkflowTrailer(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "bump-timeout", Project: "tele", Workflow: "quick",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	if !strings.Contains(body, "MoE-Workflow: quick") {
		t.Fatalf("commit body missing MoE-Workflow trailer:\n%s", body)
	}
}

// TestCommitTurnCarriesWorkflowTrailer: same, for the closing
// work-turn commit.
func TestCommitTurnCarriesWorkflowTrailer(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "bump-timeout", Project: "tele", Workflow: "quick",
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	contentRel := run.ContentPath("tele", "bump-timeout", "code")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitTurn(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	if !strings.Contains(body, "MoE-Workflow: quick") {
		t.Fatalf("commit body missing MoE-Workflow trailer:\n%s", body)
	}
}
