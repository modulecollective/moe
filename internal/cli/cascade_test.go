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

// pushFromCascadeInvocation records one cascade-side push dispatch.
// args holds whatever the cascade passed through (today: just
// {project, run} for the merge path), defer is the typed
// *PushDeferredError the stub was configured to surface, exit is the
// int the stub returned. Tests assert directly on these fields.
type pushFromCascadeInvocation struct {
	args   []string
	defer_ *PushDeferredError
	exit   int
}

// stubPushFromCascade swaps pushFromCascade — the cascade's typed
// entry to runPushTyped — for a recorder. exit and deferred pin what
// the stub hands back: (exit, deferred) is the same shape the real
// runPushTyped uses on a deferred recovery (non-zero exit + typed
// error), and (exit, nil) covers the happy-ship and bare-failure
// paths. Returns a pointer to the captured invocations so the test
// can assert on call count and args. Original is restored on cleanup.
func stubPushFromCascade(t *testing.T, exit int, deferred *PushDeferredError) *[]pushFromCascadeInvocation {
	t.Helper()
	var captured []pushFromCascadeInvocation
	prev := pushFromCascade
	pushFromCascade = func(_ string, args []string, _, _ io.Writer) (int, error) {
		inv := pushFromCascadeInvocation{
			args:   append([]string(nil), args...),
			defer_: deferred,
			exit:   exit,
		}
		captured = append(captured, inv)
		if deferred != nil {
			return exit, deferred
		}
		return exit, nil
	}
	t.Cleanup(func() { pushFromCascade = prev })
	return &captured
}

// openSdlcStageInvocation records one openSdlcStage dispatch — the
// stage name, the (project, run) tuple, and the next-stage
// suppression flag. Tests assert on these directly instead of an
// args slice; the rename run carved away the
// `--one-shot` prefix that used to be the assertion target.
type openSdlcStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	suppressNextStage bool
}

// stubOpenSdlcStage replaces openSdlcStage with a recorder for the
// duration of the test. perStageExit lets a test pin a non-zero exit
// for a named stage to drive cascade-failure behaviour. The returned
// slice records invocations in dispatch order across all stages.
func stubOpenSdlcStage(t *testing.T, perStageExit map[string]int) *[]openSdlcStageInvocation {
	t.Helper()
	var captured []openSdlcStageInvocation
	prev := openSdlcStage
	openSdlcStage = func(stage, projectID, runID string, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openSdlcStageInvocation{stage, projectID, runID, suppressNextStage})
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
	res, code := cascadeFromGate("code", "push", false, md, &stdout, &stderr)
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
		if inv.projectID != "tele" || inv.runID != "fix-it" || !inv.suppressNextStage {
			t.Fatalf("openSdlcStage args = %+v, want (tele, fix-it, suppressNextStage=true)", inv)
		}
	}
	// push was NOT dispatched.
	if got := countInvocations(*captured, "push"); got != 0 {
		t.Fatalf("push must not dispatch on !push (parks at push gate): got %d", got)
	}
}

// TestCascadeFromGateYoloShipsAtPush pins the !! shape: cascade
// walks every remaining stage and ships at push. code/test go
// through openSdlcStage (headless), push goes through pushFromCascade
// (the typed entry that wraps runPushTyped — merge path, no flags).
// There is no separate cascade synthesis step: `!!` defaults to
// fast-forward merge and runPushTyped writes the merge-path push note.
func TestCascadeFromGateYoloShipsAtPush(t *testing.T) {
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, md, &stdout, &stderr)
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
		t.Fatalf("push must not dispatch via openSdlcStage: got %d", got)
	}
	for _, inv := range *openCaptured {
		if !inv.suppressNextStage {
			t.Fatalf("cascade openSdlcStage args = %+v, want suppressNextStage=true", inv)
		}
	}
	// push ship is a pushFromCascade call with the bare (project, run)
	// args — merge path, no --pr flag.
	if len(*pushCaptured) != 1 {
		t.Fatalf("push ship dispatched %d times, want 1: %v", len(*pushCaptured), *pushCaptured)
	}
	if got, want := strings.Join((*pushCaptured)[0].args, " "), "tele fix-it"; got != want {
		t.Fatalf("push ship args = %q, want %q (merge path, no flags)", got, want)
	}
}

