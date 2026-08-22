package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

// seedInputRoot stands up a git-backed bureaucracy with one in-progress
// run — the answer route writes a real journal commit, so a bare TempDir
// won't do.
func seedInputRoot(t *testing.T) string {
	t.Helper()
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed run")
	return root
}

func askOn(t *testing.T, root, slug string) input.Request {
	t.Helper()
	req, err := input.Ask(root, "alpha", slug, "alpha/pulse-one",
		"Which compatibility policy?", []string{"Preserve", "Adopt"}, "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return req
}

// postAnswer drives the answer route with a Referer, which is what picks
// between its two redirect destinations.
func postAnswer(t *testing.T, s *Server, path, referer string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestInboxPageListsOpenQuestions(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	s := newUnarmedTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/inbox")
	mustContain(t, rr,
		"alpha/fix-it",
		"Which compatibility policy?",
		`action="/run/alpha/fix-it/input"`,
		`name="request" value="1"`,
		">Preserve<", ">Adopt<",
	)
}

func TestInboxPageEmptyState(t *testing.T) {
	root := seedInputRoot(t)
	s := newUnarmedTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	mustContain(t, get(t, s, "/inbox"), "(nothing waiting)")
}

// The link is a count where there is something to count — a board with
// nothing waiting gets no new navigation landmark.
func TestDashLinksInboxOnlyWhenNonEmpty(t *testing.T) {
	root := seedInputRoot(t)
	s := newUnarmedTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 1, 0, nil, nil },
	})

	if body := get(t, s, "/").Body.String(); strings.Contains(body, `href="/inbox"`) {
		t.Fatalf("quiet dash advertises the inbox:\n%s", body)
	}
	askOn(t, root, "fix-it")
	mustContain(t, get(t, s, "/"), `href="/inbox">inbox (1)`)
}

// The answer route is journal-only: it writes a commit, redirects, and
// starts nothing.
func TestAnswerRouteWritesCommitAndSpawnsNothing(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postAnswer(t, s, "/run/alpha/fix-it/input", "http://x/inbox",
		url.Values{"request": {"1"}, "choice": {"2"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); !strings.HasPrefix(got, "/inbox") {
		t.Fatalf("Location = %q, want back to the inbox", got)
	}
	if len(s.children.all) != 0 {
		t.Fatalf("the answer route spawned %d children", len(s.children.all))
	}

	f, err := input.Load(root, "alpha", "fix-it")
	if err != nil {
		t.Fatal(err)
	}
	if f.Requests[0].Answer() != "Adopt" {
		t.Fatalf("record = %+v, want Adopt selected", f.Requests[0])
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Answered: alpha/fix-it#1") {
		t.Fatalf("answer commit missing its trailer:\n%s", msg)
	}
}

// A run page answer goes back to the run page, not to the inbox.
func TestAnswerRouteRedirectsToRunWithoutInboxReferer(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postAnswer(t, s, "/run/alpha/fix-it/input", "http://x/run/alpha/fix-it",
		url.Values{"request": {"1"}, "choice": {"1"}})
	if got := rr.Header().Get("Location"); got != "/run/alpha/fix-it" {
		t.Fatalf("Location = %q, want the run page", got)
	}
}

// The stale-tab case the request id exists for: a phone that has been
// showing question #1 since it was answered and replaced answers
// nothing.
func TestAnswerRouteRefusesStaleRequestID(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	if _, err := input.Answer(root, "alpha", "fix-it", 1, 1, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Ask(root, "alpha", "fix-it", "alpha/pulse-two",
		"And the migration?", []string{"In place", "Offline"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postAnswer(t, s, "/run/alpha/fix-it/input", "http://x/inbox",
		url.Values{"request": {"1"}, "choice": {"2"}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	f, _ := input.Load(root, "alpha", "fix-it")
	if f.Requests[1].Selected != 0 {
		t.Fatalf("the newer question was answered by a stale tab: %+v", f.Requests[1])
	}
}

func TestAnswerRouteRefusesOutOfRangeChoice(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postAnswer(t, s, "/run/alpha/fix-it/input", "",
		url.Values{"request": {"1"}, "choice": {"9"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// The run page carries the history and, while the run is live, the
// buttons for its open question.
func TestRunPageShowsInputHistoryAndButtons(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	if _, err := input.Answer(root, "alpha", "fix-it", 1, 1, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Ask(root, "alpha", "fix-it", "alpha/pulse-two",
		"And the migration?", []string{"In place", "Offline"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/run/alpha/fix-it")
	mustContain(t, rr,
		"human inputs",
		"Which compatibility policy?", "Preserve", // answered
		"And the migration?", // open
		`name="request" value="2"`,
	)
}

// A terminal run keeps its history and loses its buttons: there is
// nothing left to discharge.
func TestTerminalRunPageShowsHistoryWithoutButtons(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it")
	md, err := run.Load(root, "alpha", "fix-it")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusMerged
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/run/alpha/fix-it")
	mustContain(t, rr, "human inputs", "Which compatibility policy?")
	if strings.Contains(rr.Body.String(), `/fix-it/input`) {
		t.Fatalf("a terminal run still offers answer buttons:\n%s", rr.Body.String())
	}
	// And it has dropped out of the queue.
	mustContain(t, get(t, s, "/inbox"), "(nothing waiting)")
}
