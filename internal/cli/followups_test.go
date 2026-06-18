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
	lines, todo, err := parseFollowups(body, "tele")
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
	_, todo, err := parseFollowups(body, "tele")
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
	_, todo, err := parseFollowups(body, "tele")
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
	_, todo, err := parseFollowups(body, "tele")
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
	_, todo, err := parseFollowups(body, "tele")
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
	_, todo, err := parseFollowups(body, "tele")
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
			_, _, err := parseFollowups([]byte(tc.body), "tele")
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseFollowupsRejectsStrayContent pins Change 2's backstop: a
// file with substantive content but no `- [ ]` checkbox fails loud
// instead of silently no-opping. This is the population that loses
// ideas today — an agent that wrote prose or plain bullets instead of
// the checklist grammar.
func TestParseFollowupsRejectsStrayContent(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"plain bullets", "- clean up the foo helper\n- chase the zlib upgrade\n"},
		{"prose", "We should clean up the foo helper and chase zlib.\n"},
		{"prose under a heading", "# Follow-ups\n\nClean up the foo helper.\n"},
		{"plain bullets after the editor-pop header", "<!--\nfollowups.md — captured this run.\nDelete a line to skip.\n-->\n\n- clean up foo\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, todo, err := parseFollowups([]byte(tc.body), "tele")
			if err == nil {
				t.Fatalf("expected stray-content error, got nil (todo=%+v)", todo)
			}
			if !strings.Contains(err.Error(), "has content but no") || !strings.Contains(err.Error(), "wrong format") {
				t.Fatalf("error %q missing the stray-content phrasing", err.Error())
			}
		})
	}
}

// TestParseFollowupsCleanNoOps pins the negative side of the backstop:
// the legitimate "nothing to harvest" shapes must NOT trip the guard.
// An empty / header-only / heading-only file, and a fully-harvested
// file (all `- [x]`, including ones carrying indented audit bodies),
// each parse to zero entries with no error.
func TestParseFollowupsCleanNoOps(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"empty", ""},
		{"blank lines only", "\n\n\n"},
		{"heading only", "# Follow-ups\n\n"},
		{"editor-pop header only", "<!--\nfollowups.md — captured this run.\nDelete a line to skip.\n-->\n"},
		{"all checked", "- [x] `did-this` — Done\n- [x] `did-that` — Also done\n"},
		{
			"all checked with indented bodies",
			strings.Join([]string{
				"# Follow-ups",
				"",
				"- [x] `did-this` — Done",
				"",
				"  Why: the body that rode into the idea on a prior harvest.",
				"",
				"- [x] `did-that` — Also done",
				"",
			}, "\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, todo, err := parseFollowups([]byte(tc.body), "tele")
			if err != nil {
				t.Fatalf("expected clean no-op, got error: %v", err)
			}
			if len(todo) != 0 {
				t.Fatalf("expected zero entries, got %d: %+v", len(todo), todo)
			}
		})
	}
}

// TestParseFollowupsStrayGuardYieldsToValidEntries pins the guard's
// precondition: it fires only when the parse yielded *zero* open
// entries. Stray prose alongside a valid `- [ ]` entry is tolerated —
// the entry harvests and the loose line rides along untouched.
func TestParseFollowupsStrayGuardYieldsToValidEntries(t *testing.T) {
	body := strings.Join([]string{
		"# Follow-ups",
		"",
		"some loose prose the agent left above the list",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"",
	}, "\n")
	_, todo, err := parseFollowups([]byte(body), "tele")
	if err != nil {
		t.Fatalf("stray line alongside a valid entry should not error: %v", err)
	}
	if len(todo) != 1 || todo[0].slug != "cleanup-foo" {
		t.Fatalf("expected the one valid entry, got %+v", todo)
	}
}

