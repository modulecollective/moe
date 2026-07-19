package cli

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/serve"
)

// The serve-side chip tests run against a hand-written WorkflowUI stub
// (internal/serve can't import internal/cli — the dependency runs the
// other way), and the cli-side tests assert lookupServeWorkflowUI's
// return value without rendering. Neither composes the two, so a
// workflow could derive Cascade correctly and still not render chips.
// These tests wire the real lookup into a real serve.Server and read
// the rendered run page — the seam the operator actually clicks.
func TestServeRunPageChipsComposeWithRealLookup(t *testing.T) {
	cases := []struct {
		workflow  string
		nextStage string
		wantChips bool
	}{
		{workflow: "twin", nextStage: "architecture", wantChips: true},
		{workflow: "kb", nextStage: "summarize", wantChips: true},
		{workflow: "hooks", nextStage: "code", wantChips: true},
		{workflow: "chores", nextStage: "code", wantChips: true},
		// Undeclared workflows stay read-only: no serve declaration, so
		// no cascade chips regardless of their CLI dispatcher.
		{workflow: "idea", nextStage: "idea", wantChips: false},
		{workflow: "pulse", nextStage: "pulse", wantChips: false},
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			root := t.TempDir()
			seedServeRun(t, root, "alpha", "r1", tc.workflow)
			now := time.Now().UTC()
			srv, err := serve.New(serve.Options{
				Addr:       "127.0.0.1:0",
				Root:       root,
				Logger:     io.Discard,
				Insecure:   true,
				WorkflowUI: lookupServeWorkflowUI,
				GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
					return dash.Row{Project: p, Run: slug,
						Note:   tc.workflow + ":" + tc.nextStage,
						Stage:  tc.nextStage,
						Bucket: dash.BucketActiveRuns, When: now}, true, nil
				},
			})
			if err != nil {
				t.Fatalf("serve.New: %v", err)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/r1", nil))
			if rr.Code != 200 {
				t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			chips := []string{
				`action="/run/alpha/r1/advance"`,
				`action="/run/alpha/r1/ship"`,
				`action="/run/alpha/r1/chain"`,
			}
			for _, chip := range chips {
				got := strings.Contains(body, chip)
				if got != tc.wantChips {
					t.Errorf("%s run page: chip %q present=%v, want %v\n%s",
						tc.workflow, chip, got, tc.wantChips, body)
				}
			}
		})
	}
}

func seedServeRun(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"),
		[]byte(`{"id":"`+projectID+`","remote":"git@example.com:acme/`+projectID+`.git"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{
		ID: runID, Project: projectID, Status: run.StatusInProgress,
		Workflow: workflow, Created: "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// TestParkedRunRowResolvesToFirstStage pins the fact the park-by-default
// UI leans on: a run parked from the serve form (or from promote) — no
// agent has touched it, no stage document exists — resolves through the
// real GatherRunRow to its workflow's first stage. That Stage is what
// composeRunActions matches against ui.Stages to render the "→ design"
// chip, so if a parked run resolved to "" the destination page would
// offer no ride and "park now, decide later" would be a dead end.
//
// The chip-rendering half is covered above (and in internal/serve by
// TestPromoteParksThenRunPageRidesIt) against a stubbed row; this is the
// one assertion that runs the real gatherer.
func TestParkedRunRowResolvesToFirstStage(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	seedServeRun(t, root, "alpha", "seed-run", "sdlc")
	gittest.Commit(t, root, "seed project")

	// Park a run exactly the way serve's handleNewRunSubmit does.
	if _, err := runopen.Open(root, "alpha", run.Options{
		ID: "parked-one", Workflow: "sdlc", Agent: "claude",
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("runopen.Open: %v", err)
	}

	row, ok, err := GatherRunRow(root, "alpha", "parked-one", time.Now().UTC())
	if err != nil {
		t.Fatalf("GatherRunRow: %v", err)
	}
	if !ok {
		t.Fatal("parked run not found by the real dash lookup")
	}
	if row.Stage != "design" {
		t.Errorf("parked sdlc run Stage = %q, want %q — the destination page's ride chip keys off this",
			row.Stage, "design")
	}
}
