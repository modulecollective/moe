package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// writeFile creates parent dirs and writes a browse-corpus file under
// the bureaucracy root.
func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	return rr
}

func mustContain(t *testing.T, rr *httptest.ResponseRecorder, wants ...string) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("body missing %q\n%s", w, out)
		}
	}
}

func TestLoreIndexAndEntry(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lore/widget-thing.md",
		"---\ntitle: Widgets jam on Tuesdays\napplies-when: a widget is involved\n---\n\n# Widgets\n\nThe **body** of the lore.\n")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	idx := get(t, s, "/lore")
	mustContain(t, idx, "Widgets jam on Tuesdays", "a widget is involved", `href="/lore/widget-thing"`)

	entry := get(t, s, "/lore/widget-thing")
	mustContain(t, entry, "<h1>Widgets</h1>", "<strong>body</strong>", "applies when: a widget is involved")
	// Frontmatter must not leak into the rendered body.
	if strings.Contains(entry.Body.String(), "applies-when:") {
		t.Errorf("frontmatter leaked into body:\n%s", entry.Body.String())
	}
}

func TestLoreEntryRejectsBadName(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	for _, bad := range []string{"/lore/bad.name", "/lore/..%2fetc"} {
		if rr := get(t, s, bad); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: want 404, got %d", bad, rr.Code)
		}
	}
}

func TestProjectsIndexAndHub(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	seedProject(t, root, "beta")
	writeFile(t, root, "projects/alpha/digital-twin/architecture.md", "# Architecture\n\nshape.\n")
	writeFile(t, root, "projects/alpha/knowledge/index.md", "# alpha kb\n")
	writeFile(t, root, "projects/alpha/knowledge/topics/foo.md", "# Foo\n")

	gather := func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			{Project: "alpha", Run: "fix-1", Bucket: dash.BucketActiveRuns, When: time.Now()},
			{Project: "alpha", Run: "pulse-1", Bucket: dash.BucketActiveRuns, When: time.Now(), Depth: 1},
			{Project: "alpha", Run: "tidy", Bucket: dash.BucketChores, When: time.Now()},
			{Project: "beta", Run: "old", Bucket: dash.BucketCompletedRuns, When: time.Now()},
		}, 2, 1, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, GatherDash: gather})

	idx := get(t, s, "/projects")
	mustContain(t, idx, `href="/projects/alpha"`, `href="/projects/beta"`, "1 topic", "1 twin doc")

	hub := get(t, s, "/projects/alpha")
	mustContain(t, hub,
		`href="/run/alpha/fix-1"`,
		`href="/chore/alpha/tidy"`,
		`href="/projects/alpha/knowledge"`,
		`href="/projects/alpha/twin/architecture"`,
		// A nested descendant draws its connector on the hub's ACTIVE
		// list too, same classes as the home dash.
		`class="row nested depth1"`,
	)

	// A project with no knowledge/twin reads as empty-state, not broken.
	betaHub := get(t, s, "/projects/beta")
	mustContain(t, betaHub, "no knowledge base yet", "no twin yet")
}

// TestProjectHubRendersBannerArt pins requirement 2: the project hub
// gathers scoped to its own project (the projectID reaches GatherDash)
// and draws the factory art + project-scoped histogram under the header —
// both banner-art <pre> blocks, with the factory rail carrying the
// #factory-art hook the cross-fade JS targets.
func TestProjectHubRendersBannerArt(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")

	counts := make([]int, dash.HistDays)
	counts[dash.HistDays-1] = 7
	var gotProject string
	gather := func(projectID string) ([]dash.Row, int, int, []int, error) {
		gotProject = projectID
		return []dash.Row{
			{Project: "alpha", Run: "fix-1", Bucket: dash.BucketActiveRuns, When: time.Now()},
		}, 1, 1, counts, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, GatherDash: gather})

	hub := get(t, s, "/projects/alpha")
	mustContain(t, hub,
		`class="banner-art"`,                  // histogram <pre>
		`class="banner-art" id="factory-art"`, // factory rail with the JS hook
		`<span class="art-mid">`,              // glyphs banded, not flat-tinted
	)
	// The caption's words land in separate band spans; strip the markup to
	// assert the text a reader sees.
	for _, want := range []string{
		"activity · last 60 days", // the histogram caption
		"peak 7 runs/day",         // scaled to the project's window
	} {
		if !strings.Contains(artText(hub.Body.String()), want) {
			t.Errorf("hub art missing %q\n%s", want, hub.Body.String())
		}
	}
	if gotProject != "alpha" {
		t.Errorf("GatherDash got projectID %q, want \"alpha\"", gotProject)
	}
}

