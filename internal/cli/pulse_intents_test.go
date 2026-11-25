package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// intentFixture is spawnFixture plus the root project, which a gate may
// open intents in.
func intentFixture(t *testing.T) string {
	t.Helper()
	root := spawnFixture(t)
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	return root
}

// seedIntent opens an in-progress intent with a canvas.
func seedIntent(t *testing.T, root, projectID, slug, body string) {
	t.Helper()
	if _, err := run.New(root, projectID, run.Options{
		ID:       slug,
		Workflow: dash.IntentWorkflow,
		SeedDocs: map[string]string{dash.IntentDocID: body},
	}); err != nil {
		t.Fatal(err)
	}
}

func seedIdea(t *testing.T, root, projectID, slug string) {
	t.Helper()
	if _, err := run.New(root, projectID, run.Options{
		ID:       slug,
		Workflow: dash.IdeaWorkflow,
		SeedDocs: map[string]string{dash.IdeaDocID: "# " + slug + "\n"},
	}); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, root, projectID, slug string) *run.Metadata {
	t.Helper()
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestPulseGateIntentGrammar(t *testing.T) {
	refused := []struct{ name, payload, want string }{
		{"open and close", `{"status":"ok","intents":[{"open":"moe/a","close":"moe/a","title":"t","body":"b","why":"w"}]}`, `exactly one of`},
		{"neither", `{"status":"ok","intents":[{"why":"w"}]}`, `exactly one of`},
		{"open without body", `{"status":"ok","intents":[{"open":"moe/a","title":"t","why":"w"}]}`, `"title" and "body" are required`},
		{"close with a body", `{"status":"ok","intents":[{"close":"moe/a","body":"b","why":"w"}]}`, `belong to open`},
		{"close without why", `{"status":"ok","intents":[{"close":"moe/a"}]}`, `"why" is required`},
		{"unqualified intent", `{"status":"ok","intents":[{"close":"a","why":"w"}]}`, `expected <project>/<run>`},
		{"unknown key on an act", `{"status":"ok","intents":[{"close":"moe/a","why":"w","reason":"x"}]}`, `unknown field "reason"`},
		{"unqualified serves run", `{"status":"ok","serves":[{"run":"x","intent":"moe/a"}]}`, `serves[0]: run`},
		{"unqualified serves intent", `{"status":"ok","serves":[{"run":"moe/x","intent":"a"}]}`, `serves[0]: intent`},
		{"unknown key on serves", `{"status":"ok","serves":[{"run":"moe/x","intnet":"moe/a"}]}`, `unknown field "intnet"`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			root := spawnFixture(t)
			writePulseGate(t, root, "moe", "pulse-x", tc.payload)
			if _, err := readPulseGate(root, "moe", "pulse-x"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal = %v, want it to name %s", err, tc.want)
			}
		})
	}
	root := spawnFixture(t)
	writePulseGate(t, root, "moe", "pulse-x", `{"status":"ok",
		"serves":[{"run":"moe/x","intent":"moe/a"},{"run":"moe/y","intent":""}],
		"intents":[{"open":"_cabinet/b","title":"t","body":"b","why":"w"},{"close":"moe/a","why":"w"}]}`)
	g, err := readPulseGate(root, "moe", "pulse-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Serves) != 2 || len(g.Intents) != 2 {
		t.Fatalf("gate = %+v", g)
	}
}

