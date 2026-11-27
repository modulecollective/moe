package serve

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/servepause"
)

// pulseServer is a git-backed board whose gather shows one closed pulse
// run, so the section has a last pulse to link.
func pulseServer(t *testing.T, armed bool) *Server {
	t.Helper()
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	now := time.Now()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return []dash.Row{{Project: "_cabinet", Run: "pulse-7", Workflow: dash.PulseWorkflow,
				Bucket: dash.BucketCompletedRuns, When: now.Add(-12 * time.Minute)}}, 1, 0, nil, nil
		},
		Dynamic: armed,
	})
	gittest.InitAt(t, root)
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed")
	return s
}

func pulseSection(t *testing.T, s *Server) string {
	t.Helper()
	body := getBody(t, s, "/")
	start := strings.Index(body, `id="pulse"`)
	end := strings.Index(body, `<span>active`)
	if start < 0 || end < start {
		t.Fatalf("no PULSE section above active:\n%s", body)
	}
	return body[start:end]
}

// The composer's POST is one journal commit and a 303 back to the
// section; a blank note is a 400 that commits nothing.
func TestPulseNotePostCommitsAndRedirects(t *testing.T) {
	s := pulseServer(t, true)
	root := s.opts.Root
	before := gittest.Output(t, root, "rev-list", "--count", "HEAD")

	rec := postForm(t, s, "/pulse/note", "text="+strings.ReplaceAll("bump Go everywhere\n\nstart with moe", "\n", "%0A"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/#pulse" {
		t.Fatalf("POST /pulse/note = %d %q, want 303 to /#pulse", rec.Code, rec.Header().Get("Location"))
	}
	if subj := gittest.Output(t, root, "log", "-1", "--format=%s"); strings.TrimSpace(subj) != "input: pulse note #1" {
		t.Fatalf("tip = %q, want the note commit", subj)
	}
	if after := gittest.Output(t, root, "rev-list", "--count", "HEAD"); strings.TrimSpace(after) == strings.TrimSpace(before) {
		t.Fatal("no commit landed")
	}
	f, _ := input.LoadPulse(root)
	if len(f.Notes) != 1 || f.Notes[0].Text != "bump Go everywhere\n\nstart with moe" {
		t.Fatalf("record = %+v", f.Notes)
	}

	head := gittest.Output(t, root, "rev-parse", "HEAD")
	if rec := postForm(t, s, "/pulse/note", "text=+++"); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank POST = %d, want 400", rec.Code)
	}
	if got := gittest.Output(t, root, "rev-parse", "HEAD"); got != head {
		t.Fatal("a blank note committed")
	}
}

