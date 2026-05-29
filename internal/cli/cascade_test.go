package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
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
			want: "cascade moe/run: code ok · test ok",
		},
		{
			name: "failed-stopped",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 1},
			}},
			want: "cascade moe/run: code failed (exit 1) — stopped",
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
			want: "cascade moe/run: code ok · test ok · push ok — shipped",
		},
		{
			name: "yolo-aborted",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 0},
				{stage: "test", code: 2},
			}},
			want: "cascade moe/run: code ok · test failed (exit 2) — stopped",
		},
		{
			// An operator Ctrl-C reads as "interrupted", not
			// "failed (exit 130)" — distinct from a stage barf, and the
			// trailing "— stopped" still fires because the cascade halted.
			name: "interrupted-stopped",
			res: cascadeResult{ran: []cascadeStepResult{
				{stage: "code", code: 0},
				{stage: "test", code: exitInterrupted},
			}},
			want: "cascade moe/run: code ok · test interrupted — stopped",
		},
		{
			name: "empty-no-summary",
			res:  cascadeResult{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCascadeSummary("moe/run", tc.res); got != tc.want {
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
	args    []string
	options pushRunOptions
	defer_  *PushDeferredError
	exit    int
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
	pushFromCascade = func(_ string, args []string, opts pushRunOptions, _, _ io.Writer) (int, error) {
		inv := pushFromCascadeInvocation{
			args:    append([]string(nil), args...),
			options: opts,
			defer_:  deferred,
			exit:    exit,
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

// pushOutcome is one (exit, deferred) pair the sequencing stub hands
// back on a given call. A nil deferred is a happy ship / bare failure;
// a non-nil deferred is the typed deferral runPushTyped surfaces when
// the pre-push gate hands off to a recovery session.
type pushOutcome struct {
	exit     int
	deferred *PushDeferredError
}

// stubPushFromCascadeSeq swaps pushFromCascade for a recorder that
// hands back a different outcome per call — the shape the retry loop
// needs (first call defers, second call ships or re-defers). A call
// past the end of the sequence fails the test: the retry bound means
// the cascade must never call push more than len(outcomes) times, and
// an over-call is exactly the runaway-retry regression worth catching.
// Returns a pointer to the captured invocations so the test can assert
// on call count and per-call options.
func stubPushFromCascadeSeq(t *testing.T, outcomes []pushOutcome) *[]pushFromCascadeInvocation {
	t.Helper()
	var captured []pushFromCascadeInvocation
	prev := pushFromCascade
	pushFromCascade = func(_ string, args []string, opts pushRunOptions, _, _ io.Writer) (int, error) {
		if len(captured) >= len(outcomes) {
			t.Fatalf("pushFromCascade called %d times, want at most %d (runaway retry?)", len(captured)+1, len(outcomes))
		}
		out := outcomes[len(captured)]
		captured = append(captured, pushFromCascadeInvocation{
			args:    append([]string(nil), args...),
			options: opts,
			defer_:  out.deferred,
			exit:    out.exit,
		})
		if out.deferred != nil {
			return out.exit, out.deferred
		}
		return out.exit, nil
	}
	t.Cleanup(func() { pushFromCascade = prev })
	return &captured
}

// openSdlcStageInvocation records one openSdlcStage dispatch — the
// stage name, the (project, run) tuple, the headless flag (false under
// driven `!!`), and the next-stage suppression flag. Tests assert on
// these directly instead of an args slice; the rename run carved away
// the `--one-shot` prefix that used to be the assertion target.
type openSdlcStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	headless          bool
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
	openSdlcStage = func(stage, projectID, runID string, headless, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openSdlcStageInvocation{stage, projectID, runID, headless, suppressNextStage})
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
	res, code := cascadeFromGate("code", "push", false, false, md, &stdout, &stderr)
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

// TestCascadeFromGateYoloShipsAtPush pins the !!! shape: cascade
// walks every remaining stage headless and ships at push. code/test
// go through openSdlcStage (headless=true), push goes through
// pushFromCascade (the typed entry that wraps runPushTyped — merge
// path, no flags). There is no separate cascade synthesis step:
// `!!!` defaults to fast-forward merge and runPushTyped writes the
// merge-path push note.
func TestCascadeFromGateYoloShipsAtPush(t *testing.T) {
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
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
		if !inv.headless {
			t.Fatalf("!!! cascade openSdlcStage args = %+v, want headless=true", inv)
		}
	}
	// push ship is a pushFromCascade call with the bare (project, run)
	// args — merge path, no --pr flag.
	if len(*pushCaptured) != 1 {
		t.Fatalf("push ship dispatched %d times, want 1: %v", len(*pushCaptured), *pushCaptured)
	}
	if got, want := strings.Join((*pushCaptured)[0].args, " "), "tele/fix-it"; got != want {
		t.Fatalf("push ship args = %q, want %q (merge path, no flags)", got, want)
	}
	if !(*pushCaptured)[0].options.HeadlessRecovery {
		t.Fatalf("!!! push recovery option HeadlessRecovery = false, want true")
	}
	// Summary line tags the headless mode per stage.
	if got := stdout.String(); !strings.Contains(got, "cascade: code (headless)") {
		t.Fatalf("expected per-stage `(headless)` mode tag in stdout, got: %q", got)
	}
}

// TestCascadeFromGateDrivenShipsAtPush pins the `!!` (driven) shape:
// each stage opens interactively (headless=false) but the cascade
// otherwise walks the same ladder and ships at push. Mirrors the
// `!!!` shape one variant over — the only ambient difference is the
// `headless` flag threaded into each opener and the `(driven)` tag in
// the per-stage cascade announcement.
func TestCascadeFromGateDrivenShipsAtPush(t *testing.T) {
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, true, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("driven cascade must ship at push: %+v", res)
	}
	for _, inv := range *openCaptured {
		if inv.headless {
			t.Fatalf("driven cascade openSdlcStage args = %+v, want headless=false", inv)
		}
		if !inv.suppressNextStage {
			t.Fatalf("driven cascade openSdlcStage args = %+v, want suppressNextStage=true", inv)
		}
	}
	if len(*pushCaptured) != 1 {
		t.Fatalf("push ship dispatched %d times, want 1: %v", len(*pushCaptured), *pushCaptured)
	}
	if (*pushCaptured)[0].options.HeadlessRecovery {
		t.Fatalf("!! push recovery option HeadlessRecovery = true, want false")
	}
	if got := stdout.String(); !strings.Contains(got, "cascade: code (driven)") {
		t.Fatalf("expected per-stage `(driven)` mode tag in stdout, got: %q", got)
	}
}

// openTwinStageInvocation mirrors openSdlcStageInvocation for the
// twin cascade dispatcher: stage name, (project, run), headless flag,
// suppression.
type openTwinStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	headless          bool
	suppressNextStage bool
}

// stubOpenTwinStage swaps openTwinStage for a recorder so cascade tests
// can drive twin cascades without invoking real stage sessions.
// perStageExit pins a non-zero exit for a named stage when needed.
func stubOpenTwinStage(t *testing.T, perStageExit map[string]int) *[]openTwinStageInvocation {
	t.Helper()
	var captured []openTwinStageInvocation
	prev := openTwinStage
	openTwinStage = func(stage, projectID, runID string, headless, suppressNextStage bool, _ string, _, _ io.Writer) int {
		captured = append(captured, openTwinStageInvocation{stage, projectID, runID, headless, suppressNextStage})
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
	res, code := cascadeFromGate("vision", "", false, false, md, &stdout, &stderr)
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
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit moe/reflect-2026-05-17"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
	}
	// Summary ends with the close step and the shipped marker.
	wantSummary := "cascade moe/reflect-2026-05-17: vision ok · architecture ok · patterns ok · operations ok · roadmap ok · glossary ok · finalize ok · close ok — shipped"
	if got := renderCascadeSummary("moe/reflect-2026-05-17", res); got != wantSummary {
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
	res, code := cascadeFromGate("vision", "finalize", false, false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("vision", "", false, false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("code", "push", false, false, md, &stdout, &stderr)
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

// TestCascadeFromGateStopsOnInterrupt: an operator Ctrl-C during a
// cascaded stage (the stage exits exitInterrupted) stops the cascade
// like any non-zero exit, but the summary reads "interrupted", and the
// code propagates as exitInterrupted so the caller (dispatchCascade)
// can tell stop-the-chain from a stage failure. Stages downstream of
// the interrupt never dispatch.
func TestCascadeFromGateStopsOnInterrupt(t *testing.T) {
	captured := stubOpenSdlcStage(t, map[string]int{"test": exitInterrupted})
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
	if code != exitInterrupted {
		t.Fatalf("cascade exit=%d, want %d (exitInterrupted); stderr=%q", code, exitInterrupted, stderr.String())
	}
	if res.shipped {
		t.Fatalf("an interrupted cascade must not mark shipped: %+v", res)
	}
	wantRan := []cascadeStepResult{{stage: "code", code: 0}, {stage: "test", code: exitInterrupted}}
	if len(res.ran) != len(wantRan) || res.ran[1].code != exitInterrupted {
		t.Fatalf("ran = %+v, want code ok then test interrupted", res.ran)
	}
	// push never dispatched — the cascade stopped at the interrupted test.
	if got := countInvocations(*captured, "push"); got != 0 {
		t.Fatalf("push must not dispatch after an interrupt: got %d", got)
	}
	if got := renderCascadeSummary("tele/fix-it", res); got != "cascade tele/fix-it: code ok · test interrupted — stopped" {
		t.Fatalf("summary = %q, want interrupted shape", got)
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
	res, code := cascadeFromGate("code", "design", false, false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("code", "code", false, false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("code", "", true, false, md, &stdout, &stderr)
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
	res, code := cascadeFromGate("finalize", "", true, false, md, &stdout, &stderr)
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
	if got := stdout.String(); !strings.Contains(got, "cascade tele/fix-it: code ok") {
		t.Fatalf("expected `cascade tele/fix-it: code ok` summary in stdout, got: %q", got)
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
	if got := stdout.String(); !strings.Contains(got, "cascade moe/reflect-2026-05-17: architecture ok") {
		t.Fatalf("expected `cascade moe/reflect-2026-05-17: architecture ok` summary, got: %q", got)
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
	if got := stdout.String(); !strings.Contains(got, "cascade tele/fix-it: code ok") {
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

// TestPromptStageNextStageBangBangDispatchesDriven: typing `!!` at
// the design→code gate parses as the driven variant — every
// openSdlcStage dispatch in the resulting cascade sees headless=false,
// and the per-stage announcement carries the `(driven)` mode tag.
// Pins the swap: pre-swap, `!!` meant headless yolo; post-swap it
// means driven.
func TestPromptStageNextStageBangBangDispatchesDriven(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	stubPushFromCascade(t, 0, nil)
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
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
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d stderr=%q", code, stderr.String())
	}
	if len(*captured) == 0 {
		t.Fatalf("!! must dispatch at least one stage; got no openSdlcStage calls")
	}
	for _, inv := range *captured {
		if inv.headless {
			t.Fatalf("!! cascade openSdlcStage args = %+v, want headless=false (driven)", inv)
		}
	}
	if got := stdout.String(); !strings.Contains(got, "(driven)") {
		t.Fatalf("expected `(driven)` cascade mode tag in stdout, got: %q", got)
	}
}

// TestPromptStageNextStageBangBangBangDispatchesHeadless: typing
// `!!!` at the same gate parses as the headless yolo variant —
// openSdlcStage sees headless=true and the announcement reads
// `(headless)`. The sibling to TestPromptStageNextStageBangBangDispatchesDriven
// pinning the other half of the swap.
func TestPromptStageNextStageBangBangBangDispatchesHeadless(t *testing.T) {
	captured := stubOpenSdlcStage(t, nil)
	stubPushFromCascade(t, 0, nil)
	next := &Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "!!!\n"); err != nil {
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
	if len(*captured) == 0 {
		t.Fatalf("!!! must dispatch at least one stage; got no openSdlcStage calls")
	}
	for _, inv := range *captured {
		if !inv.headless {
			t.Fatalf("!!! cascade openSdlcStage args = %+v, want headless=true", inv)
		}
	}
	if got := stdout.String(); !strings.Contains(got, "(headless)") {
		t.Fatalf("expected `(headless)` cascade mode tag in stdout, got: %q", got)
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
	if !strings.Contains(stdout.String(), "! = cascade one stage") {
		t.Fatalf("expected bare-! legend in stdout, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "!<stage> = cascade to gate") {
		t.Fatalf("expected cascade legend in stdout, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "!! = driven cascade") {
		t.Fatalf("expected !! driven legend in stdout, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "!!! = headless cascade") {
		t.Fatalf("expected !!! headless legend in stdout, got: %q", stdout.String())
	}
	// The cascade-extras line is reserved for the genuinely multi-char
	// forms now; bare `!` lives in the main legend instead. Guards
	// against re-merging the old "!=advance one stage" prefix into the
	// extras line.
	if strings.Contains(stdout.String(), "cascade one stage · !<stage>") {
		t.Fatalf("bare-! must not appear on the cascade-extras line: %q", stdout.String())
	}
}

// TestPromptStageNextStageNoCascadeLegendWithoutDispatcher: a workflow
// without a registered headless dispatcher must not advertise the
// cascade syntax. After the lingering-workflows design landed,
// idea is the only such workflow today (canvas-only, no Command for
// its stage) — every other workflow registers a dispatcher, so the
// cascade legend is workflow-by-presence, not workflow-by-name.
func TestPromptStageNextStageNoCascadeLegendWithoutDispatcher(t *testing.T) {
	next := &Command{Name: dash.IdeaDocID, Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	md := &run.Metadata{ID: "lingering-workflows", Project: "moe", Workflow: dash.IdeaWorkflow, Status: run.StatusInProgress}

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
	if code := promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe idea idea moe lingering-workflows", &stdout, &stderr); code != 0 {
		t.Fatalf("prompt exit=%d", code)
	}
	if strings.Contains(stdout.String(), "! = cascade") || strings.Contains(stdout.String(), "!<stage>") || strings.Contains(stdout.String(), "!! =") {
		t.Fatalf("workflow without dispatcher must not advertise cascade legend, got: %q", stdout.String())
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
	if got, want := strings.Join(gotArgs, " "), "tele/fix-it"; got != want {
		t.Fatalf("push args = %q, want %q (merge path, no flags)", got, want)
	}
}

// TestPromptPushNextStageBangStageIsNoOp: `!<stage>` and bare `!`
// at the push gate are no-ops — every stage is at or behind. The
// push command must not dispatch; stderr surfaces a hint.
func TestPromptPushNextStageBangStageIsNoOp(t *testing.T) {
	for _, input := range []string{"!test", "!"} {
		t.Run(input, func(t *testing.T) {
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
			if _, err := io.WriteString(w, input+"\n"); err != nil {
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
				t.Fatalf("%q at push gate must not dispatch ship", input)
			}
			if !strings.Contains(stderr.String(), "at or behind the push gate") {
				t.Fatalf("expected no-op hint on stderr, got: %q", stderr.String())
			}
		})
	}
}

// TestPromptPushNextStageShowsBangBangLegend: the push prompt
// legend mentions `!!` / `!!!` for sdlc workflows.
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
	if !strings.Contains(stdout.String(), "!! / !!! = ship now") {
		t.Fatalf("expected !! / !!! legend at push gate, got: %q", stdout.String())
	}
}

// TestCascadeFromGateDrivenPushDeferredStops pins the driven (`!!`)
// deferral: recovery is interactive, so the operator picks up at the
// chain prompt — the cascade must not retry (it would race the human).
// The push step is marked deferred (not shipped), the summary reads
// "— stopped", and push dispatches exactly once.
//
// Two flavours: rebase-conflict (built-in hook check) and hook-failure
// (project script). Both deserve the same summary shape and ship gate
// behaviour.
func TestCascadeFromGateDrivenPushDeferredStops(t *testing.T) {
	cases := []struct {
		name        string
		recovery    string
		wantSummary string
	}{
		{
			name:        "rebase-conflict",
			recovery:    "rebase-conflict",
			wantSummary: "cascade tele/fix-it: code ok · test ok · push deferred to recovery (rebase conflict) — stopped",
		},
		{
			name:        "hook-failure",
			recovery:    "hook-failure",
			wantSummary: "cascade tele/fix-it: code ok · test ok · push deferred to recovery (pre-push hook) — stopped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openCaptured := stubOpenSdlcStage(t, nil)
			deferred := &PushDeferredError{Recovery: tc.recovery, Project: "tele", Run: "fix-it"}
			// Driven recovery is interactive (exit 0 from the operator
			// closing the session). The cascade must stop regardless —
			// no retry under `!!`.
			pushCaptured := stubPushFromCascadeSeq(t, []pushOutcome{{exit: 0, deferred: deferred}})
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

			var stdout, stderr bytes.Buffer
			res, code := cascadeFromGate("code", "", false, true, md, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cascade exit=%d, want 0 (recovery exited cleanly); stderr=%q", code, stderr.String())
			}
			if res.shipped {
				t.Fatalf("res.shipped = true on deferred push; a deferral is not a ship")
			}
			wantStages := []string{"code", "test", "push"}
			if len(res.ran) != len(wantStages) {
				t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantStages), res.ran)
			}
			for i, s := range wantStages {
				if res.ran[i].stage != s {
					t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
				}
			}
			if pushStep := res.ran[len(res.ran)-1]; pushStep.deferred != tc.recovery {
				t.Fatalf("push step deferred tag: want %q, got %q", tc.recovery, pushStep.deferred)
			}
			if got := renderCascadeSummary("tele/fix-it", res); got != tc.wantSummary {
				t.Fatalf("summary = %q, want %q", got, tc.wantSummary)
			}
			// Driven dispatches push exactly once — no retry.
			if len(*pushCaptured) != 1 {
				t.Fatalf("push dispatched %d times, want 1 (driven never retries): %+v", len(*pushCaptured), *pushCaptured)
			}
			// Driven recovery is interactive, not headless.
			if (*pushCaptured)[0].options.HeadlessRecovery {
				t.Fatalf("driven `!!` push recovery option HeadlessRecovery = true, want false")
			}
			for _, inv := range *openCaptured {
				if inv.stage == "push" {
					t.Fatalf("openSdlcStage must not dispatch push (cascade routes push through pushFromCascade): %+v", inv)
				}
			}
		})
	}
}

// TestCascadeFromGateHeadlessRecoveryFailedStops: under `!!!`, a
// recovery session that exits non-zero means the agent gave up. The
// cascade stops with the deferred marker — no retry — and propagates
// the recovery's exit code so the failure is visible.
func TestCascadeFromGateHeadlessRecoveryFailedStops(t *testing.T) {
	stubOpenSdlcStage(t, nil)
	deferred := &PushDeferredError{Recovery: "hook-failure", Project: "tele", Run: "fix-it"}
	// Recovery exited 1 — the agent could not fix it.
	pushCaptured := stubPushFromCascadeSeq(t, []pushOutcome{{exit: 1, deferred: deferred}})
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cascade exit=%d, want 1 (recovery gave up); stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("res.shipped = true after a failed recovery: %+v", res)
	}
	if len(*pushCaptured) != 1 {
		t.Fatalf("push dispatched %d times, want 1 (no retry after a non-zero recovery): %+v", len(*pushCaptured), *pushCaptured)
	}
	wantSummary := "cascade tele/fix-it: code ok · test ok · push deferred to recovery (pre-push hook) — stopped"
	if got := renderCascadeSummary("tele/fix-it", res); got != wantSummary {
		t.Fatalf("summary = %q, want %q", got, wantSummary)
	}
}

// TestCascadeFromGateHeadlessCleanRecoveryRetriesAndShips is the
// behaviour this run adds: under `!!!`, a recovery session that exits
// cleanly (exit 0, fix committed) earns one push retry. The retry
// re-runs the pre-push gate against the new commit; when it passes,
// push ships and the cascade rides to completion. The deferred step
// stays recorded, so the summary reads as two honest steps — the
// recover, then the ship.
func TestCascadeFromGateHeadlessCleanRecoveryRetriesAndShips(t *testing.T) {
	for _, recovery := range []string{"rebase-conflict", "hook-failure"} {
		t.Run(recovery, func(t *testing.T) {
			openCaptured := stubOpenSdlcStage(t, nil)
			deferred := &PushDeferredError{Recovery: recovery, Project: "tele", Run: "fix-it"}
			// First push defers to a clean (exit 0) recovery; the
			// retry passes the gate and ships.
			pushCaptured := stubPushFromCascadeSeq(t, []pushOutcome{
				{exit: 0, deferred: deferred},
				{exit: 0, deferred: nil},
			})
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

			var stdout, stderr bytes.Buffer
			res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cascade exit=%d, want 0 (clean recovery then ship); stderr=%q", code, stderr.String())
			}
			if !res.shipped {
				t.Fatalf("res.shipped = false after a clean recovery + retry that passed the gate: %+v", res)
			}
			// code, test, push (deferred), push (ok) — four steps.
			wantStages := []string{"code", "test", "push", "push"}
			if len(res.ran) != len(wantStages) {
				t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantStages), res.ran)
			}
			for i, s := range wantStages {
				if res.ran[i].stage != s {
					t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
				}
			}
			if got := res.ran[2].deferred; got != recovery {
				t.Fatalf("first push step deferred tag = %q, want %q", got, recovery)
			}
			if got := res.ran[3]; got.deferred != "" || got.code != 0 {
				t.Fatalf("second push step = %+v, want a clean ship (no deferred, code 0)", got)
			}
			// Push dispatched exactly twice: the deferral and the retry.
			if len(*pushCaptured) != 2 {
				t.Fatalf("push dispatched %d times, want 2 (deferral + one retry): %+v", len(*pushCaptured), *pushCaptured)
			}
			for i, inv := range *pushCaptured {
				if !inv.options.HeadlessRecovery {
					t.Fatalf("push call %d HeadlessRecovery = false, want true (`!!!`)", i)
				}
			}
			// Summary shows the recover-then-ship as two steps and
			// ends with the ship marker, not "— stopped".
			wantSummary := "cascade tele/fix-it: code ok · test ok · push deferred to recovery (" +
				deferredLabel(recovery) + ") · push ok — shipped"
			if got := renderCascadeSummary("tele/fix-it", res); got != wantSummary {
				t.Fatalf("summary = %q, want %q", got, wantSummary)
			}
			for _, inv := range *openCaptured {
				if inv.stage == "push" {
					t.Fatalf("openSdlcStage must not dispatch push: %+v", inv)
				}
			}
		})
	}
}

// TestCascadeFromGateHeadlessRetryRedefersStopsAtBound pins the retry
// bound: when the retry's pre-push gate defers a second time (the fix
// didn't stick), the cascade stops rather than opening a third
// headless turn. push dispatches exactly twice — the original and the
// one retry — and the summary records both deferred steps and reads
// "— stopped".
func TestCascadeFromGateHeadlessRetryRedefersStopsAtBound(t *testing.T) {
	stubOpenSdlcStage(t, nil)
	deferred := &PushDeferredError{Recovery: "rebase-conflict", Project: "tele", Run: "fix-it"}
	// Both the original push and the retry defer to a clean recovery —
	// the stub fails the test if the cascade calls push a third time.
	pushCaptured := stubPushFromCascadeSeq(t, []pushOutcome{
		{exit: 0, deferred: deferred},
		{exit: 0, deferred: deferred},
	})
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d, want 0 (both recoveries exited cleanly); stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("res.shipped = true after the retry re-deferred: %+v", res)
	}
	// push dispatched exactly twice — the bound holds.
	if len(*pushCaptured) != 2 {
		t.Fatalf("push dispatched %d times, want 2 (one retry, then stop): %+v", len(*pushCaptured), *pushCaptured)
	}
	wantStages := []string{"code", "test", "push", "push"}
	if len(res.ran) != len(wantStages) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantStages), res.ran)
	}
	for i, s := range wantStages {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	wantSummary := "cascade tele/fix-it: code ok · test ok · push deferred to recovery (rebase conflict) · push deferred to recovery (rebase conflict) — stopped"
	if got := renderCascadeSummary("tele/fix-it", res); got != wantSummary {
		t.Fatalf("summary = %q, want %q", got, wantSummary)
	}
}

// openKbStageInvocation, openMetaMoeStageInvocation, and
// openHooksStageInvocation mirror openSdlcStageInvocation /
// openTwinStageInvocation for the kb / meta-moe / hooks cascade
// dispatchers. Same fields — the cascade exercises identical shapes
// across workflows, so a tighter type-share would lose more in test
// readability than it'd save in lines.
type openKbStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	headless          bool
	suppressNextStage bool
}

type openMetaMoeStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	headless          bool
	suppressNextStage bool
}

type openHooksStageInvocation struct {
	stage             string
	projectID         string
	runID             string
	headless          bool
	suppressNextStage bool
}

// stubOpenKbStage / stubOpenMetaMoeStage / stubOpenHooksStage mirror
// stubOpenSdlcStage / stubOpenTwinStage: replace the workflow's
// dispatcher var with a recorder for the duration of the test.
func stubOpenKbStage(t *testing.T, perStageExit map[string]int) *[]openKbStageInvocation {
	t.Helper()
	var captured []openKbStageInvocation
	prev := openKbStage
	openKbStage = func(stage, projectID, runID string, headless, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openKbStageInvocation{stage, projectID, runID, headless, suppressNextStage})
		return perStageExit[stage]
	}
	t.Cleanup(func() { openKbStage = prev })
	return &captured
}

func stubOpenMetaMoeStage(t *testing.T, perStageExit map[string]int) *[]openMetaMoeStageInvocation {
	t.Helper()
	var captured []openMetaMoeStageInvocation
	prev := openMetaMoeStage
	openMetaMoeStage = func(stage, projectID, runID string, headless, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openMetaMoeStageInvocation{stage, projectID, runID, headless, suppressNextStage})
		return perStageExit[stage]
	}
	t.Cleanup(func() { openMetaMoeStage = prev })
	return &captured
}

func stubOpenHooksStage(t *testing.T, perStageExit map[string]int) *[]openHooksStageInvocation {
	t.Helper()
	var captured []openHooksStageInvocation
	prev := openHooksStage
	openHooksStage = func(stage, projectID, runID string, headless, suppressNextStage bool, _, _ io.Writer) int {
		captured = append(captured, openHooksStageInvocation{stage, projectID, runID, headless, suppressNextStage})
		return perStageExit[stage]
	}
	t.Cleanup(func() { openHooksStage = prev })
	return &captured
}

// TestCascadeFromGateKbYoloAutoCloses pins the kb `!!` shape: cascade
// walks the kb ladder (research → summarize) then auto-closes the
// run. Copy of TestCascadeFromGateTwinYoloAutoCloses one workflow over
// — kb is the multi-stage no-push case the lingering-workflows design
// added a dispatcher for. close lands via --no-edit so the hands-off
// cascade never blocks on an editor.
func TestCascadeFromGateKbYoloAutoCloses(t *testing.T) {
	stageCaptured := stubOpenKbStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, "kb", 0)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Workflow: "kb", Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("research", "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("kb !! cascade must ship via close: %+v", res)
	}
	wantSteps := []string{"research", "summarize", "close"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	for _, stage := range []string{"research", "summarize"} {
		got := 0
		for _, inv := range *stageCaptured {
			if inv.stage == stage {
				got++
			}
		}
		if got != 1 {
			t.Fatalf("stage %s dispatched %d times via openKbStage, want 1", stage, got)
		}
	}
	for _, inv := range *stageCaptured {
		if inv.stage == "close" {
			t.Fatalf("close must not dispatch via openKbStage: %+v", inv)
		}
	}
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit tele/dns-basics"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
	}
	wantSummary := "cascade tele/dns-basics: research ok · summarize ok · close ok — shipped"
	if got := renderCascadeSummary("tele/dns-basics", res); got != wantSummary {
		t.Fatalf("summary = %q, want %q", got, wantSummary)
	}
}

// TestCascadeFromGateMetaMoeYoloAutoCloses: meta-moe is single-stage;
// `!!` from its one gate dispatches report then auto-closes. The
// dispatcher exists for symmetry with the multi-stage workflows — the
// operator's mental model is "every workflow with a dispatcher has
// `!!`," not "only multi-stage workflows do."
func TestCascadeFromGateMetaMoeYoloAutoCloses(t *testing.T) {
	stageCaptured := stubOpenMetaMoeStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, metaMoeWorkflow, 0)
	md := &run.Metadata{ID: "meta-moe-2026-05-17", Project: "moe", Workflow: metaMoeWorkflow, Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate(metaMoeReportDoc, "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("meta-moe !! cascade must ship via close: %+v", res)
	}
	wantSteps := []string{metaMoeReportDoc, "close"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	dispatched := 0
	for _, inv := range *stageCaptured {
		if inv.stage == metaMoeReportDoc {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("%s dispatched %d times, want 1", metaMoeReportDoc, dispatched)
	}
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit moe/meta-moe-2026-05-17"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
	}
}

// TestCascadeFromGateHooksYoloAutoCloses: same shape as meta-moe one
// workflow over. hooks is also single-stage; `!!` from its one gate
// dispatches code then auto-closes.
func TestCascadeFromGateHooksYoloAutoCloses(t *testing.T) {
	stageCaptured := stubOpenHooksStage(t, nil)
	closeCaptured := stubGroupCloseCommand(t, hooksWorkflow, 0)
	md := &run.Metadata{ID: "hooks-2026-05-17", Project: "moe", Workflow: hooksWorkflow, Status: run.StatusInProgress}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate(hooksCodeDoc, "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("hooks !! cascade must ship via close: %+v", res)
	}
	wantSteps := []string{hooksCodeDoc, "close"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran %d steps, want %d (%+v)", len(res.ran), len(wantSteps), res.ran)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	dispatched := 0
	for _, inv := range *stageCaptured {
		if inv.stage == hooksCodeDoc {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("%s dispatched %d times, want 1", hooksCodeDoc, dispatched)
	}
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit moe/hooks-2026-05-17"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
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
