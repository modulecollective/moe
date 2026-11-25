package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func heartbeatAdvanceServer(t *testing.T) (string, http.Handler, *heartbeatGate) {
	t.Helper()
	root := parallelQuietFixture(t)
	seedRun(t, root, "moe", "advance-me", sdlcWorkflow, run.StatusInProgress, time.Now(), nil)
	md, err := run.Load(root, "moe", "advance-me")
	if err != nil {
		t.Fatal(err)
	}
	writeReadyDesign(t, root, md)
	gittest.Commit(t, root, "persist ready design")
	trailerstest.CommitWorkTurnAt(t, root, "moe", "advance-me", sdlcWorkflow, "design", time.Now().Add(-time.Hour))
	seedPulseRun(t, root, "moe", "pulse-before", run.StatusClosed, time.Hour)
	gittest.Commit(t, root, "persist pulse metadata")
	backdateHead(t, root, time.Hour)
	opts := serveOptions(root, io.Discard)
	opts.Dynamic = true
	srv, err := serve.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return root, srv.Handler(), opts.Heartbeat.(*heartbeatGate)
}

func postHeartbeatAdvance(t *testing.T, root string, h http.Handler) string {
	t.Helper()
	before := headSHA(t, root)
	tree := gittest.Output(t, root, "rev-parse", "HEAD^{tree}")
	rr := do(t, h, "POST", "/run/moe/advance-me/advance", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("advance: %d %s", rr.Code, rr.Body.String())
	}
	after := headSHA(t, root)
	if after == before || gittest.Output(t, root, "rev-parse", "HEAD^{tree}") != tree {
		t.Fatal("advance must change HEAD without changing its tree")
	}
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	if body != "advance: design\n\nMoE-Run: advance-me\nMoE-Project: moe\nMoE-Workflow: sdlc\nMoE-Document: design" {
		t.Fatalf("unexpected marker: %q", body)
	}
	md, err := run.Load(root, "moe", "advance-me")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := LookupWorkflow(sdlcWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	stage, kind, err := wf.Next(root, md)
	if err != nil || stage != "build" || kind != NextKindStage {
		t.Fatalf("next = %s, %v, %v", stage, kind, err)
	}
	return after
}

func TestHeartbeatHTTPAdvanceWakesSurveyedBoard(t *testing.T) {
	t.Parallel()
	root, h, g := heartbeatAdvanceServer(t)
	g.Swept("moe", true)
	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Fatalf("surveyed board due: %v", got)
	}
	mark := postHeartbeatAdvance(t, root, h)
	decisions := dueDecisions(t, g, testTick)
	if !heldFor(decisions, "moe") || len(sweepIDs(decisions)) != 0 {
		t.Fatalf("advance missed quiet window: %+v", decisions)
	}
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 {
		t.Fatalf("empty advance did not wake surveyed board: %v", got)
	}
	g.Dispatching()
	if g.dispatched["moe"] != mark {
		t.Fatalf("dispatched %q, want advance %s", g.dispatched["moe"], mark)
	}
	// A restart must recover the parked advance without an in-memory cursor.
	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 1 {
		t.Fatalf("restart lost advance: %v", got)
	}
	seedPulseRun(t, root, "moe", "pulse-after", run.StatusClosed, 0)
	g.Swept("moe", true)
	for range 3 {
		if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
			t.Fatalf("consumed advance reoffered: %v", got)
		}
	}
}

