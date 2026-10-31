package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func reportSlot(rs []cliout.ProgramStatus, slot string) cliout.ProgramStatus {
	for _, r := range rs {
		if r.ID == "moe/dash/"+slot {
			return r
		}
	}
	return cliout.ProgramStatus{}
}

func TestDashStatusProjection(t *testing.T) {
	snap := DashSnapshot{Questions: map[string]string{"tele/fix": "Which version?"}, Rows: []dash.Row{
		{Project: "tele", Run: "fix", RunningDoc: "code"}, {Project: "tele", Run: "fix", RunningDoc: "code"},
	}}
	st := serveState{ok: true, live: serveAlive, snap: serve.ActivitySnapshot{Projects: []serve.ActivityProject{{Project: "tele", SweepStarted: time.Now()}}}}
	rs := dashStatusReports(snap, nil, st, DashFilter{})
	if r := reportSlot(rs, "activity"); r.State != "working" || r.Message != "Live sweeps: tele" {
		t.Fatalf("activity = %+v", r)
	}
	if r := reportSlot(rs, "questions"); r.State != "blocked" || r.Kind != "question" || !strings.Contains(r.Title, "operator questions") || !strings.Contains(r.Message, "1 outstanding operator questions: tele/fix: Which version?") {
		t.Fatalf("questions = %+v", r)
	}
	st.snap.Projects = nil
	rs = dashStatusReports(snap, nil, st, DashFilter{})
	if r := reportSlot(rs, "activity"); r.State != "idle" || r.Message != "1 open run sessions; agent activity unknown" {
		t.Fatalf("session claim = %+v", r)
	}
	// Age and start-time changes are not status changes.
	st.snap.Started = time.Now().Add(-time.Hour)
	if got := reportSlot(dashStatusReports(snap, nil, st, DashFilter{}), "activity"); got != reportSlot(rs, "activity") {
		t.Fatal("timestamp affected report")
	}
}

func TestDashStatusHealthAndFilters(t *testing.T) {
	for _, tc := range []struct {
		name     string
		st       serveState
		want     string
		activity string
	}{
		{"never run", serveState{}, "", "idle"},
		{"dead", serveState{ok: true, live: serveDead}, "serve process is dead", ""},
		{"unknown", serveState{ok: true, live: serveUnknown}, "liveness unknown", ""},
		{"unreadable", serveState{err: errors.New("bad snapshot")}, "observation failed: bad snapshot", ""},
		{"host unusable", serveState{ok: true, live: serveAlive, snap: serve.ActivitySnapshot{Host: "rex unusable", HostError: "rex API revision 7 is older than 8"}}, "Serve-wide: rex host unusable: rex API revision 7 is older than 8", "idle"},
		{"failed", serveState{ok: true, live: serveAlive, snap: serve.ActivitySnapshot{Projects: []serve.ActivityProject{{Project: "other", SweptAt: time.Now(), SweepClean: false}}}}, "last sweep failed for other", "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := dashStatusReports(DashSnapshot{Questions: map[string]string{"tele/fix": "Answer?"}}, nil, tc.st, DashFilter{ProjectFilter: "tele"})
			if r := reportSlot(rs, "activity"); r.State != tc.activity {
				t.Fatalf("activity = %+v", r)
			}
			health := reportSlot(rs, "health")
			if tc.want == "" {
				if health.State != "" {
					t.Fatalf("unexpected health: %+v", health)
				}
			} else if health.State != "error" || !strings.Contains(health.Message, tc.want) || !strings.Contains(health.Title, "serve-wide") {
				t.Fatalf("health = %+v", health)
			}
			if reportSlot(rs, "questions").State != "blocked" {
				t.Fatal("health hid independently known question")
			}
		})
	}
	st := serveState{ok: true, live: serveAlive, snap: serve.ActivitySnapshot{Projects: []serve.ActivityProject{{Project: "tele", SweepStarted: time.Now()}, {Project: "other", SweepStarted: time.Now()}}}}
	for _, tc := range []struct {
		filter DashFilter
		want   string
	}{{DashFilter{ProjectFilter: "tele"}, "Live sweeps: tele"}, {DashFilter{WorkflowFilter: "sdlc"}, "No live sweeps or open sessions observed"}, {DashFilter{WorkflowFilter: "pulse"}, "Live sweeps: other, tele"}} {
		if r := reportSlot(dashStatusReports(DashSnapshot{}, nil, st, tc.filter), "activity"); r.Message != tc.want {
			t.Fatalf("filter %+v: %+v", tc.filter, r)
		}
	}
	for _, snap := range []DashSnapshot{{InputErrors: []error{errors.New("broken input")}}, {SessionError: errors.New("broken sessions")}} {
		rs := dashStatusReports(snap, nil, serveState{}, DashFilter{})
		if reportSlot(rs, "health").State != "error" || reportSlot(rs, "questions").State != "" {
			t.Fatalf("read error = %+v", rs)
		}
		if snap.SessionError != nil && reportSlot(rs, "activity").State != "" {
			t.Fatal("unknown sessions became idle")
		}
	}
	rs := dashStatusReports(DashSnapshot{}, errors.New("scan failed"), serveState{}, DashFilter{})
	if reportSlot(rs, "activity").State != "" || reportSlot(rs, "health").State != "error" {
		t.Fatalf("scan failure = %+v", rs)
	}
}

