package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// mintHead opens a chain head and returns its slug.
func mintHead(t *testing.T, root string, args ...string) string {
	t.Helper()
	if code := runChainNew(args, io.Discard, os.Stderr); code != 0 {
		t.Fatalf("chain new %v failed", args)
	}
	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain heads = %v, want 1", heads)
	}
	return heads[0]
}

func readNote(t *testing.T, root, slug string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", slug, chainDoc)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestChainNewIsFastByDefault: minting a head must not pop an editor
// and must not require prose. The purpose note is worth having, but
// making it mandatory would tax the one verb whose job is to be cheap
// enough to reach for.
func TestChainNewIsFastByDefault(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	noEditor(t)

	slug := mintHead(t, root, "moe/perf-cleanups")
	if got := readNote(t, root, slug); !strings.Contains(got, "# Chain") {
		t.Errorf("note missing heading:\n%s", got)
	}
}

// TestChainNewSeedWritesTheNote: --seed is the opt-in editor at mint.
// The workflow-generic --seed seeds a first stage; a chain head has no
// stages, so chain's seeds the purpose note instead.
func TestChainNewSeedWritesTheNote(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	writingEditor(t, "Everything the profiler flagged in June.")

	slug := mintHead(t, root, "--seed", "moe/perf-cleanups")
	got := readNote(t, root, slug)
	if !strings.Contains(got, "Everything the profiler flagged in June") {
		t.Errorf("seeded note not on disk:\n%s", got)
	}
	if !isCleanTree(t, root) {
		t.Error("chain new --seed should leave a clean tree")
	}
}

// TestChainNewSeedRefusals: --seed needs an editor, and an operator who
// pops one and types nothing meant to back out — minting a head with an
// empty note is what plain `moe chain new` is for.
func TestChainNewSeedRefusals(t *testing.T) {
	t.Run("no editor", func(t *testing.T) {
		root := spawnFixture(t)
		t.Chdir(root)
		noEditor(t)
		var errb bytes.Buffer
		if code := runChainNew([]string{"--seed", "moe/perf"}, io.Discard, &errb); code == 0 {
			t.Fatal("want refusal with no editor configured")
		}
		if !strings.Contains(errb.String(), "EDITOR") {
			t.Errorf("error should name $EDITOR, got %q", errb.String())
		}
		if heads := runsWithWorkflow(t, root, "moe", chainWorkflow); len(heads) != 0 {
			t.Errorf("refused mint left heads %v behind", heads)
		}
	})

	t.Run("note unchanged", func(t *testing.T) {
		root := spawnFixture(t)
		t.Chdir(root)
		stubEditor(t) // `true` — leaves the stub exactly as seeded
		var errb bytes.Buffer
		if code := runChainNew([]string{"--seed", "moe/perf"}, io.Discard, &errb); code == 0 {
			t.Fatal("want refusal when the note comes back unchanged")
		}
		if heads := runsWithWorkflow(t, root, "moe", chainWorkflow); len(heads) != 0 {
			t.Errorf("aborted mint left heads %v behind", heads)
		}
	})
}

// TestChainNoteEditsAndCommits: `moe chain note` is the CLI edit path
// for the purpose note — $EDITOR on the file in place, then one
// trailered commit, same shape as `moe idea edit`.
func TestChainNoteEditsAndCommits(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	noEditor(t)
	slug := mintHead(t, root, "moe/perf-cleanups")

	writingEditor(t, "The June profiler batch.")
	var out, errb bytes.Buffer
	if code := runChainNote([]string{"moe/" + slug}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := readNote(t, root, slug); !strings.Contains(got, "The June profiler batch") {
		t.Errorf("note edit not on disk:\n%s", got)
	}
	if !isCleanTree(t, root) {
		t.Error("chain note should commit its edit")
	}
	subject, err := git.Output(root, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(subject) != "work: update "+chainDoc {
		t.Errorf("commit subject = %q, want the doc work-turn shape", strings.TrimSpace(subject))
	}
	body, err := git.Output(root, "log", "-1", "--format=%b")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MoE-Run: " + slug, "MoE-Project: moe", "MoE-Document: " + chainDoc} {
		if !strings.Contains(body, want) {
			t.Errorf("commit body missing %q:\n%s", want, body)
		}
	}
}

// TestChainNoteRefusesNonChainRun: the note is a chain head's one doc.
// Pointing the verb at an sdlc run must say so rather than fail later
// on a canvas that was never going to be there.
func TestChainNoteRefusesNonChainRun(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	stubEditor(t)
	if err := run.Save(root, &run.Metadata{
		ID: "fix-it", Project: "moe", Status: run.StatusInProgress,
		Workflow: "sdlc", Created: "2026-07-19", Documents: map[string]*run.Document{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.StageAndCommit(root, "seed a non-chain run", "."); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := runChainNote([]string{"moe/fix-it"}, io.Discard, &errb); code == 0 {
		t.Fatal("want refusal on a non-chain run")
	}
	if !strings.Contains(errb.String(), "only chain heads") {
		t.Errorf("error should say why, got %q", errb.String())
	}
}

// TestChainNoteUnchangedIsNotAnError: saving without editing reports
// unchanged and exits 0 — same three-way outcome `moe idea edit` has.
func TestChainNoteUnchangedIsNotAnError(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	noEditor(t)
	slug := mintHead(t, root, "moe/perf-cleanups")

	stubEditor(t)
	var out, errb bytes.Buffer
	if code := runChainNote([]string{"moe/" + slug}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("stdout should report unchanged, got %q", out.String())
	}
}

func isCleanTree(t *testing.T, root string) bool {
	t.Helper()
	st, err := git.Status(root)
	if err != nil {
		t.Fatal(err)
	}
	return len(st) == 0
}
