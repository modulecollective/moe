package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// commitWikiTurn is the shared per-turn commit for the two wiki-
// attached out-of-band sessions (reflect, lint). The behaviour
// it owns is: stage the wiki content dir; conditionally stage the
// per-run canvas if the agent wrote one; commit both in a single
// `work: <docID> pass <slug>` commit with the right trailers.
// Reflect writes a canvas; lint doesn't, and the helper's
// os.Stat-skip is what keeps that case wiki-only. One parameterised
// test pins both.
func TestCommitWikiTurn(t *testing.T) {
	cases := []struct {
		docID       string
		runSlug     string
		writeCanvas bool
	}{
		// Reflect instructs the agent (in its kickoff) to drop a
		// per-pass record at canvasRel; the helper must stage it
		// alongside the wiki edits so the session-close gate sees a
		// non-empty canvas at the branch tip — without this, the gate
		// refuses to fast-forward main (the original bug).
		{docID: "reflect", runSlug: "reflect-2026-05-11-120000", writeCanvas: true},
		// Lint never writes a canvas; the os.Stat skip in commitWikiTurn
		// is what keeps the commit wiki-only. Pin that branch here —
		// it had no dedicated test before the helper collapse.
		{docID: "lint", runSlug: "lint-2026-05-10-120000", writeCanvas: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.docID, func(t *testing.T) {
			root := newTestBureaucracy(t)

			twinDir := wiki.TwinDir(root, "tele")
			if err := os.MkdirAll(twinDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n\nupdated.\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			canvasRel := run.ContentPath("tele", tc.runSlug, tc.docID)
			if tc.writeCanvas {
				canvasPath := filepath.Join(root, canvasRel)
				if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(canvasPath, []byte("per-pass record.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			wikiRel, err := filepath.Rel(root, twinDir)
			if err != nil {
				t.Fatal(err)
			}

			if err := commitWikiTurn(root, "twin", "tele", tc.runSlug, tc.docID, wikiRel); err != nil {
				t.Fatalf("commitWikiTurn: %v", err)
			}

			names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
			wantPaths := []string{filepath.Join(wikiRel, "vision.md")}
			if tc.writeCanvas {
				wantPaths = append(wantPaths, canvasRel)
			}
			for _, want := range wantPaths {
				if !strings.Contains(names, want) {
					t.Errorf("commit missing %q in:\n%s", want, names)
				}
			}
			if !tc.writeCanvas && strings.Contains(names, canvasRel) {
				t.Errorf("commit unexpectedly staged absent canvas %q in:\n%s", canvasRel, names)
			}

			subject := gittest.Output(t, root, "log", "-1", "--pretty=%s")
			wantSubject := "work: " + tc.docID + " pass " + tc.runSlug
			if strings.TrimSpace(subject) != wantSubject {
				t.Errorf("commit subject = %q, want %q", strings.TrimSpace(subject), wantSubject)
			}
		})
	}
}

// TestProjectCommitDirsPerWorkflow pins the whitelist: sdlc stages
// carry both hooks/ and chores/ edits, the dedicated workflows carry
// only their own dir, and everything else (twin, kb, chat, …) carries
// nothing — a twin edit must reach the twin through feedback/twin.md,
// not through a stage commit.
func TestProjectCommitDirsPerWorkflow(t *testing.T) {
	cases := []struct {
		workflow string
		want     []string
	}{
		{sdlcWorkflow, []string{"hooks", "chores"}},
		{hooksWorkflow, []string{"hooks"}},
		{choresWorkflow, []string{"chores"}},
		{"twin", nil},
		{"kb", nil},
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
	if err := commitTurn(root, md, "code", extras...); err != nil {
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