type dashStatusSink struct {
	bytes.Buffer
	writes int
	fail   bool
}

func (w *dashStatusSink) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *dashStatusSink) Write(p []byte) (int, error) {
	w.writes++
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(p)
}

func TestDashStatusChangesAndLifetime(t *testing.T) {
	w := &dashStatusSink{}
	s := dashStatus{w: w}
	r := cliout.ProgramStatus{ID: "moe/dash/questions", State: "blocked", Kind: "question", App: "moe", Title: "Questions", Message: "Answer?"}
	update := func(reports []cliout.ProgramStatus) {
		t.Helper()
		if err := s.update(reports); err != nil {
			t.Fatal(err)
		}
	}
	update([]cliout.ProgramStatus{r})
	initial := w.String()
	if w.writes != 2 || !strings.HasPrefix(initial, "\x1b]7501;state=clear:id=moe/dash\x1b\\") {
		t.Fatalf("initial snapshot = %q (%d writes)", initial, w.writes)
	}
	for range 100 {
		update([]cliout.ProgramStatus{r})
	}
	if w.String() != initial {
		t.Fatal("unchanged refresh re-emitted")
	}
	changed := r
	changed.Message = "Changed?"
	update([]cliout.ProgramStatus{changed})
	changed.State = "idle"
	changed.Kind = ""
	update([]cliout.ProgramStatus{changed})
	update([]cliout.ProgramStatus{r})
	if w.writes != 5 {
		t.Fatalf("change and return writes = %d", w.writes)
	}
	update(nil)
	update(nil)
	if w.writes != 6 || !strings.HasSuffix(w.String(), "\x1b]7501;state=clear:id=moe/dash/questions\x1b\\") {
		t.Fatalf("removal writes = %d", w.writes)
	}
	s.close()
	if w.writes != 7 {
		t.Fatal("exit did not clear owned subtree")
	}
	s = dashStatus{w: w}
	update([]cliout.ProgramStatus{r})
	if w.writes != 9 {
		t.Fatal("restart did not clear and seed")
	}
	// Changes beyond the normalized UTF-8 limit do not cause writes.
	r.Message = strings.Repeat("a", 2048) + "x"
	update([]cliout.ProgramStatus{r})
	n := w.writes
	r.Message = strings.Repeat("a", 2048) + "y"
	update([]cliout.ProgramStatus{r})
	if w.writes != n {
		t.Fatal("compared unnormalized record")
	}
}

