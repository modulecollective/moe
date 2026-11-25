package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// atTime runs fn with git's author and committer clocks pinned, so a fixture
// can order a design turn against a prerequisite's settlement at second
// resolution without sleeping.
func atTime(t *testing.T, when time.Time, fn func()) {
	t.Helper()
	stamp := when.Format(time.RFC3339)
	for _, k := range []string{"GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE"} {
		if err := os.Setenv(k, stamp); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		os.Unsetenv("GIT_AUTHOR_DATE")
		os.Unsetenv("GIT_COMMITTER_DATE")
	}()
	fn()
}

// blockedDesignGate is a design canvas whose gate is authored blocked;
// gateText is the gate section's prose.
func blockedDesignGate(gateText string) string {
	return "# Design\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\n" + gateText + "\n\n## Problem\n\nSomething.\n"
}

// operatorSourceBlocked opens an operator-typed sdlc run (no SpawnedBy) in
// beta and lands one blocked design turn with the given gate prose, plus a
// followups.md recording checked as its filings.
func operatorSourceBlocked(t *testing.T, root, id, gateText, checked string, when time.Time) {
	t.Helper()
	if _, err := runopen.Open(root, "beta", run.Options{
		ID: id, Workflow: "sdlc", SeedDocs: map[string]string{"design": "# Seed\n"},
	}); err != nil {
		t.Fatal(err)
	}
	commitDesignTurn(t, root, "beta", id, blockedDesignGate(gateText), checked, when)
}

// commitDesignTurn commits a design work turn on beta/id: the canvas and, when
// non-empty, followups.md.
func commitDesignTurn(t *testing.T, root, projectID, id, canvas, followups string, when time.Time) {
	t.Helper()
	paths := []string{filepath.Join(root, run.ContentPath(projectID, id, "design"))}
	if err := os.WriteFile(paths[0], []byte(canvas), 0o644); err != nil {
		t.Fatal(err)
	}
	if followups != "" {
		p := filepath.Join(root, run.FollowupsPath(projectID, id))
		if err := os.WriteFile(p, []byte(followups), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	msg := "work: update design\n\n" + trailers.Block{
		Run: id, Project: projectID, Workflow: "sdlc", Document: "design",
	}.String()
	atTime(t, when, func() {
		if err := run.StageAndCommit(root, msg, paths...); err != nil {
			t.Fatal(err)
		}
	})
}

// settleRun flips projectID/id to status in a commit naming the run.
func settleRun(t *testing.T, root, projectID, id, status string, when time.Time) {
	t.Helper()
	md, err := run.Load(root, projectID, id)
	if err != nil {
		t.Fatal(err)
	}
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	msg := "settle " + id + "\n\n" + trailers.Block{Run: id, Project: projectID, Workflow: md.Workflow}.String()
	atTime(t, when, func() {
		if err := run.StageAndCommit(root, msg, filepath.Join(run.Dir(projectID, id), "run.json")); err != nil {
			t.Fatal(err)
		}
	})
}

// promoteIdea promotes projectID/idea into an sdlc run and returns its id.
func promoteIdea(t *testing.T, root, projectID, idea string, when time.Time) string {
	t.Helper()
	t.Setenv("MOE_HOME", root)
	var out bytes.Buffer
	atTime(t, when, func() {
		if code := runNew("sdlc", []string{"--from-idea", projectID + "/" + idea, "--park"}, &out, &out); code != 0 {
			t.Fatalf("promote %s: %d %s", idea, code, &out)
		}
	})
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan failed")
	}
	to := sc.idx.PromotedTo[projectID+"/"+idea]
	if to == "" {
		t.Fatalf("no promotion recorded for %s: %s", idea, &out)
	}
	_, id, _ := strings.Cut(to, "/")
	return id
}

// seedOwner opens an in-progress run without touching its project.json.
func seedOwner(t *testing.T, root, projectID, id, workflow string) {
	t.Helper()
	doc := "design"
	if workflow == "idea" {
		doc = "idea"
	}
	if _, err := runopen.Open(root, projectID, run.Options{
		ID: id, Workflow: workflow, SeedDocs: map[string]string{doc: "# " + id + "\n"},
	}); err != nil {
		t.Fatal(err)
	}
}

func scanOf(t *testing.T, root string) *pulseScan {
	t.Helper()
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan failed")
	}
	return sc
}

