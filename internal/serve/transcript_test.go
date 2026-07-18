package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/run"
)

// ladderStages is the RunStages closure most transcript tests share:
// the sdlc ladder, so a stage in it validates and a stage outside 404s.
func ladderStages(_, _ string) ([]string, error) {
	return []string{"design", "code", "review", "test", "push"}, nil
}

// writeThread drops a thread-<agent>.jsonl under the run's
// documents/<stage>/ dir, returning the absolute path.
func writeThread(t *testing.T, root, projectID, runID, stage, agent, content string) string {
	t.Helper()
	path := filepath.Join(root, run.ThreadPathFor(agent, projectID, runID, stage))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const claudeSample = `{"type":"user","timestamp":"2026-05-16T21:17:27.000Z","message":{"role":"user","content":"look at the canvas"}}
{"type":"assistant","timestamp":"2026-05-16T21:17:31.000Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/x/canvas.md"}}]}}
{"type":"user","timestamp":"2026-05-16T21:17:31.500Z","message":{"role":"user","content":[{"tool_use_id":"tu-1","type":"tool_result","content":"line1\nline2"}]}}
{"type":"assistant","timestamp":"2026-05-16T21:17:34.000Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"All done here."}]}}
`

// TestTranscriptRouteRendersPage: the happy path — a claude thread on
// disk renders as structured HTML with the agent name, a model chip, the
// user/assistant text, the tool call, and a <details> result block.
func TestTranscriptRouteRendersPage(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeThread(t, root, "alpha", "fix-it", "design", "claude", claudeSample)

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, want := range []string{
		`class="t-agent">claude`,
		`class="badge t-model">claude-opus-4-8`,
		"look at the canvas",
		"All done here.",
		`class="t-tool">Read`,
		"<details",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q\n%s", want, out)
		}
	}
	// Single model → no per-block model chip (the header chip suffices).
	if strings.Contains(out, "assistant · claude-opus-4-8") {
		t.Errorf("single-model transcript should not carry per-block chips\n%s", out)
	}
	// Whole file fits one window: the load-earlier control is inert.
	if !strings.Contains(out, "start of transcript") {
		t.Errorf("short transcript should show an inert start marker\n%s", out)
	}
	if strings.Contains(out, `id="load-earlier"`) {
		t.Errorf("short transcript should not show an active load-earlier button\n%s", out)
	}
}

// TestTranscriptRouteAgentPickAndCrossLink: with both threads on disk,
// no ?agent renders claude and links to codex; ?agent=codex renders
// codex and links back to claude.
func TestTranscriptRouteAgentPickAndCrossLink(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeThread(t, root, "alpha", "fix-it", "design", "claude", claudeSample)
	codexSample := `{"timestamp":"2026-05-15T16:20:42.296Z","type":"turn_context","payload":{"type":"turn_context","model":"gpt-5-codex"}}
{"timestamp":"2026-05-15T16:20:59.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex speaking"}]}}
`
	writeThread(t, root, "alpha", "fix-it", "design", "codex", codexSample)

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	// Default: claude, with a link to the codex thread.
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	out := rr.Body.String()
	if !strings.Contains(out, `class="t-agent">claude`) {
		t.Errorf("default agent should be claude\n%s", out)
	}
	if !strings.Contains(out, `href="?agent=codex"`) {
		t.Errorf("default page should cross-link to codex\n%s", out)
	}

	// Explicit codex.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design?agent=codex", nil))
	out = rr.Body.String()
	if !strings.Contains(out, "codex speaking") {
		t.Errorf("?agent=codex should render the codex thread\n%s", out)
	}
	if !strings.Contains(out, `class="badge t-model">gpt-5-codex`) {
		t.Errorf("codex model chip missing\n%s", out)
	}
	if !strings.Contains(out, `href="?agent=claude"`) {
		t.Errorf("codex page should cross-link back to claude\n%s", out)
	}
}

// TestTranscriptRouteMissingThreadEmptyState: a valid stage with no
// thread on disk (a turn that hasn't closed, or a stale bookmark) is a
// 200 empty state naming the path — not a 404.
func TestTranscriptRouteMissingThreadEmptyState(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "no transcript yet") {
		t.Errorf("body should announce empty state, got:\n%s", out)
	}
	if !strings.Contains(out, "thread-claude.jsonl") {
		t.Errorf("empty state should name the thread path, got:\n%s", out)
	}
}

// TestTranscriptRouteUnknownStage404: a stage outside the run's ladder
// is a 404 (mirrors the canvas route's bogus-stage handling).
func TestTranscriptRouteUnknownStage404(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/bogus", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such stage: bogus") {
		t.Errorf("body should name the bad stage, got:\n%s", rr.Body.String())
	}
}

