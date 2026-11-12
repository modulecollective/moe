package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
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
		"pushes pending bureaucracy",
		"Journal pushing runs armed or unarmed",
		"Stopping serve prevents new sweeps",
		"Rex-hosted sweeps can finish independently",
		"Locally hosted sweeps stop with serve",
		"idea or intent",
		"paused|safe|auto",
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

// TestServeCloseRefusesOwnedRun: a web close of a run another moe
// invocation owns must answer 409 and leave the run untouched. The
// prompt gap is the case that matters — an owner can still be deciding
// after its session branch is gone — so this fixture deliberately has
// no session reservation, terminal status or harvest claim: ownership
// is the only thing that can refuse. Driving the real serveOptions
// pins both halves at once: CloseRun's acquisition and the route's
// busy-to-409 mapping. Releasing and retrying proves ownership was the
// obstacle and that the refusal left nothing behind.
func TestServeCloseRefusesOwnedRun(t *testing.T) {
	root := seedCloseFixture(t, "tele", "owned", "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "owned", "design", "# Design\n\nReady.\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var logbuf bytes.Buffer
	srv, err := serve.New(serveOptions(root, &logbuf))
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	// A separate acquisition conflicts even in-process: the guard holds
	// its own file descriptor, exactly as another invocation would.
	h, err := runowner.Acquire(root, "tele", "owned")
	if err != nil {
		t.Fatalf("runowner.Acquire: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	headBefore, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	runJSON := filepath.Join(root, run.Dir("tele", "owned"), "run.json")
	jsonBefore, err := os.ReadFile(runJSON)
	if err != nil {
		t.Fatal(err)
	}

	post := func() *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/tele/owned/close", strings.NewReader("")))
		return rr
	}

	rr := post()
	if rr.Code != http.StatusConflict {
		t.Fatalf("held close: want 409, got %d body=%s log=%q", rr.Code, rr.Body.String(), logbuf.String())
	}
	if !strings.Contains(rr.Body.String(), "owned by another moe invocation") {
		t.Errorf("held close body lacks the ownership reason: %q", rr.Body.String())
	}
	if headAfter, err := git.HEAD(root); err != nil {
		t.Fatal(err)
	} else if headAfter != headBefore {
		t.Errorf("HEAD moved under a refused close: %s -> %s", headBefore, headAfter)
	}
	if jsonAfter, err := os.ReadFile(runJSON); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(jsonAfter, jsonBefore) {
		t.Errorf("run.json changed under a refused close:\nbefore: %s\nafter: %s", jsonBefore, jsonAfter)
	}
	if md, err := run.Load(root, "tele", "owned"); err != nil {
		t.Fatal(err)
	} else if md.Status != run.StatusInProgress {
		t.Fatalf("status=%q after refused close, want in_progress", md.Status)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	rr = post()
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("released close: want 303, got %d body=%s log=%q", rr.Code, rr.Body.String(), logbuf.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/run/tele/owned" {
		t.Errorf("released close redirect=%q, want /run/tele/owned", loc)
	}
	if md, err := run.Load(root, "tele", "owned"); err != nil {
		t.Fatal(err)
	} else if md.Status != run.StatusClosed {
		t.Fatalf("status=%q after released close, want closed", md.Status)
	}
}
