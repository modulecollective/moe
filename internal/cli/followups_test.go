package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// writeFollowups drops a followups.md alongside run.json without committing.
// The harvester reads from disk regardless of git state, and the close
// path's clean-tree gate ignores this one file.
func writeFollowups(t *testing.T, root, projectID, runID, body string) string {
	t.Helper()
	rel := run.FollowupsPath(projectID, runID)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return abs
}

// readFollowups returns the on-disk body of a run's followups.md.
func readFollowups(t *testing.T, root, projectID, runID string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.FollowupsPath(projectID, runID)))
	if err != nil {
		t.Fatalf("read followups.md: %v", err)
	}
	return string(body)
}

func TestParseFollowupsRoundtrip(t *testing.T) {
	body := []byte(strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo-helper` — Clean up foo helper",
		"- [x] `chase-zlib-upgrade` — Already harvested",
		"- [ ] `chase-it` — Chase it down",
		"",
	}, "\n"))
	lines, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected non-empty lines slice")
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 unchecked entries, got %d: %+v", len(todo), todo)
	}
	if todo[0].slug != "cleanup-foo-helper" || todo[0].title != "Clean up foo helper" {
		t.Errorf("first entry wrong: %+v", todo[0])
	}
	if todo[0].body != "" {
		t.Errorf("first entry should have no body, got %q", todo[0].body)
	}
	if todo[1].slug != "chase-it" {
		t.Errorf("second entry wrong: %+v", todo[1])
	}
}

func TestParseFollowupsCapturesBody(t *testing.T) {
	body := []byte(strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"",
		"  Why: bar/baz both reach into foo's internals; foo.go:42 is",
		"  the load-bearing assumption. Fix sketch: extract an accessor.",
		"",
		"- [ ] `chase-zlib` — Chase the zlib upgrade",
		"",
	}, "\n"))
	_, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(todo))
	}
	wantBody := "Why: bar/baz both reach into foo's internals; foo.go:42 is\n" +
		"the load-bearing assumption. Fix sketch: extract an accessor."
	if todo[0].body != wantBody {
		t.Errorf("body not dedented or trimmed correctly:\nwant: %q\n got: %q", wantBody, todo[0].body)
	}
	if todo[1].body != "" {
		t.Errorf("bare second entry should have empty body, got %q", todo[1].body)
	}
}

func TestParseFollowupsCapturesMultiParagraphBody(t *testing.T) {
	body := []byte(strings.Join([]string{
		"- [ ] `do-thing` — Do the thing",
		"",
		"  First paragraph.",
		"",
		"  Second paragraph with a `code-ish` token and an em-dash —.",
		"",
	}, "\n"))
	_, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(todo))
	}
	wantBody := "First paragraph.\n" +
		"\n" +
		"Second paragraph with a `code-ish` token and an em-dash —."
	if todo[0].body != wantBody {
		t.Errorf("multi-paragraph body wrong:\nwant: %q\n got: %q", wantBody, todo[0].body)
	}
}

func TestParseFollowupsBodyDedentsExactlyTwoSpaces(t *testing.T) {
	// A line indented four spaces should keep two spaces after the
	// dedent — that's content inside the body, not metadata to strip.
	body := []byte(strings.Join([]string{
		"- [ ] `nested` — Nested example",
		"",
		"  Outer paragraph.",
		"    inner-indented line",
		"",
	}, "\n"))
	_, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(todo))
	}
	wantBody := "Outer paragraph.\n  inner-indented line"
	if todo[0].body != wantBody {
		t.Errorf("indented body line under-dedented:\nwant: %q\n got: %q", wantBody, todo[0].body)
	}
}

func TestParseFollowupsClosedItemAbsorbsItsOwnBody(t *testing.T) {
	// A `[x]` item carries history that may include an indented body
	// from a prior harvest. The parser must NOT attribute that body to
	// the open item that came before it.
	body := []byte(strings.Join([]string{
		"- [ ] `live-one` — Live entry",
		"- [x] `dead-one` — Already harvested",
		"",
		"  Stale body that should not land on live-one.",
		"",
		"- [ ] `another` — Another live entry",
	}, "\n"))
	_, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(todo))
	}
	if todo[0].body != "" {
		t.Errorf("live-one inherited closed item's body: %q", todo[0].body)
	}
	if todo[1].slug != "another" || todo[1].body != "" {
		t.Errorf("second live entry wrong: %+v", todo[1])
	}
}

func TestParseFollowupsHeaderClosesItem(t *testing.T) {
	// A non-indented, non-checkbox line (header, prose, etc.) closes
	// the current item — so a header below an open item is not body.
	body := []byte(strings.Join([]string{
		"- [ ] `first` — First",
		"",
		"  Body of first.",
		"",
		"## Some other section",
		"",
		"- [ ] `second` — Second",
	}, "\n"))
	_, todo, err := parseFollowups(body)
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(todo))
	}
	if todo[0].body != "Body of first." {
		t.Errorf("first body wrong: %q", todo[0].body)
	}
	if todo[1].body != "" {
		t.Errorf("second body should be empty, got %q", todo[1].body)
	}
}

func TestParseFollowupsRejectsMalformed(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing slug quotes", "- [ ] cleanup-foo — Title\n", "malformed"},
		{"empty title", "- [ ] `slug` — \n", "title is empty"},
		{"hyphen instead of em-dash", "- [ ] `slug` - Title\n", "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseFollowups([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseFollowupsRejectsDuplicateSlug(t *testing.T) {
	body := strings.Join([]string{
		"- [ ] `dup` — First",
		"- [ ] `dup` — Second",
	}, "\n")
	_, _, err := parseFollowups([]byte(body))
	if err == nil {
		t.Fatal("expected duplicate slug error")
	}
	if !strings.Contains(err.Error(), "duplicates line") {
		t.Fatalf("expected duplicates-line error, got %q", err.Error())
	}
}

// TestSDLCCloseHarvestsFollowupBodyIntoIdeaCanvas pins the new
// title-plus-body shape of harvested ideas: an entry with an indented
// body produces an idea whose seed canvas is "# <Title>\n\n<body>\n",
// while a bodyless entry stays on the bare "# <Title>\n" default.
func TestSDLCCloseHarvestsFollowupBodyIntoIdeaCanvas(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"",
		"  Why: bar/baz reach into foo's internals; foo.go:42 is the",
		"  load-bearing assumption. Fix sketch: extract an accessor.",
		"",
		"- [ ] `bare-line` — A bare entry without a body",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	bodyCanvas, err := os.ReadFile(filepath.Join(root,
		"projects", "tele", "runs", "cleanup-foo", "documents", "idea", "content.md"))
	if err != nil {
		t.Fatalf("read body-bearing canvas: %v", err)
	}
	wantBodyCanvas := "# Clean up foo helper\n" +
		"\n" +
		"Why: bar/baz reach into foo's internals; foo.go:42 is the\n" +
		"load-bearing assumption. Fix sketch: extract an accessor.\n"
	if string(bodyCanvas) != wantBodyCanvas {
		t.Errorf("body-bearing idea canvas wrong:\nwant: %q\n got: %q", wantBodyCanvas, string(bodyCanvas))
	}

	bareCanvas, err := os.ReadFile(filepath.Join(root,
		"projects", "tele", "runs", "bare-line", "documents", "idea", "content.md"))
	if err != nil {
		t.Fatalf("read bare canvas: %v", err)
	}
	if string(bareCanvas) != "# A bare entry without a body\n" {
		t.Errorf("bare idea canvas should fall through to default H1, got %q", string(bareCanvas))
	}
}

func TestSDLCCloseHarvestsFollowups(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"- [ ] `chase-zlib` — Chase the zlib upgrade",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// Both ideas exist with the expected slugs and run.json status.
	for _, slug := range []string{"cleanup-foo", "chase-zlib"} {
		ideaJSON := filepath.Join(root, "projects", "tele", "runs", slug, "run.json")
		body, err := os.ReadFile(ideaJSON)
		if err != nil {
			t.Fatalf("idea %s/%s: %v", "tele", slug, err)
		}
		if !strings.Contains(string(body), `"workflow": "idea"`) {
			t.Fatalf("idea %s not flagged as idea workflow:\n%s", slug, body)
		}
		canvas := filepath.Join(root, "projects", "tele", "runs", slug, "documents", "idea", "content.md")
		cb, err := os.ReadFile(canvas)
		if err != nil {
			t.Fatalf("idea canvas %s: %v", slug, err)
		}
		if string(cb) == "" {
			t.Fatalf("idea %s canvas is empty", slug)
		}
	}

	// Each idea's open commit carries the MoE-From-Run trailer.
	logOut := gitLog(t, root, "--all", "--format=%s%n%b%n----")
	if !strings.Contains(logOut, "MoE-From-Run: tele/ship-it") {
		t.Fatalf("expected MoE-From-Run trailer on harvested idea commits:\n%s", logOut)
	}

	// followups.md lines are now `- [x]` with the resolved slug.
	got := readFollowups(t, root, "tele", "ship-it")
	for _, want := range []string{
		"- [x] `cleanup-foo` — Clean up foo helper",
		"- [x] `chase-zlib` — Chase the zlib upgrade",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in followups.md after harvest:\n%s", want, got)
		}
	}

	// The followups.md update rides along on the close commit.
	headBody := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(headBody, "Close sdlc run tele/ship-it") {
		t.Fatalf("HEAD not the close commit:\n%s", headBody)
	}
	headPaths := gitLog(t, root, "-1", "--name-only", "--format=")
	if !strings.Contains(headPaths, "followups.md") {
		t.Fatalf("close commit didn't include followups.md:\n%s", headPaths)
	}
}

func TestSDLCCloseAutoDisambiguatesSlugCollision(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	// Pre-create an idea named "foo" so the harvester has to bump.
	if code := Run([]string{"idea", "new", "tele/foo"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("seed idea creation failed")
	}

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `foo` — Foo follow-up\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// The pre-existing idea is at "foo"; the harvested one must land at "foo-2".
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "foo-2", "run.json")); err != nil {
		t.Fatalf("expected disambiguated idea foo-2: %v", err)
	}
	got := readFollowups(t, root, "tele", "ship-it")
	if !strings.Contains(got, "- [x] `foo-2` — Foo follow-up") {
		t.Fatalf("expected resolved slug in followups.md:\n%s", got)
	}
}

func TestSDLCCloseAbortsOnMalformedFollowup(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] not-quoted-slug — Bad\n")

	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on malformed followup, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "malformed follow-up") {
		t.Fatalf("expected malformed error, got: %q", errb.String())
	}
	afterHead := gitLog(t, root, "-1", "--format=%H")
	if beforeHead != afterHead {
		t.Fatalf("aborted close created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "ship-it", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status": "in_progress"`) {
		t.Fatalf("run.json status mutated under abort:\n%s", body)
	}
}

