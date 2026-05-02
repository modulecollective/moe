package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
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
		"  - [ ] `nested-not-allowed` — Should still parse",
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
	if todo[1].slug != "nested-not-allowed" {
		t.Errorf("indented entry not parsed: %+v", todo[1])
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
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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
	if code := Run([]string{"idea", "new", "tele", "foo"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("seed idea creation failed")
	}

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `foo` — Foo follow-up\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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

func TestSDLCCloseEmptyFollowupsIsClean(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// No followups.md on disk; --no-edit means we don't scaffold or open
	// the editor — close should succeed with no harvest side-effects.
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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
	code := Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it"}, &out, &errb)
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
	code = Run([]string{"sdlc", "close", "--no-edit", "tele", "ship-it-2"}, &out, &errb)
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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	if code := Run([]string{"idea", "new", "tele", "Plain"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("setup capture failed")
	}

	writeFollowups(t, root, "tele", "plain", "- [ ] `should-not-harvest` — Nope\n")

	// Idea close ignores followups; we have to commit the file to keep
	// the (strict) clean-tree gate happy.
	addCmd := exec.Command("git", "-C", root, "add",
		run.FollowupsPath("tele", "plain"))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commit := exec.Command("git", "-C", root, "commit", "-m", "stage followups for test")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele", "plain"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "should-not-harvest", "run.json")); !os.IsNotExist(err) {
		t.Fatalf("idea close should not have harvested followups.md: %v", err)
	}
}
