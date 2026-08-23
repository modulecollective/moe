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
// run — the input routes write real journal commits, so a bare TempDir
// won't do.
func seedInputRoot(t *testing.T) string {
	t.Helper()
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed run")
	return root
}

func askOn(t *testing.T, root, slug, question string) input.Entry {
	t.Helper()
	e, err := input.Ask(root, "alpha", slug, "alpha/pulse-one", question,
		"dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return e
}

// postInput drives the input route with a Referer, which is what picks
// between its two redirect destinations.
func postInput(t *testing.T, s *Server, path, referer string, form url.Values) *httptest.ResponseRecorder {
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

// The queue is the phone surface: every open question with its own reply
// box, posting to that run's route.
func TestInputQueueListsOpenQuestionsWithReplyBoxes(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it", "Which compatibility policy?")
	s := newUnarmedTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	mustContain(t, get(t, s, "/input"),
		"alpha/fix-it",
		"Which compatibility policy?",
		`action="/run/alpha/fix-it/input"`,
		`name="ping" value="1"`,
		`<textarea name="text"`,
	)
}

// Below the questions, what has already been given and is waiting on a
// turn — so one page answers "what needs me, and what's already moving."
func TestInputQueueListsPendingNotesReadOnly(t *testing.T) {
	root := seedInputRoot(t)
	if _, err := input.Add(root, "alpha", "fix-it", "Ship behind the flag.\nDetails follow.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := newUnarmedTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/input")
	mustContain(t, rr, "given, awaiting pickup", "Ship behind the flag.")
	if strings.Contains(rr.Body.String(), "Details follow.") {
		t.Fatalf("the queue relays the whole note:\n%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `name="ping"`) {
		t.Fatalf("a pending note rendered a reply box:\n%s", rr.Body.String())
	}
}

func TestInputQueueEmptyState(t *testing.T) {
	root := seedInputRoot(t)
	s := newUnarmedTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	mustContain(t, get(t, s, "/input"), "(nothing waiting)")
}

// The link is a count where there is something to count — a board with
// nothing asking gets no new navigation landmark. A pending note is not
// something asking: it needs a turn, not a tap.
func TestDashLinksInputOnlyWhenSomethingIsAsking(t *testing.T) {
	root := seedInputRoot(t)
	s := newUnarmedTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 1, 0, nil, nil },
	})

	if body := get(t, s, "/").Body.String(); strings.Contains(body, `href="/input"`) {
		t.Fatalf("quiet dash advertises the queue:\n%s", body)
	}
	if _, err := input.Add(root, "alpha", "fix-it", "a note", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/").Body.String(); strings.Contains(body, `href="/input"`) {
		t.Fatalf("a pending note advertised the queue:\n%s", body)
	}
	askOn(t, root, "fix-it", "Which policy?")
	mustContain(t, get(t, s, "/"), `href="/input">input (1)`)
}

// The reply route is journal-only: it writes a commit, redirects, and
// starts nothing.
func TestReplyWritesCommitAndSpawnsNothing(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it", "Which compatibility policy?")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postInput(t, s, "/run/alpha/fix-it/input", "http://x/input",
		url.Values{"ping": {"1"}, "text": {"Adopt the new default.\r\nIt is already the docs' answer."}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); !strings.HasPrefix(got, "/input") {
		t.Fatalf("Location = %q, want back to the queue", got)
	}
	if len(s.children.all) != 0 {
		t.Fatalf("the reply route spawned %d children", len(s.children.all))
	}

	f, err := input.Load(root, "alpha", "fix-it")
	if err != nil {
		t.Fatal(err)
	}
	// Browsers send \r\n; the record lives on disk as LF.
	if want := "Adopt the new default.\nIt is already the docs' answer."; f.Notes[0].Text != want {
		t.Fatalf("record text = %q, want %q", f.Notes[0].Text, want)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Answered: alpha/fix-it#1") {
		t.Fatalf("answer commit missing its trailer:\n%s", msg)
	}
}

// The same route with no ping id is an unprompted note.
func TestPostWithoutPingIDAddsANote(t *testing.T) {
	root := seedInputRoot(t)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postInput(t, s, "/run/alpha/fix-it/input", "", url.Values{"text": {"Skip the flake."}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/fix-it" {
		t.Fatalf("Location = %q, want the run page", got)
	}
	f, _ := input.Load(root, "alpha", "fix-it")
	if len(f.Notes) != 1 || f.Notes[0].IsPing() || f.Notes[0].Text != "Skip the flake." {
		t.Fatalf("record = %+v, want one bare note", f.Notes)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Added: alpha/fix-it#1") {
		t.Fatalf("note commit missing its trailer:\n%s", msg)
	}
}

func TestPostRefusesEmptyText(t *testing.T) {
	root := seedInputRoot(t)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	rr := postInput(t, s, "/run/alpha/fix-it/input", "", url.Values{"text": {"   "}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// The stale-tab case the ping id exists for: a phone that has been
// showing question #1 since it was answered and replaced answers
// nothing.
func TestReplyRefusesStalePingID(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it", "Which compatibility policy?")
	if _, err := input.Answer(root, "alpha", "fix-it", 1, "Adopt.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	askOn(t, root, "fix-it", "And the migration?")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := postInput(t, s, "/run/alpha/fix-it/input", "http://x/input",
		url.Values{"ping": {"1"}, "text": {"In place."}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	f, _ := input.Load(root, "alpha", "fix-it")
	if f.Notes[1].Text != "" {
		t.Fatalf("the newer question was answered by a stale tab: %+v", f.Notes[1])
	}
}

// The run page carries the history with its delivery markers, a reply
// box for the open question, and a box for an unprompted note.
func TestRunPageShowsInputHistoryAndForms(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it", "Which compatibility policy?")
	if _, err := input.Answer(root, "alpha", "fix-it", 1, "Adopt.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := input.MarkDelivered(root, "alpha", "fix-it", "code", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	askOn(t, root, "fix-it", "And the migration?")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	mustContain(t, get(t, s, "/run/alpha/fix-it"),
		"operator input",
		"Which compatibility policy?", "Adopt.", "read at code",
		"And the migration?", "unanswered",
		`name="ping" value="2"`,
		"push a note at this run",
	)
}

// A run with no record still offers the note box while it is live: an
// unprompted note is the point.
func TestLiveRunPageOffersTheNoteBoxWithNoRecord(t *testing.T) {
	root := seedInputRoot(t)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	mustContain(t, get(t, s, "/run/alpha/fix-it"), "operator input", "push a note at this run")
}

// A terminal run keeps its history and loses its forms: there is no next
// turn to deliver to.
func TestTerminalRunPageShowsHistoryWithoutForms(t *testing.T) {
	root := seedInputRoot(t)
	askOn(t, root, "fix-it", "Which compatibility policy?")
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
	mustContain(t, rr, "operator input", "Which compatibility policy?")
	if strings.Contains(rr.Body.String(), `/fix-it/input`) {
		t.Fatalf("a terminal run still offers input forms:\n%s", rr.Body.String())
	}
	// And it has dropped out of the queue.
	mustContain(t, get(t, s, "/input"), "(nothing waiting)")
}