// TestApplyPulseGateIntents drives the whole apply order: an intent the
// gate opens, a run the gate mints, links naming both, a link to an idea,
// semantic refusals that skip without stopping the rest, and a close.
func TestApplyPulseGateIntents(t *testing.T) {
	root := intentFixture(t)
	seedIntent(t, root, "moe", "goal", "# Goal\n\nDone when: x.\n")
	seedIntent(t, root, "moe", "done-goal", "# Done goal\n\nDone when: y.\n")
	seedIntent(t, root, "moe", "shut", "# Shut\n")
	if err := runopen.CloseCapture(root, "moe", "shut"); err != nil {
		t.Fatal(err)
	}
	seedIdea(t, root, "moe", "some-idea")
	before := strings.TrimSpace(gittest.Output(t, root, "rev-parse", "HEAD"))

	gate := pulseGate{
		Status: "ok",
		Loose:  []pulseRunSpec{{Slug: "moe/fresh-fix", Title: "Fresh"}},
		Serves: []pulseServes{
			{Run: "moe/fresh-fix", Intent: "_cabinet/disk-exhaustion"},
			{Run: "moe/some-idea", Intent: "moe/goal"},
			{Run: "moe/no-such-run", Intent: "moe/goal"},
			{Run: "moe/some-idea", Intent: "moe/shut"},
		},
		Intents: []pulseIntentAct{
			{Open: "_cabinet/disk-exhaustion", Title: "Disk never fills", Body: "The box ran out.\n\n## Done when\n\n- alerts fire", Why: "three runs"},
			{Open: "moe/goal", Title: "Dup", Body: "b", Why: "w"},
			{Close: "moe/done-goal", Why: "moe/x merged"},
		},
	}
	var errb bytes.Buffer
	minted := applyPulseGate(root, "pulse-1", gate, io.Discard, &errb)

	opened := mustLoad(t, root, project.RootID, "disk-exhaustion")
	if opened.Workflow != dash.IntentWorkflow || opened.SpawnedBy != project.RootID+"/pulse-1" {
		t.Fatalf("opened intent = %+v", opened)
	}
	canvas, _ := os.ReadFile(filepath.Join(root, run.ContentPath(project.RootID, "disk-exhaustion", dash.IntentDocID)))
	if !strings.HasPrefix(string(canvas), "# Disk never fills\n\nThe box ran out.") {
		t.Errorf("seeded canvas = %q", canvas)
	}
	fresh := mustLoad(t, root, "moe", minted["moe/fresh-fix"])
	if fresh.Intent != "_cabinet/disk-exhaustion" {
		t.Errorf("minted run intent = %q", fresh.Intent)
	}
	if got := mustLoad(t, root, "moe", "some-idea").Intent; got != "moe/goal" {
		t.Errorf("idea intent = %q, want moe/goal (the closed-intent link must skip)", got)
	}
	done := mustLoad(t, root, "moe", "done-goal")
	if done.Status != run.StatusClosed || done.ClosedBy != project.RootID+"/pulse-1" {
		t.Errorf("closed intent = %+v", done)
	}
	for _, want := range []string{"slug already names", "moe/no-such-run", "intent moe/shut is closed", "opened intent _cabinet/disk-exhaustion", "closed intent moe/done-goal"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb.String())
		}
	}
	// One link commit, machine-marked, between the mint and the close.
	subjects := gittest.Output(t, root, "log", "--format=%s", before+"..HEAD")
	if n := strings.Count(subjects, "Link 2 run(s) to intents"); n != 1 {
		t.Errorf("link commits = %d in:\n%s", n, subjects)
	}
	link := gittest.Output(t, root, "log", "-1", "--format=%B", "--grep=^Link ")
	if !strings.Contains(link, "MoE-Consent:") || !strings.Contains(link, "MoE-Spawned-By: _cabinet/pulse-1") {
		t.Errorf("link commit = %q", link)
	}
	order := strings.Split(strings.TrimSpace(subjects), "\n")
	if !strings.HasPrefix(order[0], "Close intent moe/done-goal") || !strings.HasPrefix(order[len(order)-1], "Open run _cabinet/disk-exhaustion") {
		t.Errorf("apply order (newest first):\n%s", subjects)
	}
	closeMsg := gittest.Output(t, root, "log", "-1", "--format=%B", "--grep=^Close intent")
	if !strings.Contains(closeMsg, "moe/x merged") {
		t.Errorf("close commit body lacks the why: %q", closeMsg)
	}
	if out := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("tree dirty after apply:\n%s", out)
	}

	// An empty intent clears the link.
	applyPulseServes(root, "_cabinet/pulse-2", []pulseServes{{Run: "moe/some-idea"}}, nil, io.Discard)
	if got := mustLoad(t, root, "moe", "some-idea").Intent; got != "" {
		t.Errorf("cleared intent = %q", got)
	}
}

