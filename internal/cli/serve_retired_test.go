package cli

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// seedRetiredServeRoot stands up a bureaucracy carrying one retired-
// workflow run (tele/amp-code, workflow kb) with a canvas and a claude
// transcript on its `research` document, and returns the root.
func seedRetiredServeRoot(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRun(t, root, "tele", "amp-code", "kb", "research", "summarize")
	writeContent(t, root, "tele", "amp-code", "research", "# Research body\n")
	writeThread(t, root, "tele", "amp-code", "research", "claude", []string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"what did we learn"}}`,
	})
	return root
}

// retiredServeHandler drives the real serve.Options runServe builds —
// no stubbed callbacks — so the assertions below cover the cli-side
// wiring, not just the serve-side rendering.
func retiredServeHandler(t *testing.T, root string) func(path string) *httptest.ResponseRecorder {
	t.Helper()
	srv, err := serve.New(serveOptions(root, io.Discard, io.Discard))
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	return func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr
	}
}

// TestServeRetiredWorkflowCanvas: the canvas route serves a retired
// run's document. The workflow the run records has no registry entry to
// look up a ladder in, so the documents on disk stand in for one — the
// same fallback `moe <wf> cat` already makes.
func TestServeRetiredWorkflowCanvas(t *testing.T) {
	get := retiredServeHandler(t, seedRetiredServeRoot(t))

	rr := get("/run/tele/amp-code/canvas/research")
	if rr.Code != 200 {
		t.Fatalf("canvas route: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Research body") {
		t.Errorf("canvas page missing the canvas body:\n%s", rr.Body.String())
	}
}

// TestServeRetiredWorkflowTranscript: the transcript route validates the
// stage through the same fallback, so a retired run's thread renders
// rather than 404ing on the missing registry entry.
func TestServeRetiredWorkflowTranscript(t *testing.T) {
	get := retiredServeHandler(t, seedRetiredServeRoot(t))

	rr := get("/run/tele/amp-code/transcript/research")
	if rr.Code != 200 {
		t.Fatalf("transcript route: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "what did we learn") {
		t.Errorf("transcript page missing the thread text:\n%s", rr.Body.String())
	}
}

// TestServeRetiredWorkflowRunPageLinks: the run page's canvas and
// transcript links come from RunStages, which used to hard-fail on the
// unregistered workflow and drop *every* link on these 37 runs. The page
// rendered either way — the regression was silent.
func TestServeRetiredWorkflowRunPageLinks(t *testing.T) {
	get := retiredServeHandler(t, seedRetiredServeRoot(t))

	rr := get("/run/tele/amp-code")
	if rr.Code != 200 {
		t.Fatalf("run page: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/run/tele/amp-code/canvas/research"`) {
		t.Errorf("run page missing the canvas link:\n%s", body)
	}
	if !strings.Contains(body, `href="/run/tele/amp-code/transcript/research?agent=claude"`) {
		t.Errorf("run page missing the transcript link:\n%s", body)
	}
	// `summarize` has a documents/ dir but no canvas file, so it stays
	// off the page: canvasLinks stats before it links.
	if strings.Contains(body, `/canvas/summarize"`) {
		t.Errorf("run page linked a canvas that isn't on disk:\n%s", body)
	}
}

// TestServeRetiredWorkflowUnknownStage: a stage the retired run doesn't
// carry is a 404 naming the documents it does — the honest error now
// that `workflow "kb" not registered` no longer fires first. `design` is
// a real sdlc stage and still 404s: the run's own documents are what
// gets validated against.
func TestServeRetiredWorkflowUnknownStage(t *testing.T) {
	get := retiredServeHandler(t, seedRetiredServeRoot(t))

	for _, path := range []string{
		"/run/tele/amp-code/canvas/design",
		"/run/tele/amp-code/transcript/design",
	} {
		rr := get(path)
		if rr.Code != 404 {
			t.Fatalf("GET %s: want 404, got %d body=%s", path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "no such stage: design") {
			t.Errorf("GET %s: want a no-such-stage error, got %q", path, rr.Body.String())
		}
	}
}
