package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestRenderCascadeSummaryShapes pins the four summary shapes the
// design names: park-at-gate (no trailing clause), failure
// (— stopped), full ship (— shipped), and aborted ship (— stopped).
func TestRenderCascadeSummaryShapes(t *testing.T) {
	cases := []struct {
		name string
		res  cascadeResult
		want string
	}{
		{
			name: "park-at-gate",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 0},
				{stage: "test", code: 0},
			}},
			want: "cascade: code ok · test ok",
		},
		{
			name: "failed-stopped",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 1},
			}},
			want: "cascade: code failed (exit 1) — stopped",
		},
		{
			name: "yolo-shipped",
			res: cascadeResult{
				ran: []cascadeStepResult{
					{stage: "code", code: 0},
					{stage: "test", code: 0},
					{stage: "push", code: 0},
				},
				shipped: true,
			},
			want: "cascade: code ok · test ok · push ok — shipped",
		},
		{
			name: "yolo-aborted",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 0},
				{stage: "test", code: 2},
			}},
			want: "cascade: code ok · test failed (exit 2) — stopped",
		},
		{
			name: "empty-no-summary",
			res:  cascadeResult{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCascadeSummary(tc.res); got != tc.want {
				t.Fatalf("renderCascadeSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// stubSdlcStageCommands replaces the sdlc group's design/code/test/push
// commands with no-op recorders for the lifetime of the test. The
// returned map captures the args each stub received, in dispatch order,
// keyed by stage. Restores originals on cleanup. Used for the cascade's
// yolo-mode push-ship dispatch, which still goes through pushCmd.Run;
// non-ship dispatches now flow through openSdlcStage and are intercepted
// by stubOpenSdlcStage.
func stubSdlcStageCommands(t *testing.T, perStageExit map[string]int) map[string][][]string {
	t.Helper()
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	captured := map[string][][]string{}
	stages := []string{"design", "code", "test", "push"}
	for _, stage := range stages {
		stage := stage
		original := g.commands[stage]
		exit := perStageExit[stage]
		t.Cleanup(func() { g.commands[stage] = original })
		g.commands[stage] = &Command{
			Name: stage,
			Run: func(args []string, _, _ io.Writer) int {
				captured[stage] = append(captured[stage], append([]string(nil), args...))
				return exit
			},
		}
	}
	return captured
}

// openSdlcStageInvocation records one openSdlcStage dispatch — the
// stage name plus the (project, run) tuple. Tests assert on these
// directly instead of an args slice; the rename run carved away the
// `--one-shot` prefix that used to be the assertion target.
type openSdlcStageInvocation struct {
	stage     string
	projectID string
	runID     string
}

// stubOpenSdlcStage replaces openSdlcStage with a recorder for the
// duration of the test. perStageExit lets a test pin a non-zero exit
// for a named stage to drive cascade-failure behaviour. The returned
// slice records invocations in dispatch order across all stages.
func stubOpenSdlcStage(t *testing.T, perStageExit map[string]int) *[]openSdlcStageInvocation {
	t.Helper()
	var captured []openSdlcStageInvocation
	prev := openSdlcStage
	openSdlcStage = func(stage, projectID, runID string, _, _ io.Writer) int {
		captured = append(captured, openSdlcStageInvocation{stage, projectID, runID})
		return perStageExit[stage]
	}
	t.Cleanup(func() { openSdlcStage = prev })
	return &captured
}

// countInvocations returns the number of times stage appeared in invs,
// the count assertion both stubSdlcStageCommands callers wanted on
// captured[stage]. Tiny helper rather than a map projection to keep
// the test bodies readable.
func countInvocations(invs []openSdlcStageInvocation, stage string) int {
	n := 0
	for _, i := range invs {
		if i.stage == stage {
			n++
		}
	}
	return n
}

// TestCascadeFromGateRunsBetweenStartAndDestination pins the basic
// !<stage> shape: cascade walks from startStage up to (but not
// including) destination, dispatching each headless via
// openSdlcStage. No shipped flag — `!push` parks at the push gate.
func TestCascadeFromGateRunsBetweenStartAndDestination(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "push", md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("!<stage> cascade must not ship: %+v", res)
	}
	wantSteps := []string{"code", "test"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
		if res.ran[i].code != 0 {
			t.Fatalf("ran[%d].code = %d, want 0", i, res.ran[i].code)
		}
	}
	// Each stage got one openSdlcStage dispatch with the project/run pair.
	for _, stage := range wantSteps {
		if got := countInvocations(*captured, stage); got != 1 {
			t.Fatalf("stage %s dispatched %d times, want 1", stage, got)
		}
	}
	for _, inv := range *captured {
		if inv.projectID != "tele" || inv.runID != "fix-it" {
			t.Fatalf("openSdlcStage args = %+v, want (tele, fix-it)", inv)
		}
	}
	// push was NOT dispatched.
	if got := countInvocations(*captured, "push"); got != 0 {
		t.Fatalf("push must not dispatch on !push (parks at push gate): got %d", got)
	}
}

// TestCascadeFromGateYoloShipsAtPush pins the !! shape: cascade
// walks every remaining stage and ships at push. code/test go
// through openSdlcStage (headless), push goes through pushCmd.Run
// (merge path, no flags). No synthesis pre-call at push: `!!`
// defaults to fast-forward merge, whose commit body is bare, so
// the curation would write a canvas nothing reads.
func TestCascadeFromGateYoloShipsAtPush(t *testing.T) {
	openCaptured := stubOpenSdlcStage(t, nil)
	cmdCaptured := stubSdlcStageCommands(t, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("!! cascade must ship: %+v", res)
	}
	wantSteps := []string{"code", "test", "push"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	// code and test go through openSdlcStage; push must NOT.
	for _, stage := range []string{"code", "test"} {
		if got := countInvocations(*openCaptured, stage); got != 1 {
			t.Fatalf("stage %s openSdlcStage dispatched %d times, want 1", stage, got)
		}
	}
	if got := countInvocations(*openCaptured, "push"); got != 0 {
		t.Fatalf("push must not dispatch via openSdlcStage (no cascade synth): got %d", got)
	}
	// push ship is a separate pushCmd.Run call with no flags.
	pushShip := cmdCaptured["push"]
	if len(pushShip) != 1 {
		t.Fatalf("push ship dispatched %d times, want 1: %v", len(pushShip), pushShip)
	}
	if got, want := strings.Join(pushShip[0], " "), "tele fix-it"; got != want {
		t.Fatalf("push ship args = %q, want %q (merge path, no flags)", got, want)
	}
}

// TestCascadeFromGateStopsOnFailure: the first non-zero exit stops
// the cascade and surfaces the failure in the summary. Stages
// downstream of the failure never dispatch.
func TestCascadeFromGateStopsOnFailure(t *testing.T) {
	captured := stubOpenSdlcStage(t, map[string]int{"code": 1})
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "push", md, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cascade exit=%d, want 1; stderr=%q", code, stderr.String())
	}
	if len(res.ran) != 1 || res.ran[0].stage != "code" || res.ran[0].code != 1 {
		t.Fatalf("ran = %+v, want [{code 1}]", res.ran)
	}
	if got := countInvocations(*captured, "test"); got != 0 {
		t.Fatalf("test must not dispatch after code failed: got %d invocations", got)
	}
}

// TestCascadeFromGateNoOpBehindStart: destination at or behind the
// start gate yields an empty result with exit 0 — the prompt re-asks
// at the same gate.
func TestCascadeFromGateNoOpBehindStart(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	// startStage=code, destination=design — design is behind code.
	res, code := cascadeFromGate("code", "design", md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("no-op cascade exit=%d, want 0; stderr=%q", code, stderr.String())
	}
	if len(res.ran) != 0 {
		t.Fatalf("ran = %+v, want []", res.ran)
	}
	if len(*captured) != 0 {
		t.Fatalf("no stage must dispatch on no-op cascade: got %+v", *captured)
	}
}

// TestCascadeFromGateNoOpDestinationEqualsStart: destination equal
// to startStage (e.g., `!code` at the design→code gate) is a no-op
// — same as behind. Pins the "destination is the gate you're at"
// interpretation.
func TestCascadeFromGateNoOpDestinationEqualsStart(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "code", md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("destination=start cascade exit=%d, want 0", code)
	}
	if len(res.ran) != 0 {
		t.Fatalf("ran = %+v, want []", res.ran)
	}
	if len(*captured) != 0 {
		t.Fatalf("no stage must dispatch when destination=start: got %+v", *captured)
	}
}

// TestPromptStageNextStageDispatchesCascade: typing `!test` at the
// design→code gate cascades through code (only) and re-prompts at
// the test gate. The summary line lands on stdout.
func TestPromptStageNextStageDispatchesCascade(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	// next is the post-design gate's "next" — code.
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// `!test` cascades to the test gate. The re-entered prompt sees
	// EOF after the cascade dispatch and declines.
	if _, err := io.WriteString(w, "!test\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "cascade: code ok") {
		t.Fatalf("expected summary line in stdout, got: %q", got)
	}
	// code dispatched once; test must NOT dispatch (it's the
	// destination, parked at its gate).
	if got := countInvocations(*captured, "code"); got != 1 {
		t.Fatalf("code dispatched %d times, want 1", got)
	}
	if got := countInvocations(*captured, "test"); got != 0 {
		t.Fatalf("test must not dispatch on !test: got %d", got)
	}
}

// TestPromptStageNextStageRejectsUnknownStage: typing `!nonsense`
// prints a list of valid stages and declines.
func TestPromptStageNextStageRejectsUnknownStage(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!nonsense\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "unknown stage") {
		t.Fatalf("expected unknown-stage error, got stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "design, code, test, push") {
		t.Fatalf("expected stage list in error, got stderr=%q", stderr.String())
	}
	if len(*captured) != 0 {
		t.Fatalf("no stage must dispatch on unknown cascade target: got %+v", *captured)
	}
}

// TestPromptStageNextStageShowsCascadeLegend: the prompt legend
// names the cascade syntax for sdlc workflows.
func TestPromptStageNextStageShowsCascadeLegend(t *testing.T) {
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "!<stage>=cascade to gate") {
		t.Fatalf("expected cascade legend in stdout, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "!!=cascade and ship") {
		t.Fatalf("expected !! legend in stdout, got: %q", stdout.String())
	}
}

// TestPromptStageNextStageNoCascadeLegendForNonSdlc: kb (non-sdlc)
// prompts must not advertise the cascade syntax — it's sdlc-only.
func TestPromptStageNextStageNoCascadeLegendForNonSdlc(t *testing.T) {
	next := &Command{Name: "ingest", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Workflow: "kb", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe kb ingest tele dns-basics", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d", code)
	}
	if strings.Contains(stdout.String(), "!<stage>") || strings.Contains(stdout.String(), "!!=") {
		t.Fatalf("non-sdlc prompt must not advertise cascade legend, got: %q", stdout.String())
	}
}

// TestPromptPushNextStageBangBangShips: typing `!!` at the push
// gate ships via the same path as `m` — same args to next.Run.
func TestPromptPushNextStageBangBangShips(t *testing.T) {
	var ran bool
	var gotArgs []string
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			ran = true
			gotArgs = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!!\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("push prompt exit=%d", code)
	}
	if !ran {
		t.Fatalf("!! at push gate must dispatch the merge path")
	}
	if got, want := strings.Join(gotArgs, " "), "tele fix-it"; got != want {
		t.Fatalf("push args = %q, want %q (merge path, no flags)", got, want)
	}
}