func TestProjectHubUnknownProject404(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	if rr := get(t, s, "/projects/ghost"); rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

func TestProjectHubListsTwinDocsWithoutGitProvenance(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	writeFile(t, root, "projects/alpha/digital-twin/architecture.md", "# Architecture\n")

	oldHook := git.Hook
	t.Cleanup(func() { git.Hook = oldHook })
	var gitCalls int
	git.Hook = func(string, []string, time.Duration, error) { gitCalls++ }

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	hub := get(t, s, "/projects/alpha")
	mustContain(t, hub, `<a class="slug" href="/projects/alpha/twin/architecture">architecture</a>`)
	if gitCalls != 0 {
		t.Fatalf("project hub made %d git calls, want none", gitCalls)
	}
}

func TestKnowledgeIndexRewritesTopicLinks(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	writeFile(t, root, "projects/alpha/knowledge/index.md",
		"# alpha kb\n\n- [Claude Code](topics/claude-code.md) — the CLI\n")
	writeFile(t, root, "projects/alpha/knowledge/topics/claude-code.md", "# Claude Code\n\nbody.\n")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	idx := get(t, s, "/projects/alpha/knowledge")
	mustContain(t, idx,
		`<a href="/projects/alpha/knowledge/claude-code">Claude Code</a>`,
		`href="/projects/alpha/knowledge/claude-code"`,
	)

	topic := get(t, s, "/projects/alpha/knowledge/claude-code")
	mustContain(t, topic, "<h1>Claude Code</h1>", "<p>body.</p>")
}

// The "all topics" listing is the source of truth for navigation: a topic
// absent from the curated index.md must still be reachable. The listing is
// built from os.ReadDir, not from index.md, and carries no per-topic git
// provenance (that badge lives on the topic detail page).
func TestKnowledgeIndexListsUnlinkedTopics(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	writeFile(t, root, "projects/alpha/knowledge/index.md", "# alpha kb\n\nno links here.\n")
	writeFile(t, root, "projects/alpha/knowledge/topics/orphan.md", "# Orphan\n\nbody.\n")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	idx := get(t, s, "/projects/alpha/knowledge")
	mustContain(t, idx, `<a class="slug" href="/projects/alpha/knowledge/orphan">orphan</a>`)
	if strings.Contains(idx.Body.String(), "updated ") {
		t.Errorf("index page should carry no per-topic git provenance, got:\n%s", idx.Body.String())
	}
}

func TestTwinDocRenders(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	writeFile(t, root, "projects/alpha/digital-twin/patterns.md", "# Patterns\n\nnamed patterns.\n")
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed twin\n\nMoE-Project: alpha\nMoE-Run: shape-twin")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	rr := get(t, s, "/projects/alpha/twin/patterns")
	mustContain(t, rr,
		"<h1>Patterns</h1>",
		"<p>named patterns.</p>",
		`<a href="/run/alpha/shape-twin">alpha/shape-twin</a>`,
	)
}

func TestBrowseMissingDocIs404(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	if rr := get(t, s, "/projects/alpha/twin/nonesuch"); rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestBrowseWorksInSafeMode: read-only views must not touch the spawn
// bucket — they render identically with Insecure off.
func TestBrowseWorksInSafeMode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lore/x.md", "# X\n\nbody.\n")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	mustContain(t, get(t, s, "/lore"), "lore")
	mustContain(t, get(t, s, "/lore/x"), "<h1>X</h1>")
}
