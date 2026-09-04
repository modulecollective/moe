package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// usageFixture is a git-backed bureaucracy with two projects, each with
// one run carrying a mirrored transcript. The journal commit is what
// dates the stage, so the fixture commits rather than just writing.
func usageFixture(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	seedProject(t, root, "alpha")
	seedProject(t, root, "beta")
	seedUsageRun(t, root, "alpha", "big-one", "sdlc")
	seedUsageRun(t, root, "beta", "small-one", "chat")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed runs")

	now := time.Now()
	// Distinct dates so run order is deterministic: beta is newer.
	seedUsageThread(t, root, "alpha", "big-one", "design",
		usageTurn("a1", "claude-opus-5", 2_000_000, 12_000), now.Add(-6*time.Hour))
	seedUsageThread(t, root, "beta", "small-one", "design",
		usageTurn("b1", "claude-opus-5", 900, 900), now.Add(-1*time.Hour))
	return newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
}

func seedUsageRun(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	md := run.Metadata{
		ID: runID, Project: projectID, Status: run.StatusMerged,
		Workflow: workflow, Created: "2026-09-01",
		Documents: map[string]*run.Document{},
	}
	dir := filepath.Join(root, run.Dir(projectID, runID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedUsageThread(t *testing.T, root, projectID, runID, stage, body string, when time.Time) {
	t.Helper()
	abs := filepath.Join(root, run.ThreadPathFor("claude", projectID, runID, stage))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	date := when.Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
		"commit", "-m", "work: update "+stage+
			"\n\nMoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Document: "+stage+"\n")
}

func usageTurn(id, model string, cacheRead, output int) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"id":%q,"model":%q,"content":[{"type":"text","text":"x"}],`+
			`"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":%d,"output_tokens":%d}}}`,
		id, model, cacheRead, output) + "\n"
}

// TestUsagePageRendersBothTables: the totals strip, the bucket table and
// the runs table, with every run linking to its own page.
func TestUsagePageRendersBothTables(t *testing.T) {
	s := usageFixture(t)
	rr := get(t, s, "/usage")
	mustContain(t, rr,
		`href="/run/alpha/big-one"`,
		`href="/run/beta/small-one"`,
		"alpha/big-one",
		"beta/small-one",
		"Notional dollars are API sticker prices",
	)
	if got := strings.Count(rr.Body.String(), `class="usage-table"`); got != 2 {
		t.Errorf("usage-table count = %d, want the bucket and runs tables", got)
	}
}

// TestUsagePageCarriesRawValuesForSorting: the display is humanized, so
// the sort script needs the raw figure alongside it. Without data-v the
// column would sort lexically, which is the bug the page exists to
// avoid: 900 must not outrank 2M.
func TestUsagePageCarriesRawValuesForSorting(t *testing.T) {
	s := usageFixture(t)
	rr := get(t, s, "/usage")
	mustContain(t, rr,
		// alpha's cache reads, humanized in the cell and raw in the attr.
		`data-v="2000000">2.0M<`,
		// beta's, where display and raw agree — the discriminating pair
		// is that 900 sorts below 2000000 on the attribute, not above it
		// as "900" > "2.0M" would.
		`data-v="900">900<`,
	)
}

// TestUsagePageDefaultsToRunOrder: newest activity first, which is the
// order the dash's completed list uses — the board, with cost attached.
func TestUsagePageDefaultsToRunOrder(t *testing.T) {
	s := usageFixture(t)
	body := get(t, s, "/usage").Body.String()
	runs := body[strings.Index(body, ">runs "):]
	if strings.Index(runs, "beta/small-one") > strings.Index(runs, "alpha/big-one") {
		t.Errorf("runs table = %s\nwant the more recent run first", runs)
	}
}

// TestUsagePageProjectFilter: ?project scopes the report and the crumb,
// which is the whole per-project story — the hub links here rather than
// to a page of its own.
func TestUsagePageProjectFilter(t *testing.T) {
	s := usageFixture(t)
	rr := get(t, s, "/usage?project=beta")
	mustContain(t, rr,
		`<a href="/projects/beta">beta</a> → usage`,
		"beta/small-one",
	)
	if strings.Contains(rr.Body.String(), "alpha/big-one") {
		t.Error("filtered page still lists the other project's run")
	}
}

func TestUsagePageWindowFilter(t *testing.T) {
	s := usageFixture(t)
	// Both stages are hours old, so a 2-hour window keeps only beta's.
	rr := get(t, s, "/usage?since=2h")
	mustContain(t, rr, "beta/small-one", "last 2h")
	if strings.Contains(rr.Body.String(), "alpha/big-one") {
		t.Error("2h window still lists a 6-hour-old stage")
	}
}

// TestUsagePageRejectsBadInput mirrors the CLI: a typo'd window is an
// error, not a silent empty window, and an unknown project is a 404 like
// every other project-scoped route.
func TestUsagePageRejectsBadInput(t *testing.T) {
	s := usageFixture(t)
	for path, want := range map[string]int{
		"/usage?since=banana": http.StatusBadRequest,
		"/usage?project=nope": http.StatusNotFound,
		"/usage?project=../x": http.StatusNotFound,
	} {
		if got := get(t, s, path).Code; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
}

// TestUsageIsReachableFromTheBoards: the seed asked for it in the
// hamburger, in the dash crumb, and from a project hub.
func TestUsageIsReachableFromTheBoards(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return nil, 1, 0, nil, nil
		},
	})
	mustContain(t, get(t, s, "/"), `<a href="/usage">usage</a>`)
	mustContain(t, get(t, s, "/projects/alpha"), `<a href="/usage?project=alpha">usage</a>`)
	mustContain(t, get(t, s, "/lore"), `<a href="/usage">usage</a>`)
}

// TestUsagePageCostSitsBesideTheLabel: the notional column reads
// immediately after the columns that name the row. Both tables are wider
// than the 38rem measure and than a phone, so whatever sits at the tail
// is behind a horizontal scroll with no visible cue — which is where the
// per-run dollar figure was, on the page whose whole job is spend.
// Sliding it forward is the fix, and column order is not something the
// other usage tests pin.
func TestUsagePageCostSitsBesideTheLabel(t *testing.T) {
	s := usageFixture(t)
	body := get(t, s, "/usage").Body.String()

	// The bucket row's label columns end at the model; the run row's at
	// the slug link. In both, the next cell is the cost.
	for _, tc := range []struct{ after, want string }{
		{`<td>claude-opus-5</td>`, `<td class="num" data-v="1.30">$1.30</td>`},
		{`>alpha/big-one</a></td>`, `<td class="num" data-v="1.30">$1.30</td>`},
	} {
		i := strings.Index(body, tc.after)
		if i < 0 {
			t.Fatalf("usage page has no %q cell", tc.after)
		}
		rest := strings.TrimSpace(body[i+len(tc.after):])
		if !strings.HasPrefix(rest, tc.want) {
			t.Errorf("cell after %q is %.60q…\nwant it to start %q", tc.after, rest, tc.want)
		}
	}
}

// TestUsageRunCellWrapRuleServed: the reorder alone still loses the cost
// when the slug is wider than the screen, so the run cell is the one
// nowrap exception. That is pure CSS — no test can prove the layout, but
// this proves the rules reach the wire. Scoped to the rule block: a
// slice that runs to EOF would pass on any stylesheet that mentions
// white-space anywhere later.
func TestUsageRunCellWrapRuleServed(t *testing.T) {
	// A bare server: the rule ships through //go:embed, so this needs no
	// fixture, only the static route.
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	css := getOK(t, s, "/static/style.css")
	const sel = ".usage-table td.run {"
	start := strings.Index(css, sel)
	if start < 0 {
		t.Fatalf("style.css lost its %s rule", sel)
	}
	start += len(sel)
	end := strings.Index(css[start:], "}")
	if end < 0 {
		t.Fatalf("%s rule is unterminated", sel)
	}
	rule := css[start : start+end]
	for _, decl := range []string{"white-space: normal;", "overflow-wrap: anywhere;", "min-width:"} {
		if !strings.Contains(rule, decl) {
			t.Errorf("%s is missing %q: %q", sel, decl, rule)
		}
	}
}
