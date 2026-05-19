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

// writeLoreFeedback drops a feedback/lore.md alongside run.json
// without committing. The harvester reads from disk regardless of git
// state, and the close path's clean-tree gate ignores this file (same
// exception followups.md gets).
func writeLoreFeedback(t *testing.T, root, projectID, runID, body string) string {
	t.Helper()
	rel := run.FeedbackPath(projectID, runID, "lore")
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return abs
}

func readLoreFeedback(t *testing.T, root, projectID, runID string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.FeedbackPath(projectID, runID, "lore")))
	if err != nil {
		t.Fatalf("read feedback/lore.md: %v", err)
	}
	return string(body)
}

func readLoreEntry(t *testing.T, root, slug string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, wiki.LoreDirRel, slug+".md"))
	if err != nil {
		t.Fatalf("read lore/%s.md: %v", slug, err)
	}
	return string(body)
}

func TestParseLoreRoundtrip(t *testing.T) {
	body := []byte(strings.Join([]string{
		"# Lore captured this run",
		"",
		"- [ ] `compose-binds` — Reaching compose ports",
		"- [x] `already-promoted` — Already done",
		"- [ ] `another-fact` — Another portable fact",
		"",
	}, "\n"))
	lines, todo, err := parseLore(body)
	if err != nil {
		t.Fatalf("parseLore: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected non-empty lines slice")
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 unchecked entries, got %d: %+v", len(todo), todo)
	}
	if todo[0].slug != "compose-binds" || todo[0].title != "Reaching compose ports" {
		t.Errorf("first entry wrong: %+v", todo[0])
	}
	if todo[1].slug != "another-fact" {
		t.Errorf("second entry wrong: %+v", todo[1])
	}
}

func TestParseLoreCapturesAppliesWhen(t *testing.T) {
	body := []byte(strings.Join([]string{
		"- [ ] `slug-a` — Title A",
		"",
		"  applies-when: project uses docker-compose on fly + tailscale",
		"",
		"  Body prose explaining the fact.",
		"",
	}, "\n"))
	_, todo, err := parseLore(body)
	if err != nil {
		t.Fatalf("parseLore: %v", err)
	}
	if len(todo) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(todo))
	}
	wantWhen := "project uses docker-compose on fly + tailscale"
	if todo[0].appliesWhen != wantWhen {
		t.Errorf("appliesWhen wrong:\nwant: %q\n got: %q", wantWhen, todo[0].appliesWhen)
	}
	if todo[0].body != "Body prose explaining the fact." {
		t.Errorf("body wrong: %q", todo[0].body)
	}
}

func TestParseLoreCapturesMultiLineAppliesWhen(t *testing.T) {
	body := []byte(strings.Join([]string{
		"- [ ] `slug-a` — Title A",
		"",
		"  applies-when: project uses docker-compose on a fly-box reached",
		"  via tailscale, with no fly.toml services",
		"",
		"  The fact body.",
		"",
	}, "\n"))
	_, todo, err := parseLore(body)
	if err != nil {
		t.Fatalf("parseLore: %v", err)
	}
	if len(todo) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(todo))
	}
	wantWhen := "project uses docker-compose on a fly-box reached via tailscale, with no fly.toml services"
	if todo[0].appliesWhen != wantWhen {
		t.Errorf("multi-line applies-when not joined:\nwant: %q\n got: %q", wantWhen, todo[0].appliesWhen)
	}
	if todo[0].body != "The fact body." {
		t.Errorf("body after multi-line applies-when wrong: %q", todo[0].body)
	}
}

