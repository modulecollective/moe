package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
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

	srv, err := serve.New(serveOptions(root, io.Discard))
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

// TestServeUsageDisclosesSteering: `moe serve --help` is the other place
// the security claim is printed. It used to say there is no route from
// the listener to code execution "at all — armed or not", which reads as
// an all-clear the system doesn't earn: the writes land in agent
// prompts. Same claim as the startup line, help register.
func TestServeUsageDisclosesSteering(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runServe([]string{"--nope"}, &out, &errb); code == 0 {
		t.Fatal("expected nonzero exit on an unknown flag")
	}
	usage := errb.String()
	for _, want := range []string{
		"No request executes code",
		"whoever can reach the listener steers",
		"Reach is the only auth",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("serve usage missing %q:\n%s", want, usage)
		}
	}
	if strings.Contains(usage, "code execution") {
		t.Errorf("serve usage still carries the all-clear:\n%s", usage)
	}
}

// TestServeCloseWarnsUnverifiedFollowupClaim: a close driven from the
// web panel must leave the same advisory trail as `moe sdlc close`. The
// CloseRun closure used to hand closeRunInProcess an io.Discard, which
// swallowed the one line the close writes — a canvas claiming a
// followup it never filed. This drives the real closure through the
// real route and asserts the line lands in serve's log.
func TestServeCloseWarnsUnverifiedFollowupClaim(t *testing.T) {
	root := seedCloseFixture(t, "tele", "close-me", "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "close-me", "design",
		"# Design\n\nFiled as followup `never-filed-thing`.\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var logbuf bytes.Buffer
	srv, err := serve.New(serveOptions(root, &logbuf))
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/tele/close-me/close", strings.NewReader("")))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("close: want 303, got %d body=%s log=%q", rr.Code, rr.Body.String(), logbuf.String())
	}
	if md, err := run.Load(root, "tele", "close-me"); err != nil {
		t.Fatal(err)
	} else if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want closed", md.Status)
	}

	const want = "canvas design claims followup `never-filed-thing` that was never filed"
	if !strings.Contains(logbuf.String(), want) {
		t.Errorf("serve log missing the advisory:\nwant substring: %s\ngot: %q", want, logbuf.String())
	}
}
