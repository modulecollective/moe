package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func checkProgress(t *testing.T, root string, wf *Workflow, md *run.Metadata, stage string, blocked bool) {
	t.Helper()
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	var previous WorkflowProgress
	for i, index := range []*run.JournalIndex{nil, idx} {
		p, err := wf.ProgressWithIndex(root, md, index)
		if err != nil || p.Stage != stage || (len(p.GateProblems) > 0) != blocked {
			t.Fatalf("progress = %+v, %v; want %s blocked=%v", p, err, stage, blocked)
		}
		next, kind, err := wf.NextWithIndex(root, md, index)
		if err != nil || next != p.Stage || kind != p.Kind {
			t.Fatalf("Next differs: %s %v %v / %+v", next, kind, err, p)
		}
		if i > 0 && !reflect.DeepEqual(p, previous) {
			t.Fatalf("indexed progress differs: %+v / %+v", p, previous)
		}
		previous = p
	}
}

func TestWorkflowProgressAuthoredGate(t *testing.T) {
	root := newTestBureaucracy(t)
	wf := NewWorkflow("progress-test")
	wf.RegisterStage("plan")
	wf.RegisterStage("make", "plan")
	ready := false
	calls := 0
	wf.RegisterStageGate("plan", func(string, *run.Metadata) (bool, error) { calls++; return ready, nil })
	RegisterWorkflow(wf)
	t.Cleanup(func() { delete(workflows, wf.Name) })
	md := trailerstest.SeedRun(t, root, "p", "r", wf.Name, run.StatusInProgress)
	checkProgress(t, root, wf, md, "plan", false) // absent or imported gates cannot count without work
	trailerstest.CommitTrailer(t, root, "seed imported canvas", "MoE-Project: p\nMoE-Run: r\nMoE-Document: plan", time.Time{})
	trailerstest.CommitTrailer(t, root, "work: start session plan", "MoE-Project: p\nMoE-Run: r\nMoE-Document: plan", time.Time{})
	checkProgress(t, root, wf, md, "plan", false)
	if calls != 0 {
		t.Fatal("gate read before authored turn")
	}
	at := time.Now().Add(-time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "p", "r", wf.Name, "plan", at)
	checkProgress(t, root, wf, md, "plan", true)
	calls = 0
	if _, err := wf.ProgressWithIndex(root, md, nil); err != nil || calls != 1 {
		t.Fatalf("gate reads = %d, %v", calls, err)
	}
	other := *md
	other.Project = "q"
	checkProgress(t, root, wf, &other, "plan", false)
	ready = true
	checkProgress(t, root, wf, md, "plan", false) // ready but parked
	trailerstest.CommitAdvanceAt(t, root, "p", "r", wf.Name, "plan", at.Add(time.Minute))
	checkProgress(t, root, wf, md, "make", false) // no gate on make
	trailerstest.CommitWorkTurnAt(t, root, "p", "r", wf.Name, "make", at.Add(2*time.Minute))
	checkProgress(t, root, wf, md, "", false)
	ready = false
	trailerstest.CommitWorkTurnAt(t, root, "p", "r", wf.Name, "plan", at.Add(3*time.Minute))
	checkProgress(t, root, wf, md, "plan", true)
	for _, status := range []string{run.StatusPushed, run.StatusClosed, run.StatusMerged, run.StatusPromoted} {
		md.Status = status
		checkProgress(t, root, wf, md, "", false)
	}
}

