package cli

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/serve"
)

// TestServeOptionsRendersRealWiring drives the dash and the chore page
// through the same serve.Options runServe builds — no stubbed callbacks.
// serve.New binds nothing (only ListenAndServe does), so serveOptions +
// Handler() is a complete in-process path: this is the check that
// previously needed a live `moe serve` plus curl.
//
// The chip tests next door stub GatherRunRow and WorkflowUI on purpose —
// they isolate the chip-compose seam. This one asserts the opposite: that
// the real GatherDash/GatherChore closures reach a rendered page.
func TestServeOptionsRendersRealWiring(t *testing.T) {
	root := seedChoreRoot(t)

	srv, err := serve.New(serveOptions(root, io.Discard, io.Discard))
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	get := func(path string) string {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s: want 200, got %d body=%s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	// The due chore reaches the dash through GatherDash and folds into
	// BACKLOG, linking to its own /chore/ page rather than a /run/ one.
	dashBody := get("/")
	if !strings.Contains(dashBody, `href="/chore/moe/readme-refresh"`) {
		t.Errorf("dash missing the due chore row:\n%s", dashBody)
	}

	// And the page that link points at renders through GatherChore.
	choreBody := get("/chore/moe/readme-refresh")
	if !strings.Contains(choreBody, "readme-refresh") {
		t.Errorf("chore page missing its own name:\n%s", choreBody)
	}
	if !strings.Contains(choreBody, `<span class="badge live">due</span>`) {
		t.Errorf("chore page should show the due badge:\n%s", choreBody)
	}
}