// TestParseFollowupsIndentedOrphanIsNotStray names a code-stage choice
// the design left open (design "Indented orphan lines"): a purely
// indented line with no preceding checkbox is NOT counted as stray.
// Treating it as stray would false-positive on the indented audit
// bodies of all-`[x]` files (those reach the same openIdx<0 indented
// branch), so the guard targets only non-indented substantive lines.
func TestParseFollowupsIndentedOrphanIsNotStray(t *testing.T) {
	body := "  an indented line attached to nothing\n"
	_, todo, err := parseFollowups([]byte(body), "tele")
	if err != nil {
		t.Fatalf("indented orphan should be a clean no-op, got: %v", err)
	}
	if len(todo) != 0 {
		t.Fatalf("expected zero entries, got %+v", todo)
	}
}

// TestParseLoreInheritsStrayGuard pins the intended shared-parser
// consequence: parseChecklist is shared, so lore prose-without-
// checkboxes fails loud the same way, with lore's noun in the message.
func TestParseLoreInheritsStrayGuard(t *testing.T) {
	_, _, err := parseLore([]byte("just some lore prose with no checkbox\n"))
	if err == nil {
		t.Fatal("expected stray-content error from the shared parser")
	}
	if !strings.Contains(err.Error(), "has content but no") || !strings.Contains(err.Error(), "lore entry") {
		t.Fatalf("lore error should carry the lore noun, got: %q", err.Error())
	}
}

func TestParseFollowupsRejectsDuplicateSlug(t *testing.T) {
	body := strings.Join([]string{
		"- [ ] `dup` — First",
		"- [ ] `dup` — Second",
	}, "\n")
	_, _, err := parseFollowups([]byte(body), "tele")
	if err == nil {
		t.Fatal("expected duplicate slug error")
	}
	if !strings.Contains(err.Error(), "duplicates line") {
		t.Fatalf("expected duplicates-line error, got %q", err.Error())
	}
}

// TestParseFollowupsSplitsProjectPrefix pins the routing split: a
// `<project>/slug` entry resolves project from the prefix and slug from
// the tail, while a bare slug routes to the current project. The raw
// slug is preserved verbatim either way (it's the audit-line + dedup
// key).
func TestParseFollowupsSplitsProjectPrefix(t *testing.T) {
	body := strings.Join([]string{
		"- [ ] `claudia/inherit-nginx` — Claudia inherits nginx identity",
		"- [ ] `local-cleanup` — Stays in the current project",
	}, "\n")
	_, todo, err := parseFollowups([]byte(body), "tele")
	if err != nil {
		t.Fatalf("parseFollowups: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(todo))
	}
	prefixed := todo[0]
	if prefixed.project != "claudia" || prefixed.slug != "inherit-nginx" || prefixed.rawSlug != "claudia/inherit-nginx" {
		t.Errorf("prefixed entry split wrong: %+v", prefixed)
	}
	bare := todo[1]
	if bare.project != "tele" || bare.slug != "local-cleanup" || bare.rawSlug != "local-cleanup" {
		t.Errorf("bare entry should route to current project: %+v", bare)
	}
}

// TestParseFollowupsCrossProjectSlugsAreDistinct guards the dedup key:
// the same bare slug under two different projects is two distinct
// entries (no collision), while two identical prefixed slugs still
// collide. The dedup keys off the raw line text, so this falls out of
// parseChecklist's existing `seen` map for free.
func TestParseFollowupsCrossProjectSlugsAreDistinct(t *testing.T) {
	body := strings.Join([]string{
		"- [ ] `claudia/foo` — Foo for claudia",
		"- [ ] `westworld/foo` — Foo for westworld",
	}, "\n")
	_, todo, err := parseFollowups([]byte(body), "tele")
	if err != nil {
		t.Fatalf("cross-project same-slug should not collide: %v", err)
	}
	if len(todo) != 2 {
		t.Fatalf("expected 2 distinct entries, got %d", len(todo))
	}

	dup := strings.Join([]string{
		"- [ ] `claudia/foo` — First",
		"- [ ] `claudia/foo` — Second",
	}, "\n")
	if _, _, err := parseFollowups([]byte(dup), "tele"); err == nil || !strings.Contains(err.Error(), "duplicates line") {
		t.Fatalf("identical prefixed slugs should still collide, got %v", err)
	}
}

// TestSDLCCloseRoutesPrefixedFollowupToTargetProject is the end-to-end
// routing case: a `claudia/…` entry lands an idea under claudia (not the
// closing run's tele), a bare entry stays in tele, the audit lines keep
// each entry's original shape, and the cross-project idea's open commit
// still records its provenance as the tele source run.
func TestSDLCCloseRoutesPrefixedFollowupToTargetProject(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedProject(t, root, "claudia")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `claudia/inherit-nginx` — Claudia inherits nginx identity",
		"- [ ] `local-cleanup` — Stays local",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// Routed idea landed under claudia, bare idea under tele.
	if _, err := os.Stat(filepath.Join(root, "projects", "claudia", "runs", "inherit-nginx", "run.json")); err != nil {
		t.Fatalf("expected idea routed to claudia: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "local-cleanup", "run.json")); err != nil {
		t.Fatalf("expected bare idea under tele: %v", err)
	}
	// And NOT a stray inherit-nginx under tele.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "inherit-nginx", "run.json")); !os.IsNotExist(err) {
		t.Fatalf("prefixed idea should not also land in the current project: %v", err)
	}

	// Audit lines: prefix preserved for the routed entry, bare for the local one.
	got := readFollowups(t, root, "tele", "ship-it")
	for _, want := range []string{
		"- [x] `claudia/inherit-nginx` — Claudia inherits nginx identity",
		"- [x] `local-cleanup` — Stays local",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in followups.md after harvest:\n%s", want, got)
		}
	}

	// Provenance points at the source run, not the destination project.
	logOut := gitLog(t, root, "--all", "--format=%s%n%b%n----")
	if !strings.Contains(logOut, "MoE-From-Run: tele/ship-it") {
		t.Fatalf("expected MoE-From-Run trailer pointing at the source run:\n%s", logOut)
	}
}