func TestWorkflowProgressRealGates(t *testing.T) {
	for _, stage := range []string{"design", "build", "review"} {
		t.Run(stage, func(t *testing.T) {
			root := newTestBureaucracy(t)
			md := trailerstest.SeedRun(t, root, "p", "r", "sdlc", run.StatusInProgress)
			wf, _ := LookupWorkflow("sdlc")
			at := time.Now().Add(-time.Hour)
			for i, s := range wf.Stages() {
				if s == stage {
					break
				}
				if s == "design" {
					writeReadyDesign(t, root, md)
				}
				if s == "build" {
					for _, name := range []string{"code.md", "test.md"} {
						writeFile(t, filepath.Join(root, run.ContentFilePath("p", "r", s, name)), stampedA)
					}
				}
				trailerstest.CommitWorkTurnAt(t, root, "p", "r", "sdlc", s, at.Add(time.Duration(i)*time.Minute))
				trailerstest.CommitAdvanceAt(t, root, "p", "r", "sdlc", s, at.Add(time.Duration(i)*time.Minute))
			}
			if stage == "build" {
				writeFile(t, filepath.Join(root, run.ContentFilePath("p", "r", stage, "code.md")), stampedA)
			} else {
				writeFile(t, filepath.Join(root, run.ContentPath("p", "r", stage)), skeletonA)
			}
			gittest.Run(t, root, "add", "-A")
			gittest.Commit(t, root, "seed canvases")
			checkProgress(t, root, wf, md, stage, false)
			trailerstest.CommitWorkTurnAt(t, root, "p", "r", "sdlc", stage, at.Add(10*time.Minute))
			checkProgress(t, root, wf, md, stage, true)
			snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
			if err != nil || !rowFor(t, snap, "p/r").StageBlocked {
				t.Fatalf("real %s gate not carried through gather: %v", stage, err)
			}
			if stage == "build" {
				p, _ := wf.ProgressWithIndex(root, md, nil)
				if !strings.Contains(strings.Join(p.GateProblems, "\n"), "test.md: missing") {
					t.Fatalf("missing build filename: %+v", p)
				}
			}
		})
	}
}

func TestStageBlockedPresentation(t *testing.T) {
	base := RunObservation{Status: run.StatusInProgress, Progress: WorkflowProgress{Stage: "design", GateProblems: []string{"gate not satisfied"}}}
	if !base.StageBlocked() {
		t.Fatal("parked gate hidden")
	}
	for _, phase := range []string{runsock.PhaseAdmission, runsock.PhaseQuotaWait, runsock.PhaseRunning, runsock.PhaseSettling, runsock.PhaseBetween, runsock.PhaseShipping, runsock.PhaseDeciding, "unknown"} {
		o := base
		o.Owned = true
		o.Live = &runsock.Status{Phase: phase}
		want := phase == runsock.PhaseDeciding || phase == "unknown"
		if o.StageBlocked() != want {
			t.Fatalf("phase %s blocked=%v", phase, o.StageBlocked())
		}
		o.LiveErr = errors.New("unreachable")
		if !o.StageBlocked() {
			t.Fatalf("lost contact %s erased durable result", phase)
		}
		o.LiveErr = nil
		o.OwnerErr = errors.New("unknown owner")
		if !o.StageBlocked() {
			t.Fatal("unknown owner erased durable result")
		}
	}
	base.Progress.GateProblems = nil
	if base.StageBlocked() {
		t.Fatal("ready gate is blocked")
	}
}