// TestPromptPushNextStageBangStageIsNoOp: `!<stage>` at the push
// gate is a no-op — every stage is at or behind. The push command
// must not dispatch; stderr surfaces a hint.
func TestPromptPushNextStageBangStageIsNoOp(t *testing.T) {
	var ran bool
	next := &Command{
		Name: "push",
		Run: func(_ []string, _, _ io.Writer) int {
			ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!test\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("push prompt exit=%d", code)
	}
	if ran {
		t.Fatalf("!<stage> at push gate must not dispatch ship")
	}
	if !strings.Contains(stderr.String(), "at or behind the push gate") {
		t.Fatalf("expected no-op hint on stderr, got: %q", stderr.String())
	}
}

// TestPromptPushNextStageShowsBangBangLegend: the push prompt
// legend mentions `!!` for sdlc workflows.
func TestPromptPushNextStageShowsBangBangLegend(t *testing.T) {
	next := &Command{Name: "push", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("push prompt exit=%d", code)
	}
	if !strings.Contains(stdout.String(), "!!=ship now") {
		t.Fatalf("expected !! legend at push gate, got: %q", stdout.String())
	}
}

// TestIndexOfStringFound and TestIndexOfStringMissing pin the tiny
// helper the cascade uses for stage lookups; one absent name would
// silently start the loop at index 0 (designs's gate), which would
// be a confusing failure mode.
func TestIndexOfStringFound(t *testing.T) {
	if got := indexOfString([]string{"a", "b", "c"}, "b"); got != 1 {
		t.Fatalf("indexOfString = %d, want 1", got)
	}
}

func TestIndexOfStringMissing(t *testing.T) {
	if got := indexOfString([]string{"a", "b", "c"}, "z"); got != -1 {
		t.Fatalf("indexOfString = %d, want -1", got)
	}
}