func TestHeartbeatHTTPAdvanceDuringSweep(t *testing.T) {
	t.Parallel()
	for _, clean := range []bool{true, false} {
		t.Run(map[bool]string{true: "clean", false: "failed"}[clean], func(t *testing.T) {
			root, h, g := heartbeatAdvanceServer(t)
			g.Swept("moe", true)
			base := g.tips["moe"]
			journalCommit(t, root, "moe", "machine: trigger survey", "MoE-Consent: dynamic")
			if got := dueProjectsPastTheWindow(t, g); len(got) != 1 {
				t.Fatalf("dispatch = %v", got)
			}
			g.Dispatching()
			postHeartbeatAdvance(t, root, h)
			journalCommit(t, root, "moe", "close: pulse-mid", "MoE-Workflow: pulse\nMoE-Consent: dynamic")
			g.Swept("moe", clean)
			if g.tips["moe"] != base || g.surveyed["moe"] != base {
				t.Fatal("sweep acknowledged unseen advance")
			}
			if got := dueProjectsPastTheWindow(t, g); len(got) != 1 {
				t.Fatalf("unseen advance not reoffered: %v", got)
			}
			g.Dispatching()
			journalCommit(t, root, "moe", "close: pulse-followup", "MoE-Workflow: pulse\nMoE-Consent: dynamic")
			g.Swept("moe", true)
			if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
				t.Fatalf("followup did not converge: %v", got)
			}
		})
	}
}

func TestHeartbeatHTTPAdvanceMaskedOperatorWindow(t *testing.T) {
	t.Parallel()
	root, h, g := heartbeatAdvanceServer(t)
	g.Swept("moe", true)
	postHeartbeatAdvance(t, root, h)
	journalCommit(t, root, "moe", "machine: masks advance", "MoE-Consent: dynamic")
	decisions := dueDecisions(t, g, testTick)
	if !heldFor(decisions, "moe") || len(sweepIDs(decisions)) != 0 {
		t.Fatalf("masked advance missed window: %+v", decisions)
	}
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 {
		t.Fatalf("masked window did not expire: %v", got)
	}
}

func TestHeartbeatProjectJournalScopeAndOrder(t *testing.T) {
	t.Parallel()
	for _, withPath := range []bool{true, false} {
		t.Run(map[bool]string{true: "path-and-marks", false: "marks-only"}[withPath], func(t *testing.T) {
			root := parallelBureaucracy(t)
			if _, _, _, ok := projectJournalTip(root, "moe"); ok {
				t.Fatal("unrelated root commit counted as project history")
			}
			when := time.Now().Add(-time.Hour).Truncate(time.Second)
			if withPath {
				writeFile(t, root+"/projects/moe/.journal-probe", "seed\n")
				journalCommit(t, root, "moe", "unmarked hand edit", "")
				backdateHead(t, root, time.Hour)
				whenText := gittest.Output(t, root, "log", "-1", "--format=%cI")
				var err error
				when, err = time.Parse(time.RFC3339, whenText)
				if err != nil {
					t.Fatal(err)
				}
				if tip, _, operator, ok := projectJournalTip(root, "moe"); !ok || !operator || tip != headSHA(t, root) {
					t.Fatalf("unmarked path edit lost: %s %v %v", tip, operator, ok)
				}
			}
			base := headSHA(t, root)
			trailerstest.CommitAdvanceAt(t, root, "moe", "advanced", sdlcWorkflow, "design", when)
			mark := headSHA(t, root)
			// Exact project matching must reject both prefixes and suffixes.
			for _, project := range []string{"moe-extra", "other-moe"} {
				trailerstest.CommitAdvanceAt(t, root, project, "foreign", sdlcWorkflow, "design", when)
			}
			if tip, at, operator, ok := projectJournalTip(root, "moe"); !ok || tip != mark || !at.Equal(when) || !operator {
				t.Fatalf("same-second selection = %s %s %v %v, want %s", tip, at, operator, ok, mark)
			}
			if operator, ride := journalActs(root, "moe", base+"..HEAD"); !operator || !ride {
				t.Fatalf("empty advance acts = %v %v", operator, ride)
			}
			if operator, ride := journalActs(root, "moe", mark+"..HEAD"); operator || ride {
				t.Fatalf("foreign marks counted: %v %v", operator, ride)
			}
			if operatorActedSince(root, "moe", when.Add(time.Second)) {
				t.Fatal("old advance held a later window")
			}
			trailerstest.CommitTrailer(t, root, "machine bookkeeping", "MoE-Project: moe\nMoE-Workflow: pulse\nMoE-Consent: dynamic", when)
			if tip, _, operator, ok := projectJournalTip(root, "moe"); !ok || tip != headSHA(t, root) || operator {
				t.Fatalf("newest machine mark lost: %s %v %v", tip, operator, ok)
			}
			if operator, ride := journalActs(root, "moe", "HEAD^..HEAD"); operator || ride {
				t.Fatalf("empty bookkeeping counted as unseen work: %v %v", operator, ride)
			}
		})
	}
}