// TestTranscriptRouteUnknownRun404: RunStages erroring (an unknown run
// or workflow) maps to 404, same as an unknown stage — resolution is a
// lookup, not a file stat.
func TestTranscriptRouteUnknownRun404(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		RunStages: func(_, runID string) ([]string, error) {
			return nil, fmt.Errorf("run %s does not exist", runID)
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/ghost/transcript/design", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestTranscriptRouteBadAgent400: an ?agent= that isn't claude or codex
// is a 400 rather than a silent empty state on a nonsense thread path.
func TestTranscriptRouteBadAgent400(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design?agent=gpt", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// manyUnits builds a claude thread of n user-message lines, each a
// distinct "msg-<i>" — n render units, for exercising the paging window.
func manyUnits(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b,
			`{"type":"user","timestamp":"2026-05-16T21:17:27.000Z","message":{"role":"user","content":"msg-%d"}}`+"\n", i)
	}
	return b.String()
}

// TestTranscriptRouteTailPagingAndFragment: a file larger than one
// window opens at the tail with an active load-earlier button; the
// button's ?before= cursor fetches the preceding window; the fragment
// form renders just the chunk (no page chrome) and reports its own
// earlier cursor, reaching an at-start terminus.
func TestTranscriptRouteTailPagingAndFragment(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeThread(t, root, "alpha", "fix-it", "design", "claude", manyUnits(250))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})

	// Default: the tail window [50,250). msg-249 shows, msg-0/msg-49 don't,
	// and the load-earlier button points at ?before=50.
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	out := rr.Body.String()
	if !strings.Contains(out, `id="load-earlier"`) || !strings.Contains(out, "?before=50") {
		t.Errorf("tail page should carry an active load-earlier at ?before=50\n%s", out)
	}
	if !strings.Contains(out, ">msg-249<") {
		t.Errorf("tail page should show the last unit\n%s", out)
	}
	for _, absent := range []string{">msg-0<", ">msg-49<"} {
		if strings.Contains(out, absent) {
			t.Errorf("tail page should not show %q (before the window)\n%s", absent, out)
		}
	}

	// The fragment form of the preceding window: just the chunk, reporting
	// data-before="0" data-atstart="true" (window [0,50) reaches the start).
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design?agent=claude&before=50&fragment=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment status=%d", rr.Code)
	}
	frag := rr.Body.String()
	if strings.Contains(frag, "<!doctype") || strings.Contains(frag, "<body") {
		t.Errorf("fragment must not carry page chrome\n%s", frag)
	}
	if !strings.Contains(frag, `class="transcript-chunk" data-before="0" data-atstart="true"`) {
		t.Errorf("fragment should report its earlier cursor as at-start\n%s", frag)
	}
	for _, want := range []string{">msg-0<", ">msg-49<"} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment should carry the earlier window unit %q\n%s", want, frag)
		}
	}
	if strings.Contains(frag, ">msg-50<") {
		t.Errorf("fragment window [0,50) should exclude msg-50\n%s", frag)
	}
}

// TestTranscriptRouteWithoutRunStages500: an unwired Options.RunStages
// can't validate the stage — fail loud, matching the canvas route's
// nil-resolver 500.
func TestTranscriptRouteWithoutRunStages500(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestRunPageShowsTranscriptLinks: a stage with a canvas and a thread on
// disk surfaces a transcript link beside the canvas link; a stage with a
// canvas but no thread shows none.
func TestRunPageShowsTranscriptLinks(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeCanvas(t, root, "alpha", "fix-it", "design", "# design\n")
	writeCanvas(t, root, "alpha", "fix-it", "code", "# code\n")
	writeThread(t, root, "alpha", "fix-it", "design", "claude", claudeSample)

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(p, r, stage string) (string, error) {
			return filepath.Join(root, "projects", p, "runs", r, "documents", stage, "content.md"), nil
		},
		RunStages: ladderStages,
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, `href="/run/alpha/fix-it/transcript/design?agent=claude"`) {
		t.Errorf("design stage should link to its claude transcript\n%s", out)
	}
	if !strings.Contains(out, "claude transcript") {
		t.Errorf("transcript link should be labelled\n%s", out)
	}
	// The code stage has a canvas but no thread — no transcript link.
	if strings.Contains(out, `href="/run/alpha/fix-it/transcript/code?agent=claude"`) {
		t.Errorf("code stage has no thread; it should not get a transcript link\n%s", out)
	}
}