// TestPrerequisiteResumeRidesTheBlockedDesign is the incident end to end:
// an operator-opened design parks blocked on a tagged filing its gate
// names; while the prerequisite is open the pick is held with today's
// reason, and once it merges the same pick rides from design on.
func TestPrerequisiteResumeRidesTheBlockedDesign(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	t0 := time.Now().Add(-3 * time.Hour)
	tagIdea(t, root, "beta", "update-metric")
	operatorSourceBlocked(t, root, "study",
		"Blocked on `update-metric`: it belongs on default whether or not this study lands. Verify the metric exists.",
		"- [x] `update-metric` (sdlc) — Update the metric\n", t0)
	prereq := promoteIdea(t, root, "beta", "update-metric", t0.Add(time.Hour))

	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/study","why":"its prerequisite"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(stderr, "its turn closed but not advanced — the operator holds the trigger") {
		t.Fatalf("before the merge: exit=%d stages=%v stderr=%s", code, kickStages(*stages), stderr)
	}
	if got := filingExitLines(root, mustLoad(t, root, "beta", "study"), "design"); !strings.Contains(got, "filed: beta/update-metric (sdlc run, in_progress") ||
		!strings.Contains(got, "this run resumes when it settles") {
		t.Fatalf("exit lines before the merge:\n%s", got)
	}

	if got := parkedKickableThread(root, scanOf(t, root), "beta", false); got == "beta/study" {
		t.Fatal("heartbeat woke for a source whose prerequisite is still open")
	}

	settleRun(t, root, "beta", prereq, run.StatusMerged, t0.Add(2*time.Hour))
	sc := scanOf(t, root)
	if got := parkedKickableThread(root, sc, "beta", false); got != "beta/study" {
		t.Fatalf("heartbeat named %q for a due source", got)
	}
	if block := parkedRunsBlock(sc, "beta"); !strings.Contains(block, "`beta/study` (sdlc) — due: its gate names `beta/"+prereq+"`, merged since its last turn") {
		t.Fatalf("parked line:\n%s", block)
	}
	if got := prerequisiteResumeKickoff(root, mustLoad(t, root, "beta", "study"), "design"); !strings.Contains(got, "`beta/"+prereq+"`") || !strings.Contains(got, "merged since your last turn") {
		t.Fatalf("kickoff line = %q", got)
	}
	if oneStageRide(root, "beta/study", sc) {
		t.Fatal("a settled prerequisite bounded the ride to one stage")
	}
	code, stderr = sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/study","why":"its prerequisite merged"}}`)
	got := kickStages(*stages)
	if code != 0 || len(got) < 2 || got[0] != "study:design" || got[1] != "study:build" {
		t.Fatalf("after the merge: exit=%d stages=%v stderr=%s", code, got, stderr)
	}
	if !strings.Contains(stderr, "pulse: riding beta/study (dynamic)") {
		t.Fatalf("ride label: %s", stderr)
	}
}

// TestPrerequisiteResumeNeedsTheGateToNameIt: only a backticked token in the
// gate section admits. The slug elsewhere on the canvas, or bare, does not,
// and the exit says nothing will resume the run. A token hard-wrapped onto
// the next line of the gate section still matches.
func TestPrerequisiteResumeNeedsTheGateToNameIt(t *testing.T) {
	for _, tc := range []struct {
		name, gate string
		admit      bool
	}{
		{"named", "Waiting on `fix-metric`.", true},
		{"wrapped", "Waiting on the measurement work filed as\n`beta/fix-metric`, which must land first.", true},
		{"bare", "Waiting on fix-metric.", false},
		{"absent", "Waiting on the metric.", false},
		{"elsewhere", "Waiting on the metric.\n\n## Scope\n\nFiled `fix-metric`.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := nextFixture(t)
			t0 := time.Now().Add(-3 * time.Hour)
			seedOwner(t, root, "beta", "fix-metric", "sdlc")
			operatorSourceBlocked(t, root, "study", tc.gate, "- [x] `fix-metric` — Fix the metric\n", t0)
			md := mustLoad(t, root, "beta", "study")
			exit := filingExitLines(root, md, "design")
			resumes := strings.Contains(exit, "this run resumes when it settles")
			nothing := strings.Contains(exit, "nothing will resume this run on its own")
			if resumes != tc.admit || nothing == tc.admit {
				t.Fatalf("exit lines (admit=%v):\n%s", tc.admit, exit)
			}
			settleRun(t, root, "beta", "fix-metric", run.StatusMerged, t0.Add(time.Hour))
			hold := kickFloorHold(root, "beta/study", scanOf(t, root))
			if (hold == "") != tc.admit {
				t.Fatalf("hold = %q, want admit=%v", hold, tc.admit)
			}
		})
	}
}