// TestPulseCloseIsOneTry: a pulse closes an intent, the operator reopens
// it, and the next gate close is refused by name while the operator's
// own close still works. The links survive the round trip.
func TestPulseCloseIsOneTry(t *testing.T) {
	root := intentFixture(t)
	t.Chdir(root)
	seedIntent(t, root, "moe", "goal", "# Goal\n")
	seedIdea(t, root, "moe", "linked")
	applyPulseServes(root, "_cabinet/pulse-1", []pulseServes{{Run: "moe/linked", Intent: "moe/goal"}}, nil, io.Discard)

	closePulseIntents(root, "_cabinet/pulse-1", []pulseIntentAct{{Close: "moe/goal", Why: "met"}}, io.Discard)
	if md := mustLoad(t, root, "moe", "goal"); md.Status != run.StatusClosed {
		t.Fatalf("status = %s", md.Status)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "reopen", "moe/goal"}, &out, &errb); code != 0 {
		t.Fatalf("intent reopen: %d %s", code, errb.String())
	}
	md := mustLoad(t, root, "moe", "goal")
	if md.Status != run.StatusInProgress || md.ClosedBy == "" {
		t.Fatalf("reopened = %+v", md)
	}
	if got := mustLoad(t, root, "moe", "linked").Intent; got != "moe/goal" {
		t.Errorf("link lost across reopen: %q", got)
	}

	errb.Reset()
	closePulseIntents(root, "_cabinet/pulse-2", []pulseIntentAct{{Close: "moe/goal", Why: "met again"}}, &errb)
	if !strings.Contains(errb.String(), "a pulse closed it once and the operator reopened it") {
		t.Errorf("second close stderr = %q", errb.String())
	}
	if md := mustLoad(t, root, "moe", "goal"); md.Status != run.StatusInProgress {
		t.Fatalf("second gate close went through: %s", md.Status)
	}
	if code := Run([]string{"intent", "close", "moe/goal"}, &out, &errb); code != 0 {
		t.Fatalf("operator close: %d %s", code, errb.String())
	}
	if md := mustLoad(t, root, "moe", "goal"); md.Status != run.StatusClosed {
		t.Fatalf("operator close: %s", md.Status)
	}
}

// TestIntentLinkFollowsPromotionAndReopenNotHarvest: the link rides idea
// promotion and an sdlc reopen successor, and a harvested followup of a
// linked run starts unlinked.
func TestIntentLinkFollowsPromotionAndReopenNotHarvest(t *testing.T) {
	root := intentFixture(t)
	t.Chdir(root)
	seedIntent(t, root, "moe", "goal", "# Goal\n")
	seedIdea(t, root, "moe", "carry-me")
	src, err := run.New(root, "moe", run.Options{ID: "prior-run", Workflow: "sdlc", SeedDocs: map[string]string{"design": "# Prior\n"}})
	if err != nil {
		t.Fatal(err)
	}
	applyPulseServes(root, "_cabinet/pulse-1", []pulseServes{
		{Run: "moe/carry-me", Intent: "moe/goal"},
		{Run: "moe/prior-run", Intent: "moe/goal"},
	}, nil, io.Discard)

	promoted, err := runopen.Promote(root, "moe", "carry-me", runopen.PromoteOptions{Workflow: "sdlc", FirstStage: "design"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Run.Intent != "moe/goal" || mustLoad(t, root, "moe", promoted.Run.ID).Intent != "moe/goal" {
		t.Errorf("promoted run intent = %q", promoted.Run.Intent)
	}

	writeFollowups(t, root, "moe", src.ID, "- [ ] `tidy-thing` — Tidy a thing\n\n  Body.\n")
	if err := harvestFollowups(root, "moe", src.ID, "sdlc"); err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t, root, "moe", "tidy-thing").Intent; got != "" {
		t.Errorf("harvested followup inherited intent %q", got)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m", "test: land harvest")

	setRunStatus(t, root, "moe", src.ID, run.StatusClosed)
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "reopen", "moe/prior-run", "--park"}, &out, &errb); code != 0 {
		t.Fatalf("sdlc reopen: %d %s", code, errb.String())
	}
	var successor *run.Metadata
	mds, _ := run.Scan(root)
	for _, md := range mds {
		if md.ReopenOf == "prior-run" {
			successor = md
		}
	}
	if successor == nil || successor.Intent != "moe/goal" {
		t.Fatalf("reopen successor = %+v", successor)
	}
}

