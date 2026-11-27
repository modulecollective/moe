package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/serve"
)

func rowFor(t *testing.T, snap DashSnapshot, key string) dash.Row {
	t.Helper()
	for _, r := range snap.Rows {
		if r.Project+"/"+r.Run == key && r.Bucket == dash.BucketActiveRuns {
			return r
		}
	}
	t.Fatalf("no active row for %s", key)
	return dash.Row{}
}

// Rows carry the overlay from one observation pass: a hold reads as
// held, a live owner as its phase, and an owner with no endpoint as
// exactly that — never as idle.
func TestDashRowsCarryControlOverlay(t *testing.T) {
	root, md := controlFixture(t)
	submitControl(t, root, runcontrol.KindHandoff, "h1")
	snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowFor(t, snap, "moe/change-auth").Control; got != "handed off" {
		t.Fatalf("held row = %q", got)
	}
	var out bytes.Buffer
	t.Setenv("NO_COLOR", "1")
	dash.RenderIndexedRows(&out, time.Now(), snap.Rows, 1, 1)
	if !strings.Contains(out.String(), "[handed off]") {
		t.Fatalf("terminal dash lacks the overlay:\n%s", out.String())
	}

	// Resume, then own it the way a running stage does.
	h, _ := runowner.Acquire(root, "moe", "change-auth")
	if _, _, err := runcontrol.Resume(root, "moe", "change-auth", h, runcontrol.Request{}); err != nil {
		t.Fatal(err)
	}
	h.Release()
	ownForTest(t, root)
	publishPhase(root, md, "agent running", "code", "")
	snap, _ = GatherDashSnapshot(root, time.Now(), DashFilter{})
	if got := rowFor(t, snap, "moe/change-auth").Control; !strings.HasPrefix(got, "agent running code") {
		t.Fatalf("owned row = %q", got)
	}
}

func TestUnreachableOwnerNeverReadsIdle(t *testing.T) {
	root, _ := controlFixture(t)
	h, err := runowner.Acquire(root, "moe", "change-auth") // owner without an endpoint
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	snap, _ := GatherDashSnapshot(root, time.Now(), DashFilter{})
	if got := rowFor(t, snap, "moe/change-auth").Control; got != "owned · no live status" {
		t.Fatalf("row = %q", got)
	}
}

// The web's doors: a held run offers only resume, a stale resume is
// refused and keeps the hold, a current one releases it and leaves
// nothing owning the run; finish on a held run is refused.
func TestServeControlDoors(t *testing.T) {
	root, _ := controlFixture(t)
	submitControl(t, root, runcontrol.KindPause, "p1")
	v, ok, err := serveRunControl(root, "moe", "change-auth")
	if err != nil || !ok || len(v.Actions) != 1 || v.Actions[0] != "resume" || v.Revision != 1 {
		t.Fatalf("view = %+v, %v, %v", v, ok, err)
	}
	if _, err := serveSubmitControl(root, "moe", "change-auth", "finish", "w1", 0); !errors.Is(err, serve.ErrControlRefused) {
		t.Fatalf("finish while held: %v", err)
	}
	if _, err := serveSubmitControl(root, "moe", "change-auth", "resume", "w2", 99); !errors.Is(err, serve.ErrControlRefused) {
		t.Fatalf("stale resume: %v", err)
	}
	if !controlRecord(t, root).Held() {
		t.Fatal("a stale resume released the hold")
	}
	if _, err := serveSubmitControl(root, "moe", "change-auth", "resume", "w3", 1); err != nil {
		t.Fatal(err)
	}
	if controlRecord(t, root).Held() {
		t.Fatal("resume left the hold")
	}
	if busy, _ := runowner.Busy(root, "moe", "change-auth"); busy {
		t.Fatal("web resume left the run owned")
	}
	v, _, _ = serveRunControl(root, "moe", "change-auth")
	if strings.Join(v.Actions, ",") != "pause,handoff,finish" {
		t.Fatalf("idle actions = %v", v.Actions)
	}
}

func TestServeContinuationEligibility(t *testing.T) {
	for _, workflow := range []string{"idea", "intent", "pulse", "sdlc"} {
		t.Run(workflow, func(t *testing.T) {
			root, md := controlFixture(t)
			stage := workflow
			if workflow == "sdlc" {
				stage = "design"
			}
			md.Workflow = workflow
			md.LastTurn = &run.Turn{Stage: stage, Agent: "codex"}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "add", "-A")
			gittest.Commit(t, root, "record conversation")
			srv, err := serve.New(serveOptions(root, io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			before := gittest.HeadSHA(t, root)
			for _, live := range []bool{false, true} {
				if live {
					ownForTest(t, root)
					publishRun(root, md.Project, md.ID, func(s *runsock.Status) {
						s.Phase, s.Stage, s.Agent = runsock.PhaseRunning, stage, "codex"
					})
				}
				v, ok, err := serveRunControl(root, md.Project, md.ID)
				if err != nil || !ok || len(v.Lines) == 0 {
					t.Fatalf("live=%v: status diagnostics missing: %+v, %v, %v", live, v, ok, err)
				}
				want := ""
				if workflow == "sdlc" {
					want = "moe sdlc design moe/change-auth --agent codex"
					wantActions := "pause,handoff,finish"
					if live {
						wantActions = "pause,handoff,park,finish"
					}
					if strings.Join(v.Actions, ",") != wantActions {
						t.Fatalf("live=%v: controls = %v", live, v.Actions)
					}
				} else if len(v.Actions) != 0 {
					t.Fatalf("live=%v: unsupported controls = %v", live, v.Actions)
				}
				if v.Conversation != want {
					t.Errorf("live=%v: conversation = %q, want %q", live, v.Conversation, want)
				}
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/moe/change-auth/status", nil))
				body := rr.Body.String()
				if rr.Code != 200 || !strings.Contains(body, `id="run-status"`) || !strings.Contains(body, "owner:") {
					t.Fatalf("live=%v: status = %d %s", live, rr.Code, body)
				}
				if got := strings.Contains(body, "continue the conversation at a terminal:"); got != (want != "") || (want != "" && !strings.Contains(body, want)) {
					t.Errorf("live=%v: continuation guidance = %s", live, body)
				}
			}
			if gittest.HeadSHA(t, root) != before || strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain")) != "" {
				t.Fatal("status reads mutated the journal")
			}
		})
	}
}

// The dash's Rex status names runs that wait on the operator.
func TestRexStatusNamesRunsWaitingForOperator(t *testing.T) {
	root, _ := controlFixture(t)
	submitControl(t, root, runcontrol.KindHandoff, "h1")
	snap, _ := GatherDashSnapshot(root, time.Now(), DashFilter{})
	var found bool
	for _, r := range dashStatusReports(snap, nil, serveState{}, DashFilter{}) {
		if r.ID == "moe/dash/control" {
			found = strings.Contains(r.Message, "moe/change-auth (handed off)") && r.State == "blocked"
		}
	}
	if !found {
		t.Fatal("no control report for a handed-off run")
	}
}