func blockedBoardFixture(t *testing.T) (string, *run.Metadata) {
	t.Helper()
	root, md := controlFixture(t)
	fresh := seedParkedRun(t, root, "fresh")
	for _, m := range []*run.Metadata{md, fresh} {
		writeFile(t, filepath.Join(root, run.ContentPath(m.Project, m.ID, "design")), skeletonA)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed blocked templates")
	trailerstest.CommitWorkTurnAt(t, root, md.Project, md.ID, md.Workflow, "design", time.Now())
	t.Setenv("MOE_HOME", root)
	return root, md
}

func TestStageBlockedBoardAndWeb(t *testing.T) {
	root, md := blockedBoardFixture(t)
	// Existing input hints and controls coexist; reads must mint nothing.
	addOn(t, root, "moe", md.ID, "Please investigate")
	before := gittest.HeadSHA(t, root)
	snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !rowFor(t, snap, "moe/change-auth").StageBlocked || rowFor(t, snap, "moe/fresh").StageBlocked {
		t.Fatalf("bad rows: %+v", snap.Rows)
	}
	var out, errb bytes.Buffer
	t.Setenv("NO_COLOR", "1")
	if code := runRunList(nil, &out, &errb); code != 0 {
		t.Fatalf("run list %d: %s", code, &errb)
	}
	if !strings.Contains(out.String(), "sdlc:design") || strings.Count(out.String(), "[blocked]") != 1 || !strings.Contains(out.String(), "· input") {
		t.Fatalf("run list: %s", &out)
	}
	out.Reset()
	if code := runControlShow([]string{"moe/change-auth"}, &out, &errb); code != 0 {
		t.Fatalf("control show %d: %s", code, &errb)
	}
	if !strings.Contains(out.String(), "last authored stage result: design blocked") || !strings.Contains(out.String(), "moe sdlc cat moe/change-auth design") {
		t.Fatalf("control: %s", &out)
	}
	srv, err := serve.New(serveOptions(root, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	for _, path := range []string{"/", "/projects/moe"} {
		body := get(path)
		if !strings.Contains(body, `data-blocked-key="moe/change-auth">blocked</span>`) || !strings.Contains(body, `data-blocked-key="moe/fresh" hidden>blocked</span>`) {
			t.Fatalf("%s: badges missing", path)
		}
	}
	body := get("/run/moe/change-auth")
	for _, want := range []string{`class="badge blocked">blocked</span>`, "last authored stage result: design blocked", "gate not satisfied", "moe sdlc cat moe/change-auth design", `value="pause"`, `value="finish"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %s", want)
		}
	}
	refresh := func() map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(get("/run-status")), &m); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"__serve_cluster", "__agent_summary", "__agent_error", "__agent_revision"} {
			if _, ok := m[key]; !ok {
				t.Errorf("refresh missing %s", key)
			}
		}
		return m
	}
	status := func() serve.RunRowStatus {
		t.Helper()
		var v serve.RunRowStatus
		if err := json.Unmarshal(refresh()["moe/change-auth"], &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !status().StageBlocked {
		t.Fatal("refresh failed to add badge")
	}
	actions, _, _ := serveRunControl(root, "moe", md.ID)
	if strings.Join(actions.Actions, ",") != "pause,handoff,finish" {
		t.Fatalf("blocked changed actions: %v", actions.Actions)
	}
	if gittest.HeadSHA(t, root) != before || strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain")) != "" {
		t.Fatal("rendering mutated journal")
	}
	// A confirmed retry suppresses without a commit; unreachable owner preserves.
	h, err := runowner.Acquire(root, "moe", md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !status().StageBlocked {
		t.Fatal("unreachable owner erased blocker")
	}
	h.Release()
	ownForTest(t, root)
	publishPhase(root, md, runsock.PhaseRunning, "design", "")
	if status().StageBlocked {
		t.Fatal("running retry did not suppress badge")
	}
	publishPhase(root, md, runsock.PhaseDeciding, "design", "next stage")
	if !status().StageBlocked {
		t.Fatal("decision did not restore badge")
	}
	// Settlement replaces the result on the next read.
	writeReadyDesign(t, root, md)
	gittest.Run(t, root, "add", "-A")
	trailerstest.CommitWorkTurnAt(t, root, "moe", md.ID, "sdlc", "design", time.Now().Add(time.Second))
	if status().StageBlocked {
		t.Fatal("ready settlement did not clear")
	}
	if strings.Contains(get("/run/moe/change-auth/status"), "last authored stage result:") {
		t.Fatal("stale detail after ready")
	}
}

func TestStageBlockedReadError(t *testing.T) {
	root, md := blockedBoardFixture(t)
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "design"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := GatherDashSnapshot(root, time.Now(), DashFilter{}); err == nil {
		t.Fatal("gate read error swallowed by gather")
	}
	if _, err := serveRunStatuses(root, ""); err == nil {
		t.Fatal("gate read error swallowed by refresh")
	}
	if _, _, err := serveRunControl(root, md.Project, md.ID); err == nil {
		t.Fatal("gate read error swallowed by detail")
	}
}

func TestStageBlockedIgnoresSessionCanvas(t *testing.T) {
	root, md := blockedBoardFixture(t)
	tree := filepath.Join(root, ".moe", "worktrees", "residual")
	gittest.Run(t, root, "worktree", "add", "-b", "session/moe/change-auth/design", tree)
	// A retained session's in-progress edits are not the authored result.
	writeReadyDesign(t, tree, md)
	snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
	if err != nil {
		t.Fatal(err)
	}
	row := rowFor(t, snap, "moe/change-auth")
	if !row.StageBlocked || row.RunningDoc != "design" {
		t.Fatalf("residual session erased blocker: %+v", row)
	}
}

func TestStageBlockedMalformedGate(t *testing.T) {
	root, md := blockedBoardFixture(t)
	for _, body := range []string{"# Authored without a gate", "# Authored\n\n## Gate\n\n```json\n{broken}\n```"} {
		writeFile(t, filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")), body)
		gittest.Run(t, root, "add", "-A")
		trailerstest.CommitWorkTurnAt(t, root, md.Project, md.ID, md.Workflow, "design", time.Now())
		snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
		if err != nil || !rowFor(t, snap, "moe/change-auth").StageBlocked {
			t.Fatalf("malformed gate: %v", err)
		}
	}
}