// TestPulseKickoffMapsWork: the kickoff lists a linked run under its
// intent, with the intent's canvas path and pulse mark, and an unlinked
// new run under Unmapped.
func TestPulseKickoffMapsWork(t *testing.T) {
	root := intentFixture(t)
	seedIntent(t, root, "moe", "goal", "# The goal\n")
	seedIdea(t, root, "moe", "linked")
	seedIdea(t, root, "moe", "loose-end")
	applyPulseServes(root, "_cabinet/pulse-1", []pulseServes{{Run: "moe/linked", Intent: "moe/goal"}}, nil, io.Discard)
	openPulseIntents(root, "_cabinet/pulse-1", []pulseIntentAct{{Open: "_cabinet/incident", Title: "Incident", Body: "b", Why: "w"}}, io.Discard)

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan")
	}
	got := openIntentsBlock(sc)
	for _, want := range []string{
		"- `moe/goal` — The goal\n  canvas: " + filepath.Join(root, run.ContentPath("moe", "goal", dash.IntentDocID)) +
			"\n  status: " + filepath.Join(root, run.StatusPath("moe", "goal")),
		"  - `moe/linked` (idea, in_progress)",
		"- `_cabinet/incident` — Incident (opened by pulse)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("open intents block missing %q:\n%s", want, got)
		}
	}
	unmapped := unmappedBlock(sc, time.Now().Add(-time.Hour), "")
	if !strings.Contains(unmapped, "`moe/loose-end`") || strings.Contains(unmapped, "`moe/linked`") {
		t.Errorf("unmapped block:\n%s", unmapped)
	}
	if strings.Contains(unmappedBlock(sc, time.Now().Add(time.Hour), ""), "loose-end") {
		t.Error("work with no activity since the last pulse is not new")
	}
	full := pulseKickoffFromScan(sc, root, "pulse-x", io.Discard)
	if !strings.Contains(full, "Open intents") || !strings.Contains(full, "Unmapped") || !strings.Contains(full, `"serves"`) {
		t.Errorf("kickoff lacks the intent blocks:\n%s", full)
	}
}

// TestPulsePromptSeesEveryProjectsIntents: the root project's section
// inlines every project's open intents, the root's first; another
// project's section is the same as before apart from its closing lines.
func TestPulsePromptSeesEveryProjectsIntents(t *testing.T) {
	root := intentFixture(t)
	seedIntent(t, root, project.RootID, "whole", "# Whole bureaucracy\n")
	seedIntent(t, root, "moe", "goal", "# Moe goal\n")
	got := intentsReferenceSection(root, project.RootID)
	top, own := strings.Index(got, "## Bureaucracy intents"), strings.Index(got, "## Intents of `moe`")
	if top < 0 || own < top || !strings.Contains(got, "### `goal` — Moe goal") {
		t.Fatalf("root section:\n%s", got)
	}
	other := intentsReferenceSection(root, "moe")
	if strings.Contains(other, "Intents of") || !strings.Contains(other, "## Project intents") || !strings.Contains(other, "the pulse keeps each one's Status") {
		t.Fatalf("project section:\n%s", other)
	}
}