// openTwinStageInvocation mirrors openSdlcStageInvocation for the
// twin headless dispatcher: stage name, (project, run), suppression.
type openTwinStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	suppressNextStage bool
}

// stubOpenTwinStage swaps openTwinStage for a recorder so cascade tests
// can drive twin yolo runs without invoking real stage sessions.
// perStageExit pins a non-zero exit for a named stage when needed.
func stubOpenTwinStage(t *testing.T, perStageExit map[string]int) *[]openTwinStageInvocation {
	t.Helper()
	var captured []openTwinStageInvocation
	prev := openTwinStage
	openTwinStage = func(stage, projectID, runID string, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openTwinStageInvocation{stage, projectID, runID, suppressNextStage})
		return perStageExit[stage]
	}
	t.Cleanup(func() { openTwinStage = prev })
	return &captured
}

// closeCommandInvocation records one cascade-side close dispatch — the
// args the cascade passed (today: ["--no-edit", project, run]) and the
// stub's chosen exit.
type closeCommandInvocation struct {
	args []string
	exit int
}

// stubGroupCloseCommand replaces the workflow group's close command
// with a recorder. The cascade reaches close via LookupGroup → Lookup,
// so swapping the entry on the live group is the smallest seam that
// catches the dispatch without standing up the real close machinery
// (state guards, repo lock, commit).
func stubGroupCloseCommand(t *testing.T, workflow string, exit int) *[]closeCommandInvocation {
	t.Helper()
	g, err := LookupGroup(workflow)
	if err != nil {
		t.Fatalf("LookupGroup(%q): %v", workflow, err)
	}
	prev := g.commands["close"]
	var captured []closeCommandInvocation
	g.commands["close"] = &Command{
		Name: "close",
		Run: func(args []string, _, _ io.Writer) int {
			captured = append(captured, closeCommandInvocation{args: append([]string(nil), args...), exit: exit})
			return exit
		},
	}
	t.Cleanup(func() { g.commands["close"] = prev })
	return &captured
}

// TestCascadeFromGateTwinYoloAutoCloses pins the twin `!!` shape: a
// twin cascade walks every reflect stage and then auto-closes the run.
// sdlc's push branch is the equivalent terminator; twin has no push, so
// the post-loop close dispatch handles "cascade and terminate" for
// workflows where `done → close` is the only path. --no-edit keeps
// the close non-interactive (followups harvested as-is).
func TestCascadeFromGateTwinYoloAutoCloses(t *testing.T) {
	stageCaptured := stubOpenTwinStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, "twin", 0)
	md := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("vision", "", false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("twin !! cascade must ship via close: %+v", res)
	}
	wantSteps := []string{"vision", "architecture", "patterns", "operations", "roadmap", "glossary", "finalize", "close"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	// Each reflect stage dispatched once via openTwinStage.
	for _, stage := range wantSteps[:len(wantSteps)-1] {
		got := 0
		for _, inv := range *stageCaptured {
			if inv.stage == stage {
				got++
			}
		}
		if got != 1 {
			t.Fatalf("stage %s dispatched %d times via openTwinStage, want 1", stage, got)
		}
	}
	// close must NOT go through openTwinStage — it's not a reflect stage.
	for _, inv := range *stageCaptured {
		if inv.stage == "close" {
			t.Fatalf("close must not dispatch via openTwinStage: %+v", inv)
		}
	}
	// close received --no-edit plus the (project, run) tuple.
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit moe reflect-2026-05-17"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
	}
	// Summary ends with the close step and the shipped marker.
	wantSummary := "cascade: vision ok · architecture ok · patterns ok · operations ok · roadmap ok · glossary ok · finalize ok · close ok — shipped"
	if got := renderCascadeSummary(res); got != wantSummary {
		t.Fatalf("summary = %q, want %q", got, wantSummary)
	}
}

// TestCascadeFromGateTwinBangStageDoesNotClose: a non-yolo
// `!<stage>` cascade for twin must not dispatch close — the operator
// asked for a partial walk, not a "complete the run" gesture. close is
// reserved for `!!`.
func TestCascadeFromGateTwinBangStageDoesNotClose(t *testing.T) {
	stubOpenTwinStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, "twin", 0)
	md := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("vision", "finalize", false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("!<stage> cascade must not ship: %+v", res)
	}
	if len(*closeCaptured) != 0 {
		t.Fatalf("close must not dispatch on !<stage>: %+v", *closeCaptured)
	}
}