// TestSDLCCloseHarvestPreservesPrefixThroughDisambiguation pins the
// audit-line shape when a routed slug collides at its destination: the
// idea bumps to `-2`, and the rewritten line carries the prefix plus the
// resolved id (`claudia/taken-2`), so the trail records where it landed.
func TestSDLCCloseHarvestPreservesPrefixThroughDisambiguation(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedProject(t, root, "claudia")
	// Pre-existing idea at claudia/taken forces the harvester to bump.
	trailerstest.SeedRun(t, root, "claudia", "taken", "idea", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `claudia/taken` — Routed but colliding\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	if _, err := os.Stat(filepath.Join(root, "projects", "claudia", "runs", "taken-2", "run.json")); err != nil {
		t.Fatalf("expected disambiguated idea claudia/taken-2: %v", err)
	}
	got := readFollowups(t, root, "tele", "ship-it")
	if !strings.Contains(got, "- [x] `claudia/taken-2` — Routed but colliding") {
		t.Fatalf("expected prefix-preserving resolved slug in followups.md:\n%s", got)
	}
}

// TestSDLCCloseRejectsUnknownTargetProject pins the upfront-and-total
// validation contract: a prefix naming an unregistered project fails the
// whole harvest with a 1-based line number, before any idea is created —
// including the valid bare entry that follows it.
func TestSDLCCloseRejectsUnknownTargetProject(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `nope/thing` — Targets a project that isn't registered",
		"- [ ] `would-be-fine` — A valid bare entry that must not be harvested",
		"",
	}, "\n"))

	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on unknown target project, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not registered") || !strings.Contains(errb.String(), "line 1") {
		t.Fatalf("expected line-numbered unknown-project error, got: %q", errb.String())
	}

	// Total: neither the bad entry nor the valid one created an idea.
	for _, p := range []struct{ project, slug string }{
		{"nope", "thing"},
		{"tele", "would-be-fine"},
	} {
		if _, err := os.Stat(filepath.Join(root, "projects", p.project, "runs", p.slug, "run.json")); !os.IsNotExist(err) {
			t.Fatalf("no idea should exist after an aborted batch (%s/%s): %v", p.project, p.slug, err)
		}
	}
	// And the close did not advance HEAD.
	if afterHead := gitLog(t, root, "-1", "--format=%H"); beforeHead != afterHead {
		t.Fatalf("aborted close created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
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