func TestHeartbeatOtherProjectEmptyMarkStaysQuiet(t *testing.T) {
	t.Parallel()
	root := parallelQuietFixture(t)
	g := newHeartbeatGate(root)
	g.Swept("moe", true)
	trailerstest.CommitAdvanceAt(t, root, "moe-extra", "foreign", sdlcWorkflow, "design", time.Now())
	decisions := dueDecisions(t, g, testTick)
	if heldFor(decisions, "moe") || len(sweepIDs(decisions)) != 0 {
		t.Fatalf("foreign advance woke or held moe: %+v", decisions)
	}
}

func TestHeartbeatJournalReadFailureAbstains(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, _, _, ok := projectJournalTip(root, "moe"); ok {
		t.Fatal("failed tip read reported history")
	}
	if operator, ride := journalActs(root, "moe", "HEAD"); operator || ride {
		t.Fatalf("failed log reported acts: %v %v", operator, ride)
	}
}

func TestHeartbeatHTTPAdvanceWithSameSecondFutureQuotaRetry(t *testing.T) {
	root, h, _ := heartbeatAdvanceServer(t)
	defer withRideMode(rideDynamic)()
	writePulseGate(t, root, "moe", "pulse-before", `{"status":"ok"}`)
	gittest.Commit(t, root, "persist pulse canvas")
	q, err := newPulseQuotaRetries(root, "moe")
	if err != nil {
		t.Fatal(err)
	}
	q.pulse, q.survey = "pulse-before", false
	if err := q.record(quotaDeferral{
		target:  run.QuotaTarget{Project: "moe", Run: "advance-me", Stage: "build", Backend: "codex", Reason: "weekly 99%"},
		retryAt: time.Now().Add(24 * time.Hour),
	}, io.Discard); err != nil {
		t.Fatal(err)
	}
	when := gittest.Output(t, root, "log", "-1", "--format=%cI")
	postHeartbeatAdvance(t, root, h)
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + when, "GIT_AUTHOR_DATE=" + when}, "commit", "--amend", "--allow-empty", "--date="+when)
	if !heldFor(dueDecisions(t, newHeartbeatGate(root), testTick), "moe") {
		t.Fatal("quota retry bypassed advance quiet window")
	}
	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 1 {
		t.Fatalf("future retry swallowed same-second empty advance on restart: %v", got)
	}
}

func TestHeartbeatProjectJournalTipPinsAndBoundsReads(t *testing.T) {
	root := parallelQuietFixture(t)
	pathTip := headSHA(t, root)
	trailerstest.CommitAdvanceAt(t, root, "moe", "first", sdlcWorkflow, "design", time.Now())
	head := headSHA(t, root)
	var queries []string
	prev := git.Hook
	t.Cleanup(func() { git.Hook = prev })
	git.Hook = func(_ string, args []string, _ time.Duration, _ error) {
		if args[0] != "log" {
			return
		}
		queries = append(queries, strings.Join(args, " "))
		if len(queries) == 1 {
			// Move the real journal after the path read. The trailer read
			// must retain the original observation's upper bound.
			trailerstest.CommitAdvanceAt(t, root, "moe", "later", sdlcWorkflow, "design", time.Now())
		}
	}
	if tip, _, _, ok := projectJournalTip(root, "moe"); !ok || tip != head {
		t.Fatalf("tip escaped observed HEAD: %s, want %s", tip, head)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "log "+head+" -1 ") ||
		!strings.Contains(queries[1], "log "+pathTip+".."+head+" -1 ") ||
		!strings.Contains(queries[1], "--grep ^MoE-Project: moe$ --") {
		t.Fatalf("unbounded or unpinned tip queries: %q", queries)
	}
	git.Hook = prev
	if tip, _, _, ok := projectJournalTip(root, "moe"); !ok || tip != headSHA(t, root) {
		t.Fatal("next observation failed to see the later mark")
	}
}
