package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// TestSDLCCloseCommitsInPlaceLoreAmendment covers the amend arm of the
// close harvest: TestSDLCCloseHarvestsLore pins the *addition* of a new
// lore/<slug>.md, but an entry whose `supersedes:` names its own slug
// rewrites a file already tracked in the bureaucracy. enterTerminal
// stages `lore/` as a directory precisely so a modification rides along
// like an addition — this pins that, and that the tree is clean after.
func TestSDLCCloseCommitsInPlaceLoreAmendment(t *testing.T) {
	root := seedCloseFixture(t, "tele", "repoint", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	loreDir := filepath.Join(root, wiki.LoreDirRel)
	if err := os.MkdirAll(loreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := strings.Join([]string{
		"---",
		"title: Old title",
		"applies-when: something",
		"discovered-in: other/runs/first",
		"---",
		"",
		"# Old title",
		"",
		"Body referencing dangling-slug.",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(loreDir, "fact.md"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "lore/fact.md")
	gittest.Run(t, root, "commit", "-m", "seed lore")

	writeLoreFeedback(t, root, "tele", "repoint", strings.Join([]string{
		"- [ ] `fact` — Old title",
		"",
		"  applies-when: something",
		"",
		"  supersedes: fact",
		"",
		"  Body referencing live-slug.",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/repoint"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	got := readLoreEntry(t, root, "fact")
	for _, want := range []string{
		"discovered-in: other/runs/first",
		"updated-in: tele/runs/repoint",
		"Body referencing live-slug.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lore/fact.md missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dangling-slug") {
		t.Errorf("lore/fact.md still references the old slug:\n%s", got)
	}

	// The modification must be *in* the close commit, and the tree
	// clean afterwards — a missed `git add` would leave it dirty.
	headPaths := gitLog(t, root, "-1", "--name-only", "--format=")
	if !strings.Contains(headPaths, "lore/fact.md") {
		t.Errorf("close commit didn't include the amended lore/fact.md:\n%s", headPaths)
	}
	if status := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("tree dirty after close:\n%s", status)
	}
}