func TestSDLCCloseSkipsAlreadyCheckedLines(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"- [x] `already-done` — Captured earlier",
		"- [ ] `do-this-now` — Capture me",
	}, "\n")+"\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// Only the unchecked line should produce an idea.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "do-this-now", "run.json")); err != nil {
		t.Fatalf("expected idea do-this-now: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "already-done", "run.json")); !os.IsNotExist(err) {
		t.Fatalf("already-checked entry should not have been recreated: %v", err)
	}
}

// markerEditor installs a $EDITOR that records each invocation by
// appending to a marker file. The returned func reports the call count.
// Used to prove the harvest pre-flight skipped (or didn't skip) the pop
// without depending on side-effects from a real editor.
func markerEditor(t *testing.T) func() int {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\nprintf . >> '" + marker + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")
	return func() int {
		data, err := os.ReadFile(marker)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatal(err)
		}
		return len(data)
	}
}

// TestSDLCCloseSkipsEditorOnTrivialFollowups pins the design's core
// behaviour change: without --no-edit, the editor pop is gated on the
// file having at least one unchecked entry. Absent, header-only, and
// all-`[x]` files all skip the pop — there's nothing to review.
func TestSDLCCloseSkipsEditorOnTrivialFollowups(t *testing.T) {
	cases := []struct {
		name string
		// seed runs before close. nil means "leave followups.md absent."
		seed func(t *testing.T, root, projectID, runID string)
	}{
		{
			name: "absent",
			seed: nil,
		},
		{
			name: "header-only",
			seed: func(t *testing.T, root, projectID, runID string) {
				writeFollowups(t, root, projectID, runID, "# Follow-ups\n\n")
			},
		},
		{
			name: "all-checked",
			seed: func(t *testing.T, root, projectID, runID string) {
				writeFollowups(t, root, projectID, runID, strings.Join([]string{
					"# Follow-ups",
					"",
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
			code := Run([]string{"sdlc", "close", "tele/ship-it"}, &out, &errb)
			if code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if n := editorCalls(); n != 0 {
				t.Fatalf("expected zero editor invocations on trivial followups.md, got %d", n)
			}
		})
	}
}

// TestSDLCCloseOpensEditorWhenUnchecked is the positive companion to
// the trivial-skip test: an unchecked entry on disk means the operator
// still gets the pop before harvest fans out into ideas.
func TestSDLCCloseOpensEditorWhenUnchecked(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	editorCalls := markerEditor(t)

	writeFollowups(t, root, "tele", "ship-it",
		"- [ ] `chase-it` — Chase the thing\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if n := editorCalls(); n != 1 {
		t.Fatalf("expected exactly one editor invocation, got %d", n)
	}
}

// TestSDLCCloseOpensEditorOnMalformedUnchecked pins the design's
// tie-breaker: a malformed `- [ ]` line still trips the pop, so the
// operator can fix it in-editor rather than hit parseFollowups's hard
// error with no chance to recover.
func TestSDLCCloseOpensEditorOnMalformedUnchecked(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	editorCalls := markerEditor(t)

	// Hyphen instead of em-dash — parseFollowups rejects this, but the
	// shape-only gate must still pop the editor first.
	writeFollowups(t, root, "tele", "ship-it",
		"- [ ] `chase-it` - Chase the thing\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "tele/ship-it"}, &out, &errb)
	// The malformed line still trips parseFollowups after the pop; we
	// don't care about exit code here, only that the editor ran.
	_ = code
	_ = out
	_ = errb
	if n := editorCalls(); n != 1 {
		t.Fatalf("expected editor pop on malformed unchecked entry, got %d invocations", n)
	}
}

func TestSDLCCloseEmptyFollowupsIsClean(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// No followups.md on disk; --no-edit means we don't scaffold or open
	// the editor — close should succeed with no harvest side-effects.
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, run.FollowupsPath("tele", "ship-it"))); !os.IsNotExist(err) {
		t.Fatalf("expected no followups.md on disk after empty harvest: %v", err)
	}
}

func TestSDLCCloseTreatsFollowupsAsCleanForGate(t *testing.T) {
	// A modified-but-uncommitted followups.md is the normal state at close
	// (operator just appended a line). The clean-tree gate must let it
	// through, while still refusing on any other dirty path.
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `late-add` — Late entry\n")

	// First close: followups.md is dirty/untracked — should still succeed.
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected harvest to tolerate untracked followups.md, got exit=%d stderr=%q", code, errb.String())
	}

	// Now demonstrate the gate still trips on an unrelated dirty file.
	root2 := seedCloseFixture(t, "tele", "ship-it-2", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root2)
	if err := os.WriteFile(filepath.Join(root2, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	code = Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it-2"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected close to refuse on unrelated dirty file, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("expected dirty-tree refusal, got: %q", errb.String())
	}
}

// Sanity: the idea workflow's close still ignores followups.md entirely.
// Drop one on disk and confirm no idea got created from it.
func TestIdeaCloseDoesNotHarvestFollowups(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele/plain"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup capture failed")
	}

	writeFollowups(t, root, "tele", "plain", "- [ ] `should-not-harvest` — Nope\n")

	// Idea close ignores followups; we have to commit the file to keep
	// the (strict) clean-tree gate happy.
	gittest.Run(t, root, "add", run.FollowupsPath("tele", "plain"))
	gittest.Run(t, root, "commit", "-m", "stage followups for test")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele/plain"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "should-not-harvest", "run.json")); !os.IsNotExist(err) {
		t.Fatalf("idea close should not have harvested followups.md: %v", err)
	}
}