// bigResultThread builds a one-turn claude thread whose single tool
// result carries out as its content.
func bigResultThread(t *testing.T, out string) string {
	t.Helper()
	quoted, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return `{"type":"assistant","timestamp":"2026-05-16T21:17:31.000Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/x/big"}}]}}
{"type":"user","timestamp":"2026-05-16T21:17:31.500Z","message":{"role":"user","content":[{"tool_use_id":"tu-1","type":"tool_result","content":` + string(quoted) + `}]}}
`
}

// fillerResult builds a multi-line blob of about size bytes with
// HEADSENTINEL first, MIDSENTINEL in the middle and TAILSENTINEL last,
// returning it alongside its line count.
func fillerResult(size int) (string, int) {
	var b strings.Builder
	lines := 0
	add := func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
		lines++
	}
	add("HEADSENTINEL")
	half := size / 2 / 100
	for i := 0; i < half; i++ {
		add(fmt.Sprintf("head filler %04d %s", i, strings.Repeat("a", 80)))
	}
	add("MIDSENTINEL")
	for i := 0; i < half; i++ {
		add(fmt.Sprintf("tail filler %04d %s", i, strings.Repeat("b", 80)))
	}
	add("TAILSENTINEL")
	return b.String(), lines
}

// TestTranscriptCapsHugeResult: a tool result past resultCapBytes renders
// its head and tail with the middle replaced by a marker, and the summary
// still reports the line count the tool actually produced.
func TestTranscriptCapsHugeResult(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	out, lines := fillerResult(2 * resultCapBytes)
	if len(out) <= resultCapBytes {
		t.Fatalf("fixture is only %d bytes; cap is %d", len(out), resultCapBytes)
	}
	writeThread(t, root, "alpha", "fix-it", "design", "claude", bigResultThread(t, out))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"HEADSENTINEL", "TAILSENTINEL", "KiB elided", fmt.Sprintf("· %d lines", lines)} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "MIDSENTINEL") {
		t.Errorf("middle of an over-cap result should be elided")
	}
	if len(body) > 3*resultCapBytes/2 {
		t.Errorf("page is %d bytes for a %d-byte result; cap did not bound it", len(body), len(out))
	}
}

// TestTranscriptKeepsResultUnderCap: a large-but-legal result (bigger
// than any the claude harness emits) passes through whole, marker-free.
func TestTranscriptKeepsResultUnderCap(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	out, _ := fillerResult(resultCapBytes / 2)
	if len(out) > resultCapBytes {
		t.Fatalf("fixture is %d bytes; wanted under the %d cap", len(out), resultCapBytes)
	}
	writeThread(t, root, "alpha", "fix-it", "design", "claude", bigResultThread(t, out))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"HEADSENTINEL", "MIDSENTINEL", "TAILSENTINEL"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q from an under-cap result", want)
		}
	}
	if strings.Contains(body, "elided") {
		t.Errorf("under-cap result should not be elided")
	}
}

// TestFragmentCapsHugeResult: the load-earlier fragment shares
// fillResult with the page, so an over-cap result in an earlier window
// is capped there too. Guards against the two call sites diverging.
func TestFragmentCapsHugeResult(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	out, _ := fillerResult(2 * resultCapBytes)
	// The big result sits at the head of the file, so the default tail
	// window excludes it and only ?before= reaches it.
	writeThread(t, root, "alpha", "fix-it", "design", "claude", bigResultThread(t, out)+manyUnits(250))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, RunStages: ladderStages})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/transcript/design?agent=claude&before=51&fragment=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment status=%d", rr.Code)
	}
	frag := rr.Body.String()
	for _, want := range []string{"HEADSENTINEL", "TAILSENTINEL", "KiB elided"} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
	if strings.Contains(frag, "MIDSENTINEL") {
		t.Errorf("fragment should elide the middle of an over-cap result")
	}
	if len(frag) > 3*resultCapBytes/2 {
		t.Errorf("fragment is %d bytes for a %d-byte result; cap did not bound it", len(frag), len(out))
	}
}

// TestCapResultSingleLineWhale: a newline-free result — minified JSON,
// say — still gets capped, and the cuts land on rune boundaries so the
// page stays valid UTF-8.
func TestCapResultSingleLineWhale(t *testing.T) {
	// "é" is two bytes, so a repeat count that overruns the half-budget
	// puts a cut mid-rune unless capResult walks back.
	whale := "START" + strings.Repeat("é", resultCapBytes) + "END"
	got := capResult(whale)
	if len(got) >= len(whale) {
		t.Fatalf("whale not capped: %d bytes in, %d out", len(whale), len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped output is not valid UTF-8")
	}
	if !strings.HasPrefix(got, "START") || !strings.HasSuffix(got, "END") {
		t.Errorf("capped output should keep the head and tail of the line")
	}
	if !strings.Contains(got, "KiB elided") {
		t.Errorf("capped output should carry the elision marker")
	}
}
