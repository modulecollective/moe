package input

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// seedRoot builds a bureaucracy with one project and one in-progress
// run. The record itself is what these tests exercise, so nothing here
// writes an inputs.json — Ask does.
func seedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	write(t, root, "bureaucracy.conf", "")
	write(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed bureaucracy")
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedRun(t *testing.T, root, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, "moe", run.Options{ID: slug, Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func ask(t *testing.T, root, slug string) Request {
	t.Helper()
	req, err := Ask(root, "moe", slug, "moe/pulse-one",
		"Which compatibility policy?", []string{"Preserve", "Adopt"}, "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return req
}

// A question round-trips through the file and lands as one commit
// carrying the ask trailer and the sweep's consent.
func TestAskWritesRecordAndTrailer(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")

	req := ask(t, root, "change-auth")
	if req.ID != 1 || req.Answered() {
		t.Fatalf("req = %+v, want id 1 unanswered", req)
	}

	f, err := Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	open, ok := f.Open()
	if !ok || open.Question != "Which compatibility policy?" {
		t.Fatalf("Open() = %+v, %v", open, ok)
	}

	body, err := os.ReadFile(filepath.Join(root, Path("moe", "change-auth")))
	if err != nil {
		t.Fatal(err)
	}
	// selected omits on disk: an open request's JSON says nothing about
	// an answer rather than saying "0".
	if strings.Contains(string(body), "selected") {
		t.Fatalf("inputs.json carries a selected key while unanswered:\n%s", body)
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Asked: moe/change-auth#1") {
		t.Fatalf("ask commit missing its trailer:\n%s", msg)
	}
	if !strings.Contains(msg, "MoE-Consent: dynamic") {
		t.Fatalf("ask commit missing the sweep's consent:\n%s", msg)
	}
}

// The duplicate case: one open request per run, so a second ask is
// refused rather than overwriting or stacking.
func TestAskRefusesDuplicateOpenRequest(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")

	_, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"Something else?", []string{"Yes", "No"}, "dynamic", io.Discard, io.Discard)
	if !errors.Is(err, ErrOpenRequest) {
		t.Fatalf("second Ask err = %v, want ErrOpenRequest", err)
	}
	f, _ := Load(root, "moe", "change-auth")
	if len(f.Requests) != 1 {
		t.Fatalf("requests = %d, want the refused ask not to have landed", len(f.Requests))
	}
}

// Once the first is answered the next question is allowed — the rule is
// one *open* request, not one ever.
func TestAskAllowedAfterAnswer(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 0, 2, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	req, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"And the migration?", []string{"In place", "Offline"}, "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("follow-up Ask: %v", err)
	}
	if req.ID != 2 {
		t.Fatalf("req.ID = %d, want 2", req.ID)
	}
}

// The terminal case, on both verbs: a run that has stopped can neither
// be asked nor answered, because nothing would discharge the question.
func TestTerminalRunRefusesAskAndAnswer(t *testing.T) {
	root := seedRoot(t)
	md := seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")

	md.Status = run.StatusMerged
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	if _, err := Answer(root, "moe", "change-auth", 0, 1, io.Discard, io.Discard); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Answer on merged run = %v, want ErrNotLive", err)
	}
	if _, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"Anything?", []string{"a", "b"}, "dynamic", io.Discard, io.Discard); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Ask on merged run = %v, want ErrNotLive", err)
	}
}

// The stale case: the web posts the id it rendered, so a tab sitting on
// a question that has since been answered and replaced answers nothing.
func TestAnswerRefusesStaleRequestID(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 1, 1, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"And the migration?", []string{"In place", "Offline"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The stale tab still thinks #1 is open.
	if _, err := Answer(root, "moe", "change-auth", 1, 2, io.Discard, io.Discard); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("stale Answer = %v, want ErrStaleRequest", err)
	}
	f, _ := Load(root, "moe", "change-auth")
	if got := f.Requests[1].Selected; got != 0 {
		t.Fatalf("request 2 selected = %d, want it untouched", got)
	}
}

func TestAnswerRefusesOutOfRangeChoice(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	for _, choice := range []int{0, 3, -1} {
		if _, err := Answer(root, "moe", "change-auth", 0, choice, io.Discard, io.Discard); !errors.Is(err, ErrChoiceOutOfRange) {
			t.Fatalf("Answer(choice=%d) = %v, want ErrChoiceOutOfRange", choice, err)
		}
	}
}

func TestAnswerWithNothingOpen(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 0, 1, io.Discard, io.Discard); !errors.Is(err, ErrNoOpenRequest) {
		t.Fatalf("Answer with no record = %v, want ErrNoOpenRequest", err)
	}
}

