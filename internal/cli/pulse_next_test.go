package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// nextFixture is a scratch bureaucracy of two projects with the agent's
// stage and ship seams captured, so a sweep's ride is observable.
func nextFixture(t *testing.T) (string, *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ := kickFixture(t)
	seedGitHubProject(t, root, "beta")
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	return root, stages
}

// sweepWithGate runs one survey in the root project whose agent writes
// payload as its gate.
func sweepWithGate(t *testing.T, root, payload string) (int, string) {
	t.Helper()
	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, payload)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })
	var errb bytes.Buffer
	code := runPulseSurvey(root, project.RootID, "", nil, io.Discard, &errb)
	return code, errb.String()
}

func mintIn(t *testing.T, root, projectID, slug string) string {
	t.Helper()
	m := &pulseMinter{root: root, projectID: projectID, spawnedBy: project.RootID + "/seed"}
	id := m.mint(pulseRunSpec{Slug: slug, Title: slug}, io.Discard, io.Discard)
	if id == "" {
		t.Fatalf("could not mint %s/%s", projectID, slug)
	}
	return id
}

func tagIdea(t *testing.T, root, projectID, slug string) {
	t.Helper()
	if _, err := runopen.Open(root, projectID, run.Options{
		ID: slug, Workflow: "idea", PromoteTo: "sdlc",
		SeedDocs: map[string]string{"idea": "# " + slug + "\n"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNextRidesExactlyTheNamedRunAcrossProjects(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	other := mintIn(t, root, "moe", "other-work")
	picked := mintIn(t, root, "beta", "picked-work")

	code, stderr := sweepWithGate(t, root,
		`{"status":"ok","next":{"run":"beta/`+picked+`","why":"it unblocks moe"}}`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if len(*stages) == 0 {
		t.Fatalf("the pick did not ride: %s", stderr)
	}
	for _, inv := range *stages {
		if inv.projectID != "beta" || inv.runID != picked {
			t.Fatalf("rode %s/%s, want only beta/%s (other run %s)", inv.projectID, inv.runID, picked, other)
		}
	}
	closed := closedRootPulses(t, root)
	if len(closed) != 1 {
		t.Fatalf("closed root pulses = %v", closed)
	}
	canvas := readPulseCanvas(t, root, project.RootID, closed[0])
	if !strings.Contains(canvas, "## Next") || !strings.Contains(canvas, "beta/"+picked) || !strings.Contains(canvas, "it unblocks moe") {
		t.Fatalf("canvas does not record the pick:\n%s", canvas)
	}
}

func closedRootPulses(t *testing.T, root string) []string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, md := range mds {
		if md.Project == project.RootID && md.Workflow == pulseWorkflow && md.Status == run.StatusClosed {
			out = append(out, md.ID)
		}
	}
	return out
}

func TestNextPromotesOnlyThePickedTaggedIdea(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	tagIdea(t, root, "beta", "picked-idea")
	tagIdea(t, root, "beta", "waiting-idea")

	code, stderr := sweepWithGate(t, root,
		`{"status":"ok","next":{"run":"beta/picked-idea","why":"tagged and ready"}}`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	picked, err := run.Load(root, "beta", "picked-idea")
	if err != nil || picked.Status != run.StatusPromoted {
		t.Fatalf("picked idea = %+v, %v; %s", picked, err, stderr)
	}
	waiting, err := run.Load(root, "beta", "waiting-idea")
	if err != nil || waiting.Status != run.StatusInProgress {
		t.Fatalf("other tagged idea moved: %+v, %v", waiting, err)
	}
	if len(*stages) == 0 || (*stages)[0].projectID != "beta" {
		t.Fatalf("promoted run did not ride: %+v; %s", *stages, stderr)
	}
}

func TestNextInAPausedProjectStartsNothing(t *testing.T) {
	defer withRideMode(rideDynamic)()
	defer withClockInvoked()()
	root, stages := nextFixture(t)
	picked := mintIn(t, root, "beta", "picked-work")
	tagIdea(t, root, "beta", "tagged-idea")
	if err := project.SetMode(root, "beta", project.ModePaused); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{picked, "tagged-idea"} {
		code, stderr := sweepWithGate(t, root,
			`{"status":"ok","next":{"run":"beta/`+target+`","why":"try it"}}`)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "paused mode") {
			t.Fatalf("stderr = %q, want the mode hold", stderr)
		}
	}
	if len(*stages) != 0 {
		t.Fatalf("a paused project rode: %+v", *stages)
	}
	if idea, _ := run.Load(root, "beta", "tagged-idea"); idea.Status != run.StatusInProgress {
		t.Fatalf("a paused project's idea was promoted: %s", idea.Status)
	}
}

func TestNoNextStartsNothing(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	mintIn(t, root, "moe", "parked-work")
	tagIdea(t, root, "beta", "tagged-idea")

	code, stderr := sweepWithGate(t, root, `{"status":"ok"}`)
	if code != 0 || !strings.Contains(stderr, "nothing picked") {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if len(*stages) != 0 {
		t.Fatalf("rode without a pick: %+v", *stages)
	}
	if idea, _ := run.Load(root, "beta", "tagged-idea"); idea.Status != run.StatusInProgress {
		t.Fatalf("tagged idea promoted without a pick: %s", idea.Status)
	}
}

func TestHandTypedSweepNeverRides(t *testing.T) {
	root, stages := nextFixture(t)
	picked := mintIn(t, root, "beta", "picked-work")
	code, stderr := sweepWithGate(t, root,
		`{"status":"ok","next":{"run":"beta/`+picked+`","why":"try it"}}`)
	if code != 0 || len(*stages) != 0 {
		t.Fatalf("exit=%d stages=%+v stderr=%s", code, *stages, stderr)
	}
}

func TestLooseEntryOpensInItsProjectAndCanBePicked(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	code, stderr := sweepWithGate(t, root, `{"status":"ok",`+
		`"loose":[{"slug":"beta/fresh-fix","title":"Fresh","why":"found it"},`+
		`{"slug":"_cabinet/nope","title":"No source","why":"refused"},`+
		`{"slug":"unprefixed","title":"No project","why":"refused"}],`+
		`"next":{"run":"beta/fresh-fix","why":"it is small"}}`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	md, err := run.Load(root, "beta", "fresh-fix")
	if err != nil || md.SpawnedBy == "" || !strings.HasPrefix(md.SpawnedBy, project.RootID+"/") {
		t.Fatalf("minted run = %+v, %v", md, err)
	}
	if len(*stages) == 0 || (*stages)[0].runID != "fresh-fix" {
		t.Fatalf("minted pick did not ride: %+v; %s", *stages, stderr)
	}
	for _, want := range []string{"names the root project", "must be <project>/<slug>"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if _, err := run.Load(root, project.RootID, "nope"); err == nil {
		t.Error("a run was opened in the root project")
	}
}

func TestDesignOnlyPickRidesOneStage(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	code, stderr := sweepWithGate(t, root, `{"status":"ok",`+
		`"loose":[{"slug":"beta/weigh-it","title":"Weigh it","why":"a judgement","design_only":true,"design":"# Brief\n"}],`+
		`"next":{"run":"beta/weigh-it","why":"worth a design"}}`)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if got := kickStages(*stages); len(got) != 1 || got[0] != "weigh-it:design" {
		t.Fatalf("stages = %v, want the one design turn; %s", got, stderr)
	}
}

// TestNextHoldsWhatTheFloorHolds: a machine-ridden run parked at a blocked
// gate and a reaped run are the floor's to hold, whatever the survey picks.
func TestNextOnAReapedRunIsHeld(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	id := mintIn(t, root, "beta", "reaped-work")
	md, err := run.Load(root, "beta", id)
	if err != nil {
		t.Fatal(err)
	}
	md.Reaped = &run.ReapNote{Doc: "design", At: "2099-01-01T00:00:00Z", Tip: "abc"}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	msg := "reap\n\n" + trailers.Block{Run: id, Project: "beta", Workflow: "sdlc", Consent: "dynamic"}.String()
	if err := run.StageAndCommit(root, msg, filepath.Join(run.Dir("beta", id), "run.json")); err != nil {
		t.Fatal(err)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/`+id+`","why":"retry"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(stderr, "was reaped") {
		t.Fatalf("exit=%d stages=%+v stderr=%s", code, *stages, stderr)
	}
}

// The incident's shape: a run in one project is blocked on prose naming a
// run in another. Once that one merges, the survey sees both.
func TestKickoffShowsACrossProjectWaitAfterItsPrerequisiteLands(t *testing.T) {
	root, _ := nextFixture(t)
	stubGH(t, map[string]any{})
	blocked := mintIn(t, root, "moe", "blocked-work")
	prereq := mintIn(t, root, "beta", "prereq-work")
	canvas := filepath.Join(root, run.ContentPath("moe", blocked, "design"))
	if err := os.WriteFile(canvas, []byte("# Blocked\n\nWaiting on beta/"+prereq+" to merge.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run.StageAndCommit(root, "seed the wait", canvas); err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, root, "beta", prereq, run.StatusMerged)

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan failed")
	}
	got := pulseKickoffFromScan(sc, root, "pulse-now", io.Discard)
	for _, want := range []string{"`moe/" + blocked + "`", prereq, filepath.Join(root, "projects", "beta", "src")} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q:\n%s", want, got)
		}
	}
}

func TestKickoffSkipsQuietAndPausedProjects(t *testing.T) {
	defer withClockInvoked()()
	root, _ := nextFixture(t)
	stubGH(t, map[string]any{})
	seedGitHubProject(t, root, "quiet")
	seedGitHubProject(t, root, "asleep")
	if err := project.SetMode(root, "asleep", project.ModePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := runopen.Open(root, project.RootID, run.Options{
		ID: "pulse-old", Workflow: pulseWorkflow,
		SeedDocs: map[string]string{pulseDoc: pulseCanvasSkeleton},
	}); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(root, "commit", "--allow-empty", "-m", "later"); err != nil {
		t.Fatal(err)
	}
	mintIn(t, root, "beta", "live-work")

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan failed")
	}
	got := pulseKickoffFromScan(sc, root, "pulse-now", io.Discard)
	if !strings.Contains(got, "# Project `beta`") {
		t.Errorf("project with live work has no section:\n%s", got)
	}
	if strings.Contains(got, "# Project `quiet`") || strings.Contains(got, "# Project `asleep`") {
		t.Errorf("quiet or paused project got a section:\n%s", got)
	}
	if !strings.Contains(got, "Paused, so left alone this sweep: asleep") {
		t.Errorf("paused project not named:\n%s", got)
	}
}

func TestPulseNewRefusesAProject(t *testing.T) {
	spawnFixture(t)
	var out, errb bytes.Buffer
	code := Run([]string{"pulse", "new", "moe"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "`moe pulse new`") {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
}

func TestChainCommandIsGone(t *testing.T) {
	spawnFixture(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"chain", "edit"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), `unknown command "chain"`) {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
}

func TestClosedChainRunStillLoadsAndRenders(t *testing.T) {
	root := spawnFixture(t)
	md, err := runopen.Open(root, "moe", run.Options{
		ID: "old-batch", Workflow: "chain",
		SeedDocs: map[string]string{"chain": "# Chain\n\nWhy this batch existed.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, root, "moe", md.ID, run.StatusClosed)
	if err := git.Run(root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: moe/old-batch moe/other\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := run.BuildJournalIndex(root); err != nil {
		t.Fatalf("journal with an old chain trailer: %v", err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("dash exit=%d stderr=%q", code, errb.String())
	}
	if _, err := LookupWorkflow("chain"); err != nil {
		t.Fatalf("closed chain run's workflow no longer resolves: %v", err)
	}
}

// A machine-ridden run that parked on a blocked gate must stay reachable:
// the incident's run sat blocked at design, and a pick that cannot re-enter
// it could never move it.
func TestNextReentersARunParkedOnABlockedDesignGate(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	id := mintIn(t, root, "beta", "blocked-design")
	canvas := filepath.Join(root, run.ContentPath("beta", id, "design"))
	body := "# Design\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n\nWaiting on moe/other.\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := "work: update design\n\n" + trailers.Block{
		Run: id, Project: "beta", Workflow: "sdlc", Document: "design", Consent: "dynamic",
	}.String()
	if err := run.StageAndCommit(root, msg, canvas); err != nil {
		t.Fatal(err)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/`+id+`","why":"its prerequisite landed"}}`)
	if strings.Contains(stderr, "is held") || strings.Contains(stderr, "waiting at its first stage") {
		t.Fatalf("the floor held a blocked machine run: %s", stderr)
	}
	if got := kickStages(*stages); len(got) == 0 || got[0] != id+":design" {
		t.Fatalf("stages = %v (exit %d), want the design stage re-entered; %s", got, code, stderr)
	}
}

func TestNextHeldBySafeModeWithoutAnOperatorMark(t *testing.T) {
	defer withRideMode(rideDynamic)()
	defer withClockInvoked()()
	root, stages := nextFixture(t)
	id := mintIn(t, root, "beta", "unmarked-work")
	if err := project.SetMode(root, "beta", project.ModeSafe); err != nil {
		t.Fatal(err)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/`+id+`","why":"try"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(stderr, "safe mode") {
		t.Fatalf("exit=%d stages=%+v stderr=%s", code, *stages, stderr)
	}
}

func TestNextOnAnUnsettledDesignIsHeld(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	if _, err := runopen.Open(root, "beta", run.Options{
		ID: "typed-sketch", Workflow: "sdlc",
		SeedDocs: map[string]string{"design": "# Only a seed\n"},
	}); err != nil {
		t.Fatal(err)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/typed-sketch","why":"try"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(stderr, "the operator holds the trigger") {
		t.Fatalf("exit=%d stages=%+v stderr=%s", code, *stages, stderr)
	}
}

func TestNextOnAnUntaggedIdeaIsHeld(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages := nextFixture(t)
	if _, err := runopen.Open(root, "beta", run.Options{
		ID: "human-idea", Workflow: "idea",
		SeedDocs: map[string]string{"idea": "# Mine\n"},
	}); err != nil {
		t.Fatal(err)
	}
	code, stderr := sweepWithGate(t, root, `{"status":"ok","next":{"run":"beta/human-idea","why":"try"}}`)
	if code != 0 || len(*stages) != 0 || !strings.Contains(stderr, "untagged idea") {
		t.Fatalf("exit=%d stages=%+v stderr=%s", code, *stages, stderr)
	}
}

// One sweep surveys every project, so its exit moves every dispatched
// cursor, except where work it could not have seen landed in the window,
// and never for a project held out of the sweep.
func TestRootSweptMovesEachDispatchedCursorOnItsOwnEvidence(t *testing.T) {
	root, _ := nextFixture(t)
	quiet := mintIn(t, root, "moe", "quiet-work")
	busy := mintIn(t, root, "beta", "busy-work")
	_ = quiet
	base := map[string]string{}
	for _, p := range []string{"moe", "beta"} {
		tip, _, _, ok := projectJournalTip(root, p)
		if !ok {
			t.Fatalf("no tip for %s", p)
		}
		base[p] = tip
	}
	// A ride lands in beta during the sweep.
	msg := "work: update design\n\n" + trailers.Block{
		Run: busy, Project: "beta", Workflow: "sdlc", Document: "design", Consent: "dynamic",
	}.String()
	if err := git.Run(root, "commit", "--allow-empty", "-m", msg); err != nil {
		t.Fatal(err)
	}
	g := newHeartbeatGate(root)
	g.dispatched["moe"], g.dispatched["beta"] = base["moe"], base["beta"]

	g.Swept(project.RootID, true)

	if g.surveyed["moe"] != base["moe"] {
		t.Errorf("moe surveyed = %q, want its tip %q", g.surveyed["moe"], base["moe"])
	}
	if _, moved := g.surveyed["beta"]; moved {
		t.Error("beta's cursor moved past a ride the survey never saw")
	}
	if len(g.dispatched) != 0 {
		t.Errorf("dispatched not consumed: %v", g.dispatched)
	}
}

func TestKickoffPutsARunWithAnOperatorNoteFirst(t *testing.T) {
	root, _ := nextFixture(t)
	stubGH(t, map[string]any{})
	noted := mintIn(t, root, "beta", "noted-work")
	plain := mintIn(t, root, "beta", "plain-work")
	tagIdea(t, root, "beta", "noted-idea")
	for _, id := range []string{noted, "noted-idea"} {
		if _, err := input.Add(root, "beta", id, "Do this one next."); err != nil {
			t.Fatal(err)
		}
	}
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan failed")
	}
	got := pulseKickoffFromScan(sc, root, "pulse-now", io.Discard)
	for _, want := range []string{
		"`beta/" + noted + "` (sdlc) — operator note waiting",
		"`beta/noted-idea` (idea tagged sdlc) — operator note waiting",
		"comes first when you pick `next`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "`beta/"+plain+"` (sdlc) — operator note waiting") {
		t.Errorf("run with no note was marked:\n%s", got)
	}
}
