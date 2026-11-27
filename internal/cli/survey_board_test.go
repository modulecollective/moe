package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/servepause"
)

func TestSurveyQuotaOnWebDashAndUI(t *testing.T) {
	root := surveyFixture(t)
	saved := time.Local
	time.Local = time.FixedZone("PST", -8*3600)
	t.Cleanup(func() { time.Local = saved })
	now := time.Now()
	e := run.QuotaRetry{Survey: true, Project: project.RootID, Token: "_cabinet/hold", NotBefore: now.Add(time.Hour), Targets: []run.QuotaTarget{{Backend: "claude", Model: "", Reason: "quota exhausted"}}}
	if err := commitSurveyRetry(root, e); err != nil {
		t.Fatal(err)
	}
	if _, err := input.AddPulse(root, "keep this note"); err != nil {
		t.Fatal(err)
	}
	rows := []dash.Row{{Project: project.RootID, Run: "last-real", Workflow: pulseWorkflow, Bucket: dash.BucketCompletedRuns, When: now.Add(-time.Hour)}}
	opts := serveOptions(root, io.Discard)
	opts.Dynamic = true
	opts.GatherDash = func(string) ([]dash.Row, int, int, []int, error) { return rows, 2, 0, nil, nil }
	srv, err := serve.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"waiting", "due", "changed selection", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			want := "retry after"
			switch scenario {
			case "due":
				e.NotBefore = now.Add(-time.Hour)
				if err := commitSurveyRetry(root, e); err != nil {
					t.Fatal(err)
				}
				want = "eligible for quota recheck"
			case "changed selection":
				if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
					t.Fatal(err)
				}
				want = ""
			case "unreadable":
				gittest.WriteAndCommit(t, root, agentdefault.FileName, "bad", "malformed setting")
				want = "survey quota status unavailable"
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, text := range []string{"data-pulse-quota", "/run/_cabinet/last-real", "keep this note"} {
				if !strings.Contains(body, text) {
					t.Fatal("web missing", text)
				}
			}
			refresh := httptest.NewRecorder()
			srv.Handler().ServeHTTP(refresh, httptest.NewRequest(http.MethodGet, "/run-status", nil))
			state := readServeState(root)
			var out bytes.Buffer
			notes, _ := input.LoadPulse(root)
			dash.RenderPulse(&out, now, state.pulse(now, rows, notes, ""))
			source := newUISource(root, DashFilter{}, nil)
			source.snap = DashSnapshot{Rows: rows}
			source.serveNow = state
			board, _ := source.buildBoard(now)
			terminal := out.String() + strings.Join(board.Lines, "\n")
			if want != "" {
				for _, render := range []string{body, refresh.Body.String(), terminal} {
					if !strings.Contains(render, want) {
						t.Fatalf("%s missing %q: %s", scenario, want, render)
					}
				}
			} else if strings.Contains(body, "retry after") || strings.Contains(terminal, "retry after") || strings.Contains(refresh.Body.String(), "retry after") {
				t.Fatal("old backend still blocks new selection")
			}
			if scenario == "waiting" && (!strings.Contains(body, "PST") || !strings.Contains(terminal, "PST")) {
				t.Fatal("missing local zone")
			}
		})
	}
	// Unreadable quota remains additional context when globally paused/stopped.
	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	state := readServeState(root)
	notes, _ := input.LoadPulse(root)
	pulse := state.pulse(now, rows, notes, "")
	if !strings.Contains(pulse.Waiting, "paused until") || !strings.Contains(pulse.Quota, "unavailable") {
		t.Fatal(pulse)
	}
	// Running replaces recorded quota context without adding routine activity rows.
	state.ok, state.live = true, serveAlive
	state.snap.Projects = []serve.ActivityProject{{Project: project.RootID, SweepStarted: now}}
	if got := state.pulse(now, rows, notes, "").Quota; got != "survey running" {
		t.Fatal(got)
	}
}