func TestDashStatusFailedWritesRetry(t *testing.T) {
	w := &dashStatusSink{fail: true}
	s := dashStatus{w: w}
	r := cliout.ProgramStatus{ID: "moe/dash/activity", State: "idle", App: "moe"}
	if err := s.update([]cliout.ProgramStatus{r}); err == nil || s.started {
		t.Fatal("failed reset acknowledged")
	}
	w.fail = false
	if err := s.update([]cliout.ProgramStatus{r}); err != nil {
		t.Fatal(err)
	}
	w.fail = true
	r.Message = "new"
	if err := s.update([]cliout.ProgramStatus{r}); err == nil {
		t.Fatal("failed write succeeded")
	}
	if s.last[r.ID].Message == "new" {
		t.Fatal("failed update acknowledged")
	}
	if err := s.update(nil); err == nil || len(s.last) != 1 {
		t.Fatal("failed removal acknowledged")
	}
	w.fail = false
	if err := s.update([]cliout.ProgramStatus{r}); err != nil || s.last[r.ID] != r {
		t.Fatalf("retry = %v, %+v", err, s.last)
	}
	if err := s.update(nil); err != nil || len(s.last) != 0 {
		t.Fatal("removal retry failed")
	}
}

func TestGatherDashQuestionsValidityAndFilters(t *testing.T) {
	root := newTestBureaucracy(t)
	for _, tc := range []struct {
		project, slug, wf string
		status            string
	}{
		{"tele", "live", "sdlc", run.StatusInProgress}, {"tele", "pushed", "sdlc", run.StatusPushed}, {"tele", "intent", "intent", run.StatusInProgress}, {"other", "live", "sdlc", run.StatusInProgress}, {"tele", "closed", "sdlc", run.StatusClosed},
	} {
		trailerstest.SeedRun(t, root, tc.project, tc.slug, tc.wf, tc.status)
		path := filepath.Join(root, input.Path(tc.project, tc.slug))
		if err := os.WriteFile(path, []byte(`{"notes":[{"id":1,"question":"Answer?"}]}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gather := func(filter DashFilter) DashSnapshot {
		t.Helper()
		snap, err := GatherDashSnapshot(root, time.Now(), filter)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	snap := gather(DashFilter{ProjectFilter: "tele", WorkflowFilter: "sdlc"})
	if len(snap.Questions) != 2 || snap.Questions["tele/live"] != "Answer?" || snap.Questions["tele/pushed"] != "Answer?" || len(snap.InputErrors) != 0 {
		t.Fatalf("questions = %+v, errors = %v", snap.Questions, snap.InputErrors)
	}
	path := filepath.Join(root, input.Path("tele", "live"))
	if err := os.WriteFile(path, []byte(`{"notes":[{"id":1,"question":"Answer?","text":"Yes"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := gather(DashFilter{ProjectFilter: "tele", WorkflowFilter: "sdlc"}); len(got.Questions) != 1 {
		t.Fatalf("answer did not remove question: %v", got.Questions)
	}
	if err := os.WriteFile(path, []byte(`broken`), 0600); err != nil {
		t.Fatal(err)
	}
	snap = gather(DashFilter{ProjectFilter: "tele", WorkflowFilter: "sdlc"})
	if len(snap.InputErrors) != 1 || len(snap.Questions) != 1 {
		t.Fatalf("malformed input lost validity: %+v", snap)
	}
	if got := gather(DashFilter{WorkflowFilter: "intent"}); len(got.Questions) != 1 || len(got.InputErrors) != 0 {
		t.Fatalf("workflow filter = %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := gather(DashFilter{ProjectFilter: "tele", WorkflowFilter: "sdlc"}); len(got.InputErrors) != 0 || len(got.Questions) != 1 {
		t.Fatalf("recovery = %+v", got)
	}
}