func TestParseLoreFallsBackWhenAppliesWhenMissing(t *testing.T) {
	// Body without an applies-when line: the harvester promotes the
	// entry but the renderer falls back to "(missing)" in the
	// frontmatter — the in-prompt index shows the same placeholder,
	// so the operator notices and fixes it in lore/.
	body := []byte(strings.Join([]string{
		"- [ ] `bodyless` — Bodyless lore",
		"- [ ] `body-no-when` — Body but no applies-when",
		"",
		"  Just some prose without the heuristic line.",
		"",
	}, "\n"))
	_, todo, err := parseLore(body)
	if err != nil {
		t.Fatalf("parseLore: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(todo))
	}
	if todo[0].appliesWhen != "" || todo[0].body != "" {
		t.Errorf("bodyless entry should yield empty applies-when/body, got %+v", todo[0])
	}
	if todo[1].appliesWhen != "" {
		t.Errorf("body-without-applies-when should yield empty applies-when, got %q", todo[1].appliesWhen)
	}
	if todo[1].body != "Just some prose without the heuristic line." {
		t.Errorf("body-without-applies-when prose lost: %q", todo[1].body)
	}
}

func TestParseLoreRejectsMalformed(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing slug quotes", "- [ ] slug-x — Title\n", "malformed"},
		{"empty title", "- [ ] `slug` — \n", "title is empty"},
		{"hyphen instead of em-dash", "- [ ] `slug` - Title\n", "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseLore([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseLoreRejectsDuplicateSlug(t *testing.T) {
	body := strings.Join([]string{
		"- [ ] `dup` — First",
		"- [ ] `dup` — Second",
	}, "\n")
	_, _, err := parseLore([]byte(body))
	if err == nil {
		t.Fatal("expected duplicate slug error")
	}
	if !strings.Contains(err.Error(), "duplicates line") {
		t.Fatalf("expected duplicates-line error, got %q", err.Error())
	}
}

// TestRenderLoreFileShape pins the on-disk shape of a promoted lore
// entry: frontmatter / blank / H1 / blank / prose.
func TestRenderLoreFileShape(t *testing.T) {
	got := renderLoreFile("My Title", "applies hint", "tele/runs/foo", "Body prose line.")
	want := strings.Join([]string{
		"---",
		"title: My Title",
		"applies-when: applies hint",
		"discovered-in: tele/runs/foo",
		"---",
		"",
		"# My Title",
		"",
		"Body prose line.",
		"",
	}, "\n")
	if got != want {
		t.Errorf("renderLoreFile shape wrong:\nwant: %q\n got: %q", want, got)
	}
}

func TestRenderLoreFileSubstitutesMissingAppliesWhen(t *testing.T) {
	got := renderLoreFile("T", "", "p/runs/r", "prose")
	if !strings.Contains(got, "applies-when: (missing)\n") {
		t.Errorf("expected (missing) placeholder, got:\n%s", got)
	}
}

func TestSDLCCloseHarvestsLore(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"# feedback/lore.md",
		"",
		"- [ ] `compose-binds` — Reaching compose ports",
		"",
		"  applies-when: project uses docker-compose on a fly-box reached via tailscale",
		"",
		"  Bind to 127.0.0.1:HOST:CONTAINER and reach with tailscale ssh -L.",
		"",
		"- [ ] `bareish` — Bare-ish entry",
		"",
		"  applies-when: only when you forget the body",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	got := readLoreEntry(t, root, "compose-binds")
	for _, want := range []string{
		"title: Reaching compose ports",
		"applies-when: project uses docker-compose on a fly-box reached via tailscale",
		"discovered-in: tele/runs/ship-it",
		"# Reaching compose ports",
		"Bind to 127.0.0.1:HOST:CONTAINER",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lore/compose-binds.md missing %q:\n%s", want, got)
		}
	}

	// The feedback file now carries `- [x]` lines with the resolved slug.
	feedback := readLoreFeedback(t, root, "tele", "ship-it")
	for _, want := range []string{
		"- [x] `compose-binds` — Reaching compose ports",
		"- [x] `bareish` — Bare-ish entry",
	} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("expected %q in feedback/lore.md after harvest:\n%s", want, feedback)
		}
	}

	// The close commit rolled feedback/lore.md and the new lore/ entries up.
	headPaths := gitLog(t, root, "-1", "--name-only", "--format=")
	if !strings.Contains(headPaths, "feedback/lore.md") {
		t.Errorf("close commit didn't include feedback/lore.md:\n%s", headPaths)
	}
	if !strings.Contains(headPaths, "lore/compose-binds.md") {
		t.Errorf("close commit didn't include lore/compose-binds.md:\n%s", headPaths)
	}
}

// TestSDLCCloseAutoDisambiguatesLoreCollision pins the -2, -3 collision
// policy: an entry whose slug already exists in lore/ lands as
// <slug>-2, and the resolved slug appears in the rewritten checklist.
func TestSDLCCloseAutoDisambiguatesLoreCollision(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Pre-seed lore/foo.md (and commit it, so the clean-tree gate
	// doesn't refuse on the seed) so the harvester has to bump.
	loreDir := filepath.Join(root, wiki.LoreDirRel)
	if err := os.MkdirAll(loreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loreDir, "foo.md"),
		[]byte("---\ntitle: Pre-existing\napplies-when: prior\n---\n\n# Pre-existing\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", filepath.Join(wiki.LoreDirRel, "foo.md"))
	gittest.Run(t, root, "commit", "-m", "seed lore/foo.md for collision test")

	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `foo` — New fact",
		"",
		"  applies-when: when the second foo lands",
		"",
		"  Body prose.",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	if _, err := os.Stat(filepath.Join(loreDir, "foo-2.md")); err != nil {
		t.Fatalf("expected lore/foo-2.md, got %v", err)
	}
	feedback := readLoreFeedback(t, root, "tele", "ship-it")
	if !strings.Contains(feedback, "- [x] `foo-2` — New fact") {
		t.Fatalf("expected resolved slug in feedback/lore.md:\n%s", feedback)
	}
}

func TestSDLCCloseAbortsOnMalformedLore(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeLoreFeedback(t, root, "tele", "ship-it", "- [ ] no-quotes-slug — Bad\n")

	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on malformed lore, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "malformed lore") {
		t.Fatalf("expected malformed error, got: %q", errb.String())
	}
	afterHead := gitLog(t, root, "-1", "--format=%H")
	if beforeHead != afterHead {
		t.Fatalf("aborted close created a commit:\nbefore=%s\nafter=%s", beforeHead, afterHead)
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "ship-it", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status": "in_progress"`) {
		t.Fatalf("run.json status mutated under abort:\n%s", body)
	}
}

