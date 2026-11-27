package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
)

// choreServer is serve with the callbacks moe serve wires, over a
// seeded bureaucracy, and a fake claude on PATH that records any call.
func choreServer(t *testing.T, cooldown string) (root string, h http.Handler, claudeRan string) {
	t.Helper()
	root = seedChoreRootWith(t, cooldown)
	claudeRan = filepath.Join(t.TempDir(), "claude-ran")
	fakeClaudeOnPath(t, "#!/bin/sh\ntouch '"+claudeRan+"'\n")
	srv, err := serve.New(serveOptions(root, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	return root, srv.Handler(), claudeRan
}

func do(t *testing.T, h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func headSHA(t *testing.T, root string) string { return gittest.HeadSHA(t, root) }

func TestServeChoreCreateEditDelete(t *testing.T) {
	root, h, _ := choreServer(t, "")

	if rr := do(t, h, "GET", "/projects/moe", nil); !strings.Contains(rr.Body.String(), `href="/projects/moe/chores/new"`) {
		t.Fatal("hub lacks the create link")
	}

	// An invalid create: 422, everything typed echoed, nothing written.
	before := headSHA(t, root)
	bad := url.Values{"name": {"new"}, "trigger": {"*.go"}, "when": {"it rains & pours"}, "prompt": {"line one\r\nline two"}}
	rr := do(t, h, "POST", "/projects/moe/chores/new", bad)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create: %d", rr.Code)
	}
	for _, want := range []string{`value="new"`, `value="*.go"`, `value="it rains &amp; pours"`, "line one\nline two", "exclusive"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("422 body missing %q", want)
		}
	}
	if headSHA(t, root) != before {
		t.Fatal("invalid create committed")
	}

	// An unregistered workflow is refused by the wrapper.
	rr = do(t, h, "POST", "/projects/moe/chores/new", url.Values{"name": {"new"}, "cadence": {"1d"}, "workflow": {"no-such"}})
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "no-such") || headSHA(t, root) != before {
		t.Fatalf("unregistered workflow: %d", rr.Code)
	}

	// A chore named "new" is created and stays reachable at its page.
	rr = do(t, h, "POST", "/projects/moe/chores/new", url.Values{"name": {"new"}, "cadence": {"30d"}, "prompt": {"# new\r\n"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/chore/moe/new" {
		t.Fatalf("create: %d %q %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); strings.TrimSpace(subj) != "Add chore moe/new" {
		t.Fatalf("subject %q", subj)
	}
	if s, _ := chore.LoadSpec(root, "moe", "new"); s != (chore.Spec{Cadence: "30d", Prompt: "# new\n"}) {
		t.Fatalf("spec %+v", s)
	}
	page := do(t, h, "GET", "/chore/moe/new", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `href="/chore/moe/new/edit"`) {
		t.Fatalf("chore page: %d", page.Code)
	}

	// Edit: the form is prefilled; a save lands one commit.
	form := do(t, h, "GET", "/chore/moe/new/edit", nil)
	if form.Code != http.StatusOK || !strings.Contains(form.Body.String(), `value="30d"`) || !strings.Contains(form.Body.String(), `/chore/moe/new/delete`) {
		t.Fatalf("edit form: %d", form.Code)
	}
	rr = do(t, h, "POST", "/chore/moe/new/edit", url.Values{"cadence": {"30d"}, "cooldown": {"7d"}, "prompt": {"# new\n"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/chore/moe/new" {
		t.Fatalf("edit: %d %s", rr.Code, rr.Body.String())
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); strings.TrimSpace(subj) != "Edit chore moe/new" {
		t.Fatalf("subject %q", subj)
	}
	before = headSHA(t, root)
	rr = do(t, h, "POST", "/chore/moe/new/edit", url.Values{"cadence": {"1d"}, "cooldown": {"7d"}})
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), `value="1d"`) || headSHA(t, root) != before {
		t.Fatalf("invalid edit: %d", rr.Code)
	}
	if rr := do(t, h, "GET", "/chore/moe/ghost/edit", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("missing edit form: %d", rr.Code)
	}
	if rr := do(t, h, "POST", "/chore/moe/ghost/edit", url.Values{"cadence": {"1d"}}); rr.Code != http.StatusNotFound {
		t.Fatalf("missing edit submit: %d", rr.Code)
	}

	// Delete lands on the hub.
	rr = do(t, h, "POST", "/chore/moe/new/delete", url.Values{})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/projects/moe" {
		t.Fatalf("delete: %d", rr.Code)
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); strings.TrimSpace(subj) != "Remove chore moe/new" {
		t.Fatalf("subject %q", subj)
	}
	if rr := do(t, h, "POST", "/chore/moe/new/delete", url.Values{}); rr.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rr.Code)
	}
}

// Open now: a cooling-down, not-due chore opens through the real
// pipeline, with MoE-Chore and no MoE-Consent on the open commit, and
// no agent starts. A second open is a 409 with HEAD unchanged.
func TestServeChoreOpenNow(t *testing.T) {
	root, h, claudeRan := choreServer(t, "7d")
	if choreDue(t, root, "readme-refresh") {
		t.Fatal("precondition: not due")
	}
	rr := do(t, h, "POST", "/chore/moe/readme-refresh/open", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("open: %d %s", rr.Code, rr.Body.String())
	}
	openRun := choreOpenRun(t, root, "readme-refresh")
	if openRun == "" || rr.Header().Get("Location") != "/run/moe/"+openRun {
		t.Fatalf("Location %q, open run %q", rr.Header().Get("Location"), openRun)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Chore: moe/readme-refresh") || strings.Contains(msg, "MoE-Consent") {
		t.Fatalf("open commit trailers:\n%s", msg)
	}
	// The kick reads MoE-Chore as the operator's standing intent.
	md, err := run.Load(root, "moe", openRun)
	if err != nil {
		t.Fatal(err)
	}
	mds, _ := run.Scan(root)
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if !operatorMarked(root, md, mds, idx) {
		t.Fatal("the opened run is not operator-marked for the kick")
	}

	before := headSHA(t, root)
	rr = do(t, h, "POST", "/chore/moe/readme-refresh/open", url.Values{})
	if rr.Code != http.StatusConflict || headSHA(t, root) != before {
		t.Fatalf("second open: %d", rr.Code)
	}
	if rr := do(t, h, "POST", "/chore/moe/ghost/open", url.Values{}); rr.Code != http.StatusNotFound {
		t.Fatalf("missing chore open: %d", rr.Code)
	}
	if _, err := os.Stat(claudeRan); !os.IsNotExist(err) {
		t.Fatal("open now started an agent")
	}
}