// The section in its states: no notes, a pending one with the
// countdown, read ones linking the run that read them (three at most),
// a pause, and a serve that isn't armed.
func TestDashPulseSectionStates(t *testing.T) {
	s := pulseServer(t, true)
	root := s.opts.Root

	sec := pulseSection(t, s)
	for _, want := range []string{
		`data-serve-cluster>serve armed`,
		`last <a href="/run/_cabinet/pulse-7">_cabinet/pulse-7</a> 12m ago`,
		`id="serve-pause"`, `action="/serve/pause"`,
		`action="/pulse/note"`, `placeholder="tell the next pulse — work to start, what comes first"`,
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("empty section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "pulse-notes") {
		t.Errorf("a board with no notes rendered a notes list:\n%s", sec)
	}

	for _, text := range []string{"one", "two", "three", "four"} {
		if _, err := input.AddPulse(root, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := input.MarkPulseDelivered(root, "_cabinet/pulse-7", []int{1, 2, 3, 4}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := input.AddPulse(root, "do the lightpool fix next\nit blocks the release"); err != nil {
		t.Fatal(err)
	}
	sec = pulseSection(t, s)
	if !strings.Contains(sec, "#5</span>\n        <span class=\"pulse-state\" data-pulse-waiting>▒ waiting · next check") ||
		!strings.Contains(sec, "do the lightpool fix next\nit blocks the release") {
		t.Errorf("pending note not shown in full with its countdown:\n%s", sec)
	}
	if strings.Count(sec, `░ read by <a href="/run/_cabinet/pulse-7">`) != 3 || strings.Contains(sec, "#1</span>") {
		t.Errorf("want the last three read notes, newest first:\n%s", sec)
	}
	if strings.Index(sec, "#4</span>") > strings.Index(sec, "#2</span>") {
		t.Errorf("read notes are not newest first:\n%s", sec)
	}

	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if sec = pulseSection(t, s); !strings.Contains(sec, "▒ waiting · new automatic sweeps paused until") || !strings.Contains(sec, `action="/serve/unpause"`) {
		t.Errorf("paused section does not say what holds the note:\n%s", sec)
	}

	off := pulseServer(t, false)
	if _, err := input.AddPulse(off.opts.Root, "x"); err != nil {
		t.Fatal(err)
	}
	if sec = pulseSection(t, off); !strings.Contains(sec, "▒ waiting · serve is not armed — `moe pulse new`") || !strings.Contains(sec, "serve browse-only") {
		t.Errorf("browse-only section does not say no sweep is coming:\n%s", sec)
	}
}

// /input carries the whole pulse-note history, newest first, with no
// second composer; a project-scoped view leaves it out.
func TestInputPageListsPulseNoteHistory(t *testing.T) {
	s := pulseServer(t, true)
	root := s.opts.Root
	for _, text := range []string{"first", "second"} {
		if _, err := input.AddPulse(root, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := input.MarkPulseDelivered(root, "_cabinet/pulse-7", []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, s, "/input")
	for _, want := range []string{"for the pulse", "▒ waiting", `░ read by <a href="/run/_cabinet/pulse-7">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/input lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, ">second<") > strings.Index(body, ">first<") {
		t.Errorf("history is not newest first:\n%s", body)
	}
	if strings.Contains(body, `action="/pulse/note"`) {
		t.Error("/input grew a second composer")
	}
	if body := getBody(t, s, "/input?project=alpha"); strings.Contains(body, "for the pulse") {
		t.Error("a project-scoped /input lists pulse notes")
	}
}

// The five-second refresh carries the header clock beside the cluster.
func TestRunStatusCarriesTheClock(t *testing.T) {
	s := pulseServer(t, true)
	var m map[string]any
	if err := json.Unmarshal([]byte(getBody(t, s, "/run-status")), &m); err != nil {
		t.Fatal(err)
	}
	if m["__clock"] != dash.Clock(time.Now()) && m["__clock"] != dash.Clock(time.Now().Add(-time.Minute)) {
		t.Fatalf("__clock = %v", m["__clock"])
	}
}

func TestPulseQuotaRunningAndPrimaryBrakes(t *testing.T) {
	s := pulseServer(t, true)
	now := time.Now()
	s.opts.SurveyAdmission = func(time.Time) run.SurveyAdmission {
		return run.SurveyAdmission{Backend: "claude", Reason: "quota exhausted", NotBefore: now.Add(time.Hour)}
	}
	if _, err := input.AddPulse(s.opts.Root, "pending"); err != nil {
		t.Fatal(err)
	}
	if sec := pulseSection(t, s); !strings.Contains(sec, "retry after") || !strings.Contains(sec, "pending") {
		t.Fatal(sec)
	}
	if _, err := servepause.Set(s.opts.Root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if sec := pulseSection(t, s); !strings.Contains(sec, "waiting · new automatic sweeps paused") || !strings.Contains(sec, "retry after") {
		t.Fatal(sec)
	}
	s.activity.recordSweepStart("_cabinet", now)
	if sec := pulseSection(t, s); !strings.Contains(sec, "survey running") || strings.Contains(sec, "retry after") {
		t.Fatal(sec)
	}
}