func TestSDLCCloseSkipsAlreadyCheckedLore(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [x] `prior-fact` — Already promoted",
		"- [ ] `new-fact` — Capture me",
		"",
		"  applies-when: now",
		"",
		"  The body.",
	}, "\n")+"\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	loreDir := filepath.Join(root, wiki.LoreDirRel)
	if _, err := os.Stat(filepath.Join(loreDir, "new-fact.md")); err != nil {
		t.Fatalf("expected lore/new-fact.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(loreDir, "prior-fact.md")); !os.IsNotExist(err) {
		t.Fatalf("already-checked entry should not have been re-promoted: %v", err)
	}
}

func TestSDLCCloseOpensEditorWhenLoreUnchecked(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	editorCalls := markerEditor(t)

	writeLoreFeedback(t, root, "tele", "ship-it",
		"- [ ] `fact` — A portable fact\n\n  applies-when: now\n\n  body\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "tele", "ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Followups absent → no followups pop. Lore present → one pop.
	if n := editorCalls(); n != 1 {
		t.Fatalf("expected exactly one editor invocation for lore, got %d", n)
	}
}

func TestSDLCCloseSkipsEditorOnTrivialLore(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, root, projectID, runID string)
	}{
		{name: "absent", seed: nil},
		{
			name: "header-only",
			seed: func(t *testing.T, root, projectID, runID string) {
				writeLoreFeedback(t, root, projectID, runID, "# feedback/lore.md\n\n")
			},
		},
		{
			name: "all-checked",
			seed: func(t *testing.T, root, projectID, runID string) {
				writeLoreFeedback(t, root, projectID, runID, strings.Join([]string{
					"- [x] `did-this` — Already harvested",
					"- [x] `did-that` — Also already harvested",
					"",
				}, "\n"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			editorCalls := markerEditor(t)

			if tc.seed != nil {
				tc.seed(t, root, "tele", "ship-it")
			}

			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", "close", "tele", "ship-it"}, &out, &errb)
			if code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if n := editorCalls(); n != 0 {
				t.Fatalf("expected zero editor invocations on trivial lore, got %d", n)
			}
		})
	}
}

func TestSDLCCloseTreatsLoreFileAsCleanForGate(t *testing.T) {
	// A modified-but-uncommitted feedback/lore.md is the normal state
	// at close (an agent or operator just appended a line). The
	// clean-tree gate must let it through, while still refusing on any
	// unrelated dirty path.
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeLoreFeedback(t, root, "tele", "ship-it",
		"- [ ] `late-add` — Late entry\n\n  applies-when: now\n\n  body\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected close to tolerate untracked feedback/lore.md, got exit=%d stderr=%q",
			code, errb.String())
	}

	// Now demonstrate the gate still trips on an unrelated dirty file.
	root2 := seedCloseFixture(t, "tele", "ship-it-2", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root2)
	if err := os.WriteFile(filepath.Join(root2, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	code = Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it-2"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected close to refuse on unrelated dirty file, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree refusal, got: %q", errb.String())
	}
}

func TestInjectEditorPopHeaderInjectsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	if err := os.WriteFile(path, []byte("- [ ] `x` — Y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectEditorPopHeader(path, loreHeader); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("<!--")) {
		t.Fatalf("expected leading <!-- after inject:\n%s", got)
	}
	if !bytes.Contains(got, []byte("- [ ] `x` — Y")) {
		t.Fatalf("inject dropped original body:\n%s", got)
	}
}

func TestInjectEditorPopHeaderNoOpWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	original := "<!-- previously injected -->\n\n- [ ] `x` — Y\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectEditorPopHeader(path, loreHeader); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("expected no-op on file with existing comment:\nwant: %q\n got: %q",
			original, string(got))
	}
}

func TestInjectEditorPopHeaderAbsentFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.md")
	if err := injectEditorPopHeader(path, loreHeader); err != nil {
		t.Fatalf("inject on absent: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("inject on absent file should not create it: %v", err)
	}
}

func TestInjectEditorPopHeaderEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectEditorPopHeader(path, loreHeader); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("<!--")) {
		t.Fatalf("expected header on empty file:\n%s", got)
	}
}
