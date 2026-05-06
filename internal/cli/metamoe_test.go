package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestMetaMoeRegistered partners with TestSDLCRegistered: a registration
// drift in init() ordering would silently drop the meta-moe workflow.
func TestMetaMoeRegistered(t *testing.T) {
	wf, err := LookupWorkflow(metaMoeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Summary == "" {
		t.Fatal("meta-moe workflow summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{metaMoeWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "report"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("meta-moe usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestMetaMoeWorkflowStageOrder confirms `report` is the only stage
// and `new` is a facade. A `report` ⇒ `report`-as-only-stage shape
// is the contract; growing a `scan` or `share` stage later would
// require updating this test alongside the workflow.
func TestMetaMoeWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow(metaMoeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	if len(got) != 1 || got[0] != metaMoeReportDoc {
		t.Fatalf("stages=%v want=[%s]", got, metaMoeReportDoc)
	}
}

// TestBuildSystemPromptInjectsMetaMoeReportFragment is the wiring
// check that workflows/meta-moe/report.md lands in the prompt when
// the run names the meta-moe workflow.
func TestBuildSystemPromptInjectsMetaMoeReportFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "meta-moe-2026-05-05",
		Project:  "moe",
		Title:    "meta-moe",
		Workflow: metaMoeWorkflow,
	}
	got, err := buildSystemPrompt(root, md, metaMoeReportDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: report") {
		t.Fatalf("prompt missing report fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "moe maintainers") {
		t.Fatalf("report.md missing audience framing:\n%s", got)
	}
}

func TestMetaMoeBaseSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"foo", "foo"},
		{"foo-2", "foo"},
		{"foo-13", "foo"},
		{"foo-2026-05-05", "foo"},
		{"foo-2026-05-05-2", "foo"},
		{"foo-bar", "foo-bar"},
		{"foo-bar-3", "foo-bar"},
		{"foo-redux", "foo-redux"}, // not auto-stripped; agent's job
		// The collapse-in-one-pass property: dated form takes
		// precedence so we don't get a half-stripped intermediate.
		{"metrics-2026-01-02-3", "metrics"},
	}
	for _, c := range cases {
		if got := metaMoeBaseSlug(c.in); got != c.want {
			t.Errorf("metaMoeBaseSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMetaMoeScanProjectMissingProject confirms a project with no
// runs dir on disk returns an empty result, not an error. Running
// meta-moe against a fresh project should "just work."
func TestMetaMoeScanProjectMissingProject(t *testing.T) {
	root := t.TempDir()
	got, err := metaMoeScanProject(root, "ghost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Project != "ghost" {
		t.Errorf("Project=%q want %q", got.Project, "ghost")
	}
	if len(got.SlugCollisions) != 0 || len(got.FollowupCounts) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestMetaMoeScanProjectGroupsCollisions sets up a fixture
// bureaucracy with auto-suffixed runs and asserts the scan groups
// them under their base slug. A lone slug shouldn't appear in the
// collisions map (a single-member group is not a collision).
func TestMetaMoeScanProjectGroupsCollisions(t *testing.T) {
	root := t.TempDir()
	for _, slug := range []string{
		"foo",            // collides with foo-2
		"foo-2",          // pair with foo
		"bar",            // alone
		"baz",            // collides with baz-2026-05-05
		"baz-2026-05-05", // pair with baz
	} {
		dir := filepath.Join(root, "projects", "p", "runs", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := metaMoeScanProject(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.SlugCollisions["bar"]; ok {
		t.Errorf("'bar' should not appear in collisions (single-member group)")
	}
	if want := []string{"foo", "foo-2"}; !equalStrings(got.SlugCollisions["foo"], want) {
		t.Errorf("foo collisions=%v want %v", got.SlugCollisions["foo"], want)
	}
	if want := []string{"baz", "baz-2026-05-05"}; !equalStrings(got.SlugCollisions["baz"], want) {
		t.Errorf("baz collisions=%v want %v", got.SlugCollisions["baz"], want)
	}
}

// TestMetaMoeScanProjectCountsFollowups verifies unchecked-only
// counting: `- [x]` is harvested-or-resolved and must not count.
// Files without followups.md contribute zero — they're absent from
// the result map.
func TestMetaMoeScanProjectCountsFollowups(t *testing.T) {
	root := t.TempDir()
	mk := func(slug, body string) {
		dir := filepath.Join(root, "projects", "p", "runs", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "followups.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("a", "- [ ] one — first\n- [ ] two — second\n- [x] done — already harvested\n")
	mk("b", "")                          // no followups.md
	mk("c", "  - [ ] indented — counts") // leading whitespace is fine

	got, err := metaMoeScanProject(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.FollowupCounts["a"] != 2 {
		t.Errorf("a count=%d want 2", got.FollowupCounts["a"])
	}
	if _, ok := got.FollowupCounts["b"]; ok {
		t.Errorf("b should not appear in counts (no file)")
	}
	if got.FollowupCounts["c"] != 1 {
		t.Errorf("c count=%d want 1", got.FollowupCounts["c"])
	}
}

// TestMetaMoeRenderKickoffEmpty asserts the empty-bureaucracy path
// names both sections and falls back to a one-liner per section
// rather than dropping the heading.
func TestMetaMoeRenderKickoffEmpty(t *testing.T) {
	out := metaMoeRenderKickoff(metaMoeScanResult{
		Project:        "moe",
		SlugCollisions: map[string][]string{},
		FollowupCounts: map[string]int{},
	})
	for _, want := range []string{
		"## Repeated work",
		"## Unchecked followups",
		"(no auto-suffixed run-slug groups",
		"(no runs with unchecked followups",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, out)
		}
	}
}

// TestMetaMoeRenderKickoffPopulated checks the populated path lists
// each finding and renders it in stable order so successive passes
// produce diffable output.
func TestMetaMoeRenderKickoffPopulated(t *testing.T) {
	out := metaMoeRenderKickoff(metaMoeScanResult{
		Project: "moe",
		SlugCollisions: map[string][]string{
			"foo": {"foo", "foo-2"},
			"bar": {"bar", "bar-2026-05-05"},
		},
		FollowupCounts: map[string]int{
			"foo": 3,
			"baz": 1,
		},
	})
	for _, want := range []string{
		"`foo`: foo, foo-2",
		"`bar`: bar, bar-2026-05-05",
		"`foo`: 3 unchecked",
		"`baz`: 1 unchecked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, out)
		}
	}
	// Stable bases ordering: bar < foo.
	barIdx := strings.Index(out, "`bar`: bar, bar-2026-05-05")
	fooIdx := strings.Index(out, "`foo`: foo, foo-2")
	if barIdx < 0 || fooIdx < 0 || barIdx >= fooIdx {
		t.Errorf("expected bar before foo in collision section: bar=%d foo=%d", barIdx, fooIdx)
	}
}

// TestMetaMoePublishCanvasCopies covers the publish step: given a
// canvas inside a worktree, it copies bytes verbatim to
// projects/<p>/meta-moe.md and returns the (relative) staged path.
func TestMetaMoePublishCanvasCopies(t *testing.T) {
	work := t.TempDir()
	md := &run.Metadata{ID: "r", Project: "p", Workflow: metaMoeWorkflow}
	canvasRel := run.ContentPath(md.Project, md.ID, metaMoeReportDoc)
	canvasAbs := filepath.Join(work, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# meta-moe — project p\n\nbody.\n"
	if err := os.WriteFile(canvasAbs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := metaMoePublishCanvas(work, md)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := filepath.Join("projects", md.Project, "meta-moe.md")
	if len(paths) != 1 || paths[0] != wantRel {
		t.Fatalf("paths=%v want [%s]", paths, wantRel)
	}
	got, err := os.ReadFile(filepath.Join(work, wantRel))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("published body mismatch:\n got=%q\nwant=%q", got, body)
	}
}

// TestMetaMoePublishCanvasNoCanvas verifies the missing-canvas path
// returns nil paths without error — commitTurn's existing canvas
// guard is the loud check; this helper stays quiet so it can run
// before that guard fires.
func TestMetaMoePublishCanvasNoCanvas(t *testing.T) {
	work := t.TempDir()
	md := &run.Metadata{ID: "r", Project: "p", Workflow: metaMoeWorkflow}
	paths, err := metaMoePublishCanvas(work, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paths != nil {
		t.Fatalf("expected nil paths, got %v", paths)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