func TestAnswerCommitsTrailerWithoutConsent(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	req, err := Answer(root, "moe", "change-auth", 1, 2, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if req.Answer() != "Adopt" {
		t.Fatalf("Answer() = %q, want %q", req.Answer(), "Adopt")
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Answered: moe/change-auth#1") {
		t.Fatalf("answer commit missing its trailer:\n%s", msg)
	}
	// The answer is the operator's own act, so it carries no ride level.
	if strings.Contains(msg, "MoE-Consent:") {
		t.Fatalf("answer commit stamped consent:\n%s", msg)
	}
}

// The grammar is refused before anything reaches disk, and the same
// function the pulse gate validates with is the one that says so.
func TestValidateQuestion(t *testing.T) {
	cases := []struct {
		name     string
		question string
		choices  []string
		ok       bool
	}{
		{"two choices", "Which?", []string{"a", "b"}, true},
		{"three choices", "Which?", []string{"a", "b", "c"}, true},
		{"empty question", "  ", []string{"a", "b"}, false},
		{"one choice", "Which?", []string{"a"}, false},
		{"four choices", "Which?", []string{"a", "b", "c", "d"}, false},
		{"empty choice", "Which?", []string{"a", " "}, false},
		{"duplicate choice", "Which?", []string{"a", "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateQuestion(tc.question, tc.choices)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateQuestion = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// A record that violates the invariants refuses loudly. It is
// machine-written, so a violation is a bug to see rather than noise to
// route around — every floor keys on this error.
func TestLoadRefusesMalformedRecord(t *testing.T) {
	cases := map[string]string{
		"two open":     `{"requests":[{"id":1,"question":"a?","choices":["x","y"]},{"id":2,"question":"b?","choices":["x","y"]}]}`,
		"sparse ids":   `{"requests":[{"id":7,"question":"a?","choices":["x","y"],"selected":1}]}`,
		"bad question": `{"requests":[{"id":1,"question":"","choices":["x","y"],"selected":1}]}`,
		"selected oob": `{"requests":[{"id":1,"question":"a?","choices":["x","y"],"selected":5}]}`,
		"not json":     `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "change-auth")
			write(t, root, Path("moe", "change-auth"), body)
			if _, err := Load(root, "moe", "change-auth"); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Load = %v, want ErrMalformed", err)
			}
		})
	}
}

// A run with no record is the common case and costs nothing.
func TestLoadMissingRecordIsEmpty(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	f, err := Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatalf("Load on a run with no record: %v", err)
	}
	if _, ok := f.Open(); ok || len(f.Requests) != 0 {
		t.Fatalf("f = %+v, want empty", f)
	}
}

// Scan is the inbox: open requests on live runs only, and a terminal run
// drops out even though its history stays on disk.
// The slugs are chosen against the alphabetical tie-break: `zulu` is
// asked first and must list first, so a Scan that ignored the ask time
// would fail rather than accidentally pass.
func TestScanListsOpenRequestsOldestFirstAndDropsTerminalRuns(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "alpha")
	seedRun(t, root, "zulu")
	gone := seedRun(t, root, "gone")

	// Pin the ask commits apart: LastFileActivity reads %ct, so two asks
	// inside one second would tie and fall through to the slug order the
	// slugs above are chosen to contradict.
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-01T10:00:00+00:00")
	ask(t, root, "zulu")
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-02T10:00:00+00:00")
	ask(t, root, "alpha")
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-03T10:00:00+00:00")
	ask(t, root, "gone")
	os.Unsetenv("GIT_COMMITTER_DATE")

	gone.Status = run.StatusClosed
	if err := run.Save(root, gone); err != nil {
		t.Fatal(err)
	}

	pending, errs := Scan(root, "")
	if len(errs) != 0 {
		t.Fatalf("Scan errs = %v", errs)
	}
	var got []string
	for _, p := range pending {
		got = append(got, p.Run)
	}
	if len(got) != 2 || got[0] != "zulu" || got[1] != "alpha" {
		t.Fatalf("Scan = %v, want [zulu alpha] — oldest first, terminal dropped", got)
	}
	// The terminal run keeps its history for its own page.
	f, err := Load(root, "moe", "gone")
	if err != nil || len(f.Requests) != 1 {
		t.Fatalf("terminal run's history = %+v, %v", f, err)
	}
}

func TestScanFiltersByProject(t *testing.T) {
	root := seedRoot(t)
	write(t, root, "projects/other/project.json", `{"id":"other"}`)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed second project")
	seedRun(t, root, "mine")
	if _, err := run.New(root, "other", run.Options{ID: "theirs", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	ask(t, root, "mine")
	if _, err := Ask(root, "other", "theirs", "other/pulse",
		"Which?", []string{"a", "b"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	pending, _ := Scan(root, "moe")
	if len(pending) != 1 || pending[0].Project != "moe" {
		t.Fatalf("Scan(moe) = %+v, want only moe's", pending)
	}
}

// One unreadable record degrades to one missing row, not an empty inbox.
func TestScanSkipsMalformedRecordWithError(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "good")
	seedRun(t, root, "bad")
	ask(t, root, "good")
	write(t, root, Path("moe", "bad"), `{`)

	pending, errs := Scan(root, "")
	if len(pending) != 1 || pending[0].Run != "good" {
		t.Fatalf("Scan = %+v, want just the readable one", pending)
	}
	if len(errs) != 1 {
		t.Fatalf("Scan errs = %v, want one", errs)
	}
}
