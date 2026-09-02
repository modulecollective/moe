package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestProjectCommitDirsPerWorkflow pins the whitelist: sdlc stages
// carry hooks/, chores/, knowledge/, and digital-twin/ edits, and every
// other workflow (chat, pulse, …) carries nothing.
func TestProjectCommitDirsPerWorkflow(t *testing.T) {
	cases := []struct {
		workflow string
		want     []string
	}{
		{sdlcWorkflow, []string{"hooks", "chores", "knowledge", "digital-twin"}},
		{chatWorkflow, nil},
	}
	for _, tc := range cases {
		got := projectCommitDirs(tc.workflow)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("projectCommitDirs(%q) = %v, want %v", tc.workflow, got, tc.want)
		}
	}
}

// TestStageProjectDirsSkipsMissingDirs: `git add --` fails on a
// pathspec matching nothing, and most projects have neither hooks/ nor
// chores/. The callback stats before returning, so a project with only
// one of the two still commits cleanly.
func TestStageProjectDirsSkipsMissingDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "projects", "tele", "chores"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{Project: "tele", ID: "fix-it", Workflow: sdlcWorkflow}
	paths, err := stageProjectDirs(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join("projects", "tele", "chores") {
		t.Fatalf("got %v, want [projects/tele/chores]", paths)
	}

	// No project dirs at all: nothing to stage, no error.
	bare := &run.Metadata{Project: "ghost", ID: "fix-it", Workflow: sdlcWorkflow}
	paths, err = stageProjectDirs(root, bare)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("got %v, want none", paths)
	}
}

// TestCommitTurnCarriesSdlcChoreEdit is the regression this run was
// opened against: an sdlc stage that authors a chore alongside its
// canvas had the chore file left untracked, dying with the pruned
// session worktree while the canvas claimed it landed.
func TestCommitTurnCarriesSdlcChoreEdit(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow,
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	contentRel := run.ContentPath("tele", "fix-it", "code")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# code\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	choreRel := filepath.Join("projects", "tele", "chores", "update-model-prices", "chore.json")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, choreRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, choreRel), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	extras, err := stageProjectDirs(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitTurn(root, md, "code", 0, extras...); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
	if !strings.Contains(names, choreRel) {
		t.Errorf("turn commit missing chore file %q in:\n%s", choreRel, names)
	}
	if !strings.Contains(names, contentRel) {
		t.Errorf("turn commit missing canvas %q in:\n%s", contentRel, names)
	}
}

// TestCommitTurnStampsTimedOut: a turn killed by the headless cap
// carries exactly one MoE-Timed-Out line naming the cap that fired,
// rendered right after MoE-Session. This trailer is the whole durable
// record of the kill — the transcript it interrupted is overwritten by
// the next drive of the stage — so `git log --grep='^MoE-Timed-Out:'`
// has to find it.
func TestCommitTurnStampsTimedOut(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow,
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	contentRel := run.ContentPath("tele", "fix-it", "code")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# code\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := commitTurn(root, md, "code", 3*time.Hour); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	if n := strings.Count(body, "MoE-Timed-Out:"); n != 1 {
		t.Fatalf("want exactly one MoE-Timed-Out line, got %d:\n%s", n, body)
	}
	if !strings.Contains(body, "MoE-Timed-Out: 3h0m0s") {
		t.Errorf("trailer should name the cap that fired:\n%s", body)
	}
	sessionLine := "MoE-Session: " + md.Documents["code"].Session + "\n"
	if !strings.Contains(body, sessionLine+"MoE-Timed-Out: 3h0m0s") {
		t.Errorf("MoE-Timed-Out should render directly after MoE-Session:\n%s", body)
	}

	// The grep the idea asked for.
	found := gittest.Output(t, root, "log", "--format=%H", "--grep=^MoE-Timed-Out:")
	if strings.TrimSpace(found) == "" {
		t.Error("git log --grep='^MoE-Timed-Out:' found nothing")
	}
}

// TestCommitTurnWithoutTimeoutIsUnchanged: the ordinary turn — every
// turn but the rare kill — writes a message byte-identical to what it
// wrote before the trailer existed. No MoE-Timed-Out, and nothing else
// shifted around it.
func TestCommitTurnWithoutTimeoutIsUnchanged(t *testing.T) {
	root := newTestBureaucracy(t)

	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: sdlcWorkflow,
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	contentRel := run.ContentPath("tele", "fix-it", "code")
	if err := os.WriteFile(filepath.Join(root, contentRel), []byte("# code\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := commitTurn(root, md, "code", 0); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}

	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	want := "work: update code\n\n" +
		"MoE-Run: fix-it\n" +
		"MoE-Project: tele\n" +
		"MoE-Workflow: " + sdlcWorkflow + "\n" +
		"MoE-Document: code\n" +
		"MoE-Session: " + md.Documents["code"].Session + "\n"
	if strings.TrimRight(body, "\n") != strings.TrimRight(want, "\n") {
		t.Fatalf("ordinary turn message drifted:\n got: %q\nwant: %q", body, want)
	}
}