// TestPrerequisiteSettledMeansLanded: pushed is not settled; merged,
// closed, an idea closed unpromoted and a cross-project owner are.
func TestPrerequisiteSettledMeansLanded(t *testing.T) {
	for _, tc := range []struct {
		name, owner, workflow, status string
		admit                         bool
	}{
		{"pushed", "beta/fix", "sdlc", run.StatusPushed, false},
		{"merged", "beta/fix", "sdlc", run.StatusMerged, true},
		{"closed", "beta/fix", "sdlc", run.StatusClosed, true},
		{"idea closed unpromoted", "beta/fix", "idea", run.StatusClosed, true},
		{"idea still open", "beta/fix", "idea", run.StatusInProgress, false},
		{"cross-project", "moe/fix", "sdlc", run.StatusMerged, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := nextFixture(t)
			t0 := time.Now().Add(-3 * time.Hour)
			proj, id, _ := strings.Cut(tc.owner, "/")
			seedOwner(t, root, proj, id, tc.workflow)
			line := "- [x] `fix` — Fix\n"
			if proj != "beta" {
				line = "- [x] `" + tc.owner + "` — Fix\n"
			}
			operatorSourceBlocked(t, root, "study", "Waiting on `"+tc.owner+"`.", line, t0)
			if tc.status != run.StatusInProgress {
				settleRun(t, root, proj, id, tc.status, t0.Add(time.Hour))
			}
			hold := kickFloorHold(root, "beta/study", scanOf(t, root))
			if (hold == "") != tc.admit {
				t.Fatalf("hold = %q, want admit=%v", hold, tc.admit)
			}
		})
	}
}

// TestPrerequisiteResumeIsSelfLimiting: the re-check's turn moves the
// work-turn time past the settlement, so a gate that stays blocked is held
// again, the heartbeat no longer names it, and no second ride follows.
func TestPrerequisiteResumeIsSelfLimiting(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	t0 := time.Now().Add(-3 * time.Hour)
	seedOwner(t, root, "beta", "fix", "sdlc")
	operatorSourceBlocked(t, root, "study", "Waiting on `fix`.", "- [x] `fix` — Fix\n", t0)
	settleRun(t, root, "beta", "fix", run.StatusMerged, t0.Add(time.Hour))
	if got := parkedKickableThread(root, scanOf(t, root), "beta", false); got != "beta/study" {
		t.Fatalf("heartbeat before the re-check named %q", got)
	}
	// The re-check ran and left the gate blocked.
	commitDesignTurn(t, root, "beta", "study", blockedDesignGate("Still waiting on `fix`: the evidence is not there."), "", t0.Add(2*time.Hour))
	sc := scanOf(t, root)
	if hold := kickFloorHold(root, "beta/study", sc); !strings.Contains(hold, "the operator holds the trigger") {
		t.Fatalf("hold after the re-check = %q", hold)
	}
	if got := parkedKickableThread(root, sc, "beta", false); got != "" {
		t.Fatalf("heartbeat after the re-check named %q", got)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/study","why":"again"}}`)
	if code != 0 || len(*stages) != 0 {
		t.Fatalf("second ride: exit=%d stages=%v stderr=%s", code, kickStages(*stages), stderr)
	}
}

// TestOperatorNoteBuysOneStageOnAnOperatorRun: an undelivered note admits
// one stage on any first-stage run, seed-only or worked; delivered, the run
// is held again.
func TestOperatorNoteBuysOneStageOnAnOperatorRun(t *testing.T) {
	for _, worked := range []bool{false, true} {
		name := "seed-only"
		if worked {
			name = "turn closed"
		}
		t.Run(name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, stages := nextFixture(t)
			if _, err := runopen.Open(root, "beta", run.Options{
				ID: "sketch", Workflow: "sdlc", SeedDocs: map[string]string{"design": "# Seed\n"},
			}); err != nil {
				t.Fatal(err)
			}
			if worked {
				commitDesignTurn(t, root, "beta", "sketch", blockedDesignGate("Undecided."), "", time.Now().Add(-time.Hour))
			}
			if _, err := input.Add(root, "beta", "sketch", "Take another pass."); err != nil {
				t.Fatal(err)
			}
			sc := scanOf(t, root)
			if hold := kickFloorHold(root, "beta/sketch", sc); hold != "" {
				t.Fatalf("noted run held: %s", hold)
			}
			if !oneStageRide(root, "beta/sketch", sc) {
				t.Fatal("a note did not bound the ride to one stage")
			}
			if got := parkedKickableThread(root, sc, "beta", false); got != "beta/sketch" {
				t.Fatalf("heartbeat named %q", got)
			}
			code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/sketch","why":"the operator pushed it"}}`)
			if got := kickStages(*stages); code != 0 || len(got) != 1 || got[0] != "sketch:design" {
				t.Fatalf("exit=%d stages=%v stderr=%s", code, got, stderr)
			}
			if !strings.Contains(stderr, "(dynamic, one stage)") {
				t.Fatalf("ride label: %s", stderr)
			}
			f, err := input.Load(root, "beta", "sketch")
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range f.Pending() {
				if err := input.MarkDelivered(root, "beta", "sketch", "design", []int{p.ID}, ""); err != nil {
					t.Fatal(err)
				}
			}
			if hold := kickFloorHold(root, "beta/sketch", scanOf(t, root)); !strings.Contains(hold, "the operator holds the trigger") {
				t.Fatalf("delivered note still admits: %q", hold)
			}
		})
	}
}