// TestCascadeFromGateTwinYoloStopsOnStageFailure: a failing reflect
// stage stops the cascade — close must not fire. Mirrors the
// sdlc-stops-on-failure invariant one workflow over.
func TestCascadeFromGateTwinYoloStopsOnStageFailure(t *testing.T) {
	stubOpenTwinStage(t, map[string]int{"patterns": 1})
	closeCaptured := stubGroupCloseCommand(t, "twin", 0)
	md := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("vision", "", false, md, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cascade exit=%d, want 1; stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("a stopped cascade must not mark shipped: %+v", res)
	}
	if len(*closeCaptured) != 0 {
		t.Fatalf("close must not dispatch after a stage failure: %+v", *closeCaptured)
	}
}

// TestCascadeFromGateStopsOnFailure: the first non-zero exit stops
// the cascade and surfaces the failure in the summary. Stages
// downstream of the failure never dispatch.
func TestCascadeFromGateStopsOnFailure(t *testing.T) {
	captured := stubOpenSdlcStage(t, map[string]int{"code": 1})
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "push", false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("code", "design", false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("code", "code", false, md, &stdout, &stderr)
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

// TestCascadeFromGateOneStepDispatchesStartStageOnly pins the bare-`!`
// shape: oneStep=true dispatches exactly one stage (startStage),
// regardless of where it lands in the ladder. Mid-ladder cascade
// must not advance past startStage and must not auto-ship/close.
func TestCascadeFromGateOneStepDispatchesStartStageOnly(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", true, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("oneStep cascade must not ship: %+v", res)
	}
	if len(res.ran) != 1 || res.ran[0].stage != "code" {
		t.Fatalf("ran = %+v, want one step at code", res.ran)
	}
	if got := countInvocations(*captured, "code"); got != 1 {
		t.Fatalf("code dispatched %d times, want 1", got)
	}
	if got := countInvocations(*captured, "test"); got != 0 {
		t.Fatalf("test must not dispatch on oneStep: got %d", got)
	}
}

// TestCascadeFromGateOneStepAtTerminalStage pins the terminal-stage
// edge case: bare `!` at twin's post-glossary gate (next=finalize)
// dispatches finalize once and does NOT auto-close — that's the
// `!!`-only terminator. Distinguishes oneStep from yolo at the last
// stage, where successor-name math would otherwise reinterpret one
// as the other.
func TestCascadeFromGateOneStepAtTerminalStage(t *testing.T) {
	stageCaptured := stubOpenTwinStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, "twin", 0)
	md := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("finalize", "", true, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("oneStep at terminal stage must not ship: %+v", res)
	}
	if len(res.ran) != 1 || res.ran[0].stage != "finalize" {
		t.Fatalf("ran = %+v, want one step at finalize", res.ran)
	}
	dispatched := 0
	for _, inv := range *stageCaptured {
		if inv.stage == "finalize" {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("finalize dispatched %d times, want 1", dispatched)
	}
	if len(*closeCaptured) != 0 {
		t.Fatalf("oneStep must not dispatch close: %+v", *closeCaptured)
	}
}

// TestPromptStageNextStageBangAdvancesOneStage: typing bare `!` at
// the design→code gate dispatches code once (suppressNextStage=true)
// and re-prompts at the test gate. The cascade summary lands on
// stdout — proof we walked through `cascadeFromGate`, not the legacy
// dispatcher-direct path.
func TestPromptStageNextStageBangAdvancesOneStage(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!\n"); err != nil {
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
		t.Fatalf("expected `cascade: code ok` summary in stdout, got: %q", got)
	}
	if got := countInvocations(*captured, "code"); got != 1 {
		t.Fatalf("code dispatched %d times, want 1", got)
	}
	if got := countInvocations(*captured, "test"); got != 0 {
		t.Fatalf("test must not dispatch on bare `!`: got %d", got)
	}
	for _, inv := range *captured {
		if !inv.suppressNextStage {
			t.Fatalf("bare `!` dispatch must carry suppressNextStage=true, got: %+v", inv)
		}
	}
}