// TestPulseStagesOpenIntentStatus: the pulse turn's whitelist is the
// status.md of every open intent that has one, and nothing else — no
// canvas, no closed intent's status, no idea.
func TestPulseStagesOpenIntentStatus(t *testing.T) {
	root := intentFixture(t)
	seedIntent(t, root, "moe", "goal", "# Goal\n")
	seedIntent(t, root, "moe", "bare", "# Bare\n")
	seedIntent(t, root, "moe", "shut", "# Shut\n")
	if err := runopen.CloseCapture(root, "moe", "shut"); err != nil {
		t.Fatal(err)
	}
	seedIdea(t, root, "moe", "an-idea")
	for _, slug := range []string{"goal", "shut"} {
		if err := os.WriteFile(filepath.Join(root, run.StatusPath("moe", slug)), []byte("2026-11-24, pulse\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := openIntentStatusPaths(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := run.StatusPath("moe", "goal")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("paths = %v, want [%s]", got, want)
	}
}

func TestIntentRunsGroupsLinkedWork(t *testing.T) {
	root := intentFixture(t)
	t.Chdir(root)
	seedIntent(t, root, "moe", "goal", "# Goal\n")
	seedIdea(t, root, "moe", "live-one")
	if _, err := run.New(root, "moe", run.Options{ID: "shipped", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	applyPulseServes(root, "_cabinet/pulse-1", []pulseServes{
		{Run: "moe/live-one", Intent: "moe/goal"}, {Run: "moe/shipped", Intent: "moe/goal"},
	}, nil, io.Discard)
	setRunStatus(t, root, "moe", "shipped", run.StatusMerged)
	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "runs", "moe/goal"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	live, landed := strings.Index(got, "live (1)"), strings.Index(got, "landed (1)")
	if live < 0 || landed < live || !strings.Contains(got, "moe/shipped\tsdlc\tmerged") {
		t.Fatalf("output:\n%s", got)
	}

	in := gatherIntents(root, mustScan(t, root))
	if len(in) != 1 || in[0].Open != 1 || in[0].Landed != 1 {
		t.Fatalf("intent input = %+v", in)
	}
}

func mustScan(t *testing.T, root string) []*run.Metadata {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	return mds
}

// TestServesSkipsRunWithOpenSession drives a pulse's `serves` link into
// a run's first design turn: the fake agent runs applyPulseServes from
// another process mid-turn, as a clock pulse would. The link must skip
// rather than write run.json under the session, whose close would then
// conflict on the rebase; once the session lands, the run is still
// offered in Unmapped and the next pulse links it.
func TestServesSkipsRunWithOpenSession(t *testing.T) {
	root, _, _ := designFixture(t, "")
	seedIntent(t, root, "tele", "goal", "# Goal\n")
	before := time.Now().Add(-time.Minute)
	servesLog := filepath.Join(t.TempDir(), "serves.log")
	t.Setenv("MOE_TEST_SERVES_BIN", os.Args[0])
	t.Setenv("MOE_TEST_SERVES_LOG", servesLog)
	fakeClaudeOnPath(t, `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
 if [ "$next" = 1 ]; then prompt=$a; next=0; fi
 case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
MOE_TEST_SERVES_ROOT="$MOE_HOME" "$MOE_TEST_SERVES_BIN" -test.run='^TestServesTurnHelper$' > "$MOE_TEST_SERVES_LOG" 2>&1
printf '\nUnresolved: the service contract needs investigation.\n' >> "$canvas"
`)
	code, out := designTurn(t, "progress")
	helper, _ := os.ReadFile(servesLog)
	if code != 0 || strings.Contains(out, "rebase") {
		t.Fatalf("design turn exit %d:\n%s\nserves helper:\n%s", code, out, helper)
	}
	if !strings.Contains(string(helper), "has an open session") {
		t.Fatalf("serves did not skip the reserved run:\n%s", helper)
	}
	if md := mustLoad(t, root, "tele", "design-gate"); md.Intent != "" {
		t.Fatalf("link landed under an open session: %q", md.Intent)
	}

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan")
	}
	if !strings.Contains(unmappedBlock(sc, before, ""), "`tele/design-gate`") {
		t.Fatalf("skipped run not offered again:\n%s", unmappedBlock(sc, before, ""))
	}
	var errs bytes.Buffer
	applyPulseServes(root, "_cabinet/pulse-2", []pulseServes{{Run: "tele/design-gate", Intent: "tele/goal"}}, nil, &errs)
	if md := mustLoad(t, root, "tele", "design-gate"); md.Intent != "tele/goal" {
		t.Fatalf("link after the session landed: %q\n%s", md.Intent, errs.String())
	}
}

// TestServesTurnHelper is the pulse side of TestServesSkipsRunWithOpenSession,
// run by the fake agent in its own process.
func TestServesTurnHelper(t *testing.T) {
	root := os.Getenv("MOE_TEST_SERVES_ROOT")
	if root == "" {
		return
	}
	applyPulseServes(root, "_cabinet/pulse-1", []pulseServes{{Run: "tele/design-gate", Intent: "tele/goal"}}, nil, os.Stdout)
}

// TestUnmappedLooksBackAWeek: a run opened before the last pulse is still
// offered while it moves, within the window; older work is not.
func TestUnmappedLooksBackAWeek(t *testing.T) {
	root := intentFixture(t)
	for slug, age := range map[string]int{"recent": 3, "old": 10} {
		seedIdea(t, root, "moe", slug)
		md := mustLoad(t, root, "moe", slug)
		md.Created = time.Now().AddDate(0, 0, -age).Local().Format("2006-01-02")
		if err := run.Save(root, md); err != nil {
			t.Fatal(err)
		}
		if err := run.StageAndCommit(root, "age "+slug+"\n\nMoE-Run: moe/"+slug+"\n", filepath.Join(run.Dir("moe", slug), "run.json")); err != nil {
			t.Fatal(err)
		}
	}
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatal("scan")
	}
	got := unmappedBlock(sc, time.Now().Add(-time.Hour), "")
	if !strings.Contains(got, "`moe/recent`") || strings.Contains(got, "`moe/old`") {
		t.Fatalf("unmapped block:\n%s", got)
	}
}