// TestDueRunStillHeldByTheOtherLegs: a due run is admitted past the
// first-stage leg only; paused and safe mode and a reaped turn still hold it.
func TestDueRunStillHeldByTheOtherLegs(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(t *testing.T, root string)
	}{
		{"paused", "paused mode", func(t *testing.T, root string) {
			if err := project.SetMode(root, "beta", project.ModePaused); err != nil {
				t.Fatal(err)
			}
		}},
		{"safe", "safe mode", func(t *testing.T, root string) {
			if err := project.SetMode(root, "beta", project.ModeSafe); err != nil {
				t.Fatal(err)
			}
		}},
		{"reaped", "was reaped", func(t *testing.T, root string) {
			md := mustLoad(t, root, "beta", "study")
			md.Reaped = &run.ReapNote{Doc: "design", At: "2099-01-01T00:00:00Z", Tip: "abc"}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			msg := "reap\n\n" + trailers.Block{Run: "study", Project: "beta", Workflow: "sdlc", Consent: "dynamic"}.String()
			if err := run.StageAndCommit(root, msg, filepath.Join(run.Dir("beta", "study"), "run.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withClockInvoked()()
			root, _ := nextFixture(t)
			t0 := time.Now().Add(-3 * time.Hour)
			seedOwner(t, root, "beta", "fix", "sdlc")
			operatorSourceBlocked(t, root, "study", "Waiting on `fix`.", "- [x] `fix` — Fix\n", t0)
			settleRun(t, root, "beta", "fix", run.StatusMerged, t0.Add(time.Hour))
			tc.setup(t, root)
			hold := kickFloorHold(root, "beta/study", scanOf(t, root))
			if !strings.Contains(hold, tc.want) {
				t.Fatalf("hold = %q, want %q", hold, tc.want)
			}
		})
	}
}

// TestFilingExitLines: a not-ready park names where each unsettled filing
// went and the command that starts it now.
func TestFilingExitLines(t *testing.T) {
	root, _ := nextFixture(t)
	tagIdea(t, root, "beta", "tagged-fix")
	if _, err := runopen.Open(root, "beta", run.Options{
		ID: "judgment", Workflow: "idea", SeedDocs: map[string]string{"idea": "# Judgment\n"},
	}); err != nil {
		t.Fatal(err)
	}
	operatorSourceBlocked(t, root, "study", "Waiting on `tagged-fix`.",
		"- [x] `tagged-fix` (sdlc) — Tagged\n- [x] `judgment` — Untagged\n", time.Now().Add(-time.Hour))
	md := mustLoad(t, root, "beta", "study")
	got := stageNotReadyMessage(root, "sdlc", md, "design", []string{"gate not satisfied"})
	for _, want := range []string{
		"next: moe sdlc design beta/study",
		"filed: beta/tagged-fix (idea, tagged sdlc) — an armed serve promotes and rides it; now: moe sdlc new --from-idea beta/tagged-fix\n  this run resumes when it settles",
		"filed: beta/judgment (idea, untagged — yours to promote); now: moe sdlc new --from-idea beta/judgment\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "nothing will resume") {
		t.Errorf("a named filing still printed the nothing-resumes line:\n%s", got)
	}
}