// TestPromptStageNextStageBangForTwin: bare `!` works for twin too —
// the cascade legend and the oneStep dispatch reach both registered
// workflows via the headless-dispatcher registry, no sdlc hard-wiring.
func TestPromptStageNextStageBangForTwin(t *testing.T) {
	captured := stubOpenTwinStage(t, nil)
	next := &Command{Name: "architecture", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe twin architecture moe reflect-2026-05-17", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "cascade: architecture ok") {
		t.Fatalf("expected `cascade: architecture ok` summary, got: %q", got)
	}
	dispatched := 0
	for _, inv := range *captured {
		if inv.stage == "architecture" {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("architecture dispatched %d times, want 1 (twin invocations: %+v)", dispatched, *captured)
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
	if !strings.Contains(stdout.String(), "!=advance one stage") {
		t.Fatalf("expected bare-! legend in stdout, got: %q", stdout.String())
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
	if strings.Contains(stdout.String(), "!=advance") || strings.Contains(stdout.String(), "!<stage>") || strings.Contains(stdout.String(), "!!=") {
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

// TestCascadeFromGateDoesNotShipOnPushDeferred pins the bug fix for
// cascade-message-was-a-lie: when push hands off to a recovery code
// session, the cascade must mark the step as deferred (not shipped),
// render "push deferred to recovery (rebase conflict) — stopped" in
// the summary, and not advance to a next stage (push is the last).
// Before the fix, push returning 0 was treated as a successful ship
// even when the 0 actually came from a clean-exit recovery session.
//
// Two flavours: rebase-conflict (built-in hook check) and hook-failure
// (project script). Both deserve the same summary shape and ship gate
// behaviour.
func TestCascadeFromGateDoesNotShipOnPushDeferred(t *testing.T) {
	cases := []struct {
		name        string
		recovery    string
		wantSummary string
	}{
		{
			name:        "rebase-conflict",
			recovery:    "rebase-conflict",
			wantSummary: "cascade: code ok · test ok · push deferred to recovery (rebase conflict) — stopped",
		},
		{
			name:        "hook-failure",
			recovery:    "hook-failure",
			wantSummary: "cascade: code ok · test ok · push deferred to recovery (pre-push hook) — stopped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openCaptured := stubOpenSdlcStage(t, nil)
			deferred := &PushDeferredError{
				Recovery: tc.recovery,
				Project:  "tele",
				Run:      "fix-it",
			}
			// Recovery exited cleanly (exit 0) — the cascade must
			// still treat this as a stop, not a ship.
			pushCaptured := stubPushFromCascade(t, 0, deferred)
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

			var stdout, stderr bytes.Buffer
			res, code := cascadeFromGate("code", "", false, md, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cascade exit=%d, want 0 (recovery exited cleanly); stderr=%q", code, stderr.String())
			}
			if res.shipped {
				t.Fatalf("res.shipped = true on deferred push; the bug this test pins is the cascade claiming a ship that never happened")
			}
			// All three stages should appear in res.ran; push must
			// be marked deferred with the recovery tag.
			wantStages := []string{"code", "test", "push"}
			if len(res.ran) != len(wantStages) {
				t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantStages), res.ran)
			}
			for i, s := range wantStages {
				if res.ran[i].stage != s {
					t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
				}
			}
			pushStep := res.ran[len(res.ran)-1]
			if pushStep.deferred != tc.recovery {
				t.Fatalf("push step deferred tag: want %q, got %q", tc.recovery, pushStep.deferred)
			}
			// Summary renders the deferred branch verbatim — the
			// design's chosen vocabulary, pinned end-to-end.
			if got := renderCascadeSummary(res); got != tc.wantSummary {
				t.Fatalf("summary = %q, want %q", got, tc.wantSummary)
			}
			// pushFromCascade was invoked exactly once (no retry).
			if len(*pushCaptured) != 1 {
				t.Fatalf("push dispatched %d times, want 1: %+v", len(*pushCaptured), *pushCaptured)
			}
			// No openSdlcStage call happened after the deferred push
			// (push is the last stage in the sdlc ladder; this guards
			// against a future ladder extension silently advancing
			// past a deferred ship).
			for _, inv := range *openCaptured {
				if inv.stage == "push" {
					t.Fatalf("openSdlcStage must not dispatch push (cascade routes push through pushFromCascade): %+v", inv)
				}
			}
		})
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
