package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

const readyReviewCanvas = `# Review

## Gate

` + "```json" + `
{"status":"ready"}
` + "```" + `

## Findings

Fixed: the retry loop now resets the counter.

## Evidence Reviewed

diff of foo.go; ran go test ./internal/foo.
`

const blockedTestCanvas = `# Test

## Gate

` + "```json" + `
{"status":"blocked"}
` + "```" + `

## What was verified

- ran go test ./internal/cli — a regression test still fails.

## What wasn't verified

- the failing path — blocked until the fix lands.

## Fixes applied during this stage

(none)
`

const readyFilledTestCanvas = `# Test

## Gate

` + "```json" + `
{"status":"ready"}
` + "```" + `

## What was verified

- ran go test ./internal/cli — all pass.

## What wasn't verified

- Nothing — automated tests cover the change.

## Fixes applied during this stage

(none)
`

// kickbackCall records one openKickbackSession dispatch so the headless
// recovery tests can assert document, blocked stage, and the headless
// flag the cascade passes.
type kickbackCall struct {
	document     string
	blockedStage string
	headless     bool
}

// stubHeadlessKickback swaps openKickbackSession for a recorder whose
// exit code the caller pins. The recorder optionally runs onFire (e.g.
// to simulate the recovery turn rewriting a canvas) before returning.
func stubHeadlessKickback(t *testing.T, exit int, onFire func()) *[]kickbackCall {
	t.Helper()
	var calls []kickbackCall
	old := openKickbackSession
	openKickbackSession = func(_ *run.Metadata, document, blockedStage, _ string, headless bool, _, _ io.Writer) int {
		calls = append(calls, kickbackCall{document, blockedStage, headless})
		if onFire != nil {
			onFire()
		}
		return exit
	}
	t.Cleanup(func() { openKickbackSession = old })
	return &calls
}

// TestCascadeStageGateRecoversBlockedReview: a headless ship cascade that
// hits a blocked review gate makes one headless kickback to code, then
// re-dispatches review. When the recovery turn resolves the finding (the
// re-dispatch leaves a ready canvas), the gate passes and the cascade
// proceeds — recovery steps recorded as `code · review`.
func TestCascadeStageGateRecoversBlockedReview(t *testing.T) {
	for _, stage := range []string{"review", "test"} {
		t.Run(stage, func(t *testing.T) {
			root := isolateCascadeMoeHome(t)
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			ready := readyReviewCanvas
			blocked := blockedReviewCanvas
			if stage == "test" {
				ready = readyFilledTestCanvas
				blocked = blockedTestCanvas
			}
			writeStageCanvas(t, root, md, stage, blocked)

			// The re-dispatch (second openSdlcStage call for this stage)
			// stands in for the reviewer/tester re-running clean after the
			// code fix: rewrite the canvas ready.
			calls := 0
			prev := openSdlcStage
			openSdlcStage = func(s, _, _ string, headless bool, _, _ io.Writer) int {
				if s == stage {
					calls++
					if calls >= 1 {
						writeStageCanvas(t, root, md, stage, ready)
					}
				}
				return 0
			}
			t.Cleanup(func() { openSdlcStage = prev })

			kicks := stubHeadlessKickback(t, 0, nil)

			wf, err := LookupWorkflow("sdlc")
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			steps, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, stage, true, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("gate code=%d, want 0 (recovered); stderr=%q", code, stderr.String())
			}
			if len(*kicks) != 1 {
				t.Fatalf("kickbacks = %d, want 1: %+v", len(*kicks), *kicks)
			}
			if got := (*kicks)[0]; got.document != "code" || got.blockedStage != stage || !got.headless {
				t.Fatalf("kickback = %+v, want {code %s headless}", got, stage)
			}
			wantSteps := []string{"code", stage}
			if len(steps) != len(wantSteps) {
				t.Fatalf("steps = %+v, want %v", steps, wantSteps)
			}
			for i, s := range wantSteps {
				if steps[i].stage != s || steps[i].code != 0 {
					t.Fatalf("steps[%d] = %+v, want {%s 0}", i, steps[i], s)
				}
			}
		})
	}
}

// TestCascadeStageGateRecoveryExhausted: when the recovery turn lands but
// the re-dispatch still blocks, the one retry is spent and the gate parks
// exactly as before — `parked at review` on stderr, exit 1, and no second
// kickback.
func TestCascadeStageGateRecoveryExhausted(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)

	// The re-dispatch leaves the canvas blocked — the fix didn't stick.
	prev := openSdlcStage
	openSdlcStage = func(_, _, _ string, _ bool, _, _ io.Writer) int { return 0 }
	t.Cleanup(func() { openSdlcStage = prev })

	kicks := stubHeadlessKickback(t, 0, nil)

	wf, _ := LookupWorkflow("sdlc")
	var stdout, stderr bytes.Buffer
	steps, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gate code=%d, want 1 (parked); stderr=%q", code, stderr.String())
	}
	if len(*kicks) != 1 {
		t.Fatalf("kickbacks = %d, want exactly 1 (bounded): %+v", len(*kicks), *kicks)
	}
	if !strings.Contains(stderr.String(), "cascade: review gate not satisfied; parked at review") {
		t.Fatalf("expected park message, got stderr=%q", stderr.String())
	}
	wantSteps := []string{"code", "review"}
	if len(steps) != len(wantSteps) {
		t.Fatalf("steps = %+v, want %v", steps, wantSteps)
	}
}

// TestCascadeStageGateKickbackTurnFails: when the recovery code turn
// itself exits non-zero, the gate stops with that exit code and never
// re-dispatches the blocked stage.
func TestCascadeStageGateKickbackTurnFails(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)

	dispatched := 0
	prev := openSdlcStage
	openSdlcStage = func(_, _, _ string, _ bool, _, _ io.Writer) int {
		dispatched++
		return 0
	}
	t.Cleanup(func() { openSdlcStage = prev })

	stubHeadlessKickback(t, 2, nil)

	wf, _ := LookupWorkflow("sdlc")
	var stdout, stderr bytes.Buffer
	steps, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", true, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("gate code=%d, want 2 (recovery turn exit); stderr=%q", code, stderr.String())
	}
	if dispatched != 0 {
		t.Fatalf("blocked stage re-dispatched %d times after a failed recovery turn, want 0", dispatched)
	}
	if len(steps) != 1 || steps[0].stage != "code" || steps[0].code != 2 {
		t.Fatalf("steps = %+v, want [{code 2}]", steps)
	}
}

// TestCascadeStageGateNoRecoveryWhenNotYolo: the `!` / `!<stage>` forms
// (yolo=false) re-enter the interactive prompt where a human is present,
// so a blocked gate parks without an auto-kickback.
func TestCascadeStageGateNoRecoveryWhenNotYolo(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)

	kicks := stubHeadlessKickback(t, 0, nil)

	wf, _ := LookupWorkflow("sdlc")
	var stdout, stderr bytes.Buffer
	steps, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gate code=%d, want 1 (parked, no recovery); stderr=%q", code, stderr.String())
	}
	if len(*kicks) != 0 {
		t.Fatalf("non-yolo cascade must not auto-kickback: %+v", *kicks)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %+v, want none", steps)
	}
}

// TestCascadeStageGateNoRecoveryWhenReady: a satisfied gate proceeds with
// no kickback and no recovery steps — the common path is untouched.
func TestCascadeStageGateNoRecoveryWhenReady(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", readyReviewCanvas)

	kicks := stubHeadlessKickback(t, 0, nil)

	wf, _ := LookupWorkflow("sdlc")
	var stdout, stderr bytes.Buffer
	steps, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gate code=%d, want 0; stderr=%q", code, stderr.String())
	}
	if len(*kicks) != 0 || len(steps) != 0 {
		t.Fatalf("ready gate must not recover: kicks=%+v steps=%+v", *kicks, steps)
	}
}

// TestCascadeFromGateHeadlessReviewRecoversAndShips is the end-to-end
// headline: a `!!` cascade starting at a blocked review gate recovers via
// one headless code kickback and rides through test and push to a clean
// ship. The summary records the recovery as `review ok · code ok · review
// ok · test ok · push ok — shipped`.
func TestCascadeFromGateHeadlessReviewRecoversAndShips(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
	writeStageCanvas(t, root, md, "test", readyFilledTestCanvas)

	reviewCalls := 0
	prev := openSdlcStage
	openSdlcStage = func(stage, _, _ string, _ bool, _, _ io.Writer) int {
		if stage == "review" {
			reviewCalls++
			if reviewCalls >= 2 {
				writeStageCanvas(t, root, md, "review", readyReviewCanvas)
			}
		}
		return 0
	}
	t.Cleanup(func() { openSdlcStage = prev })

	kicks := stubHeadlessKickback(t, 0, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("review", "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade code=%d, want 0; stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("recovered cascade must ship: %+v", res)
	}
	if len(*kicks) != 1 || !(*kicks)[0].headless || (*kicks)[0].document != "code" {
		t.Fatalf("expected one headless code kickback, got %+v", *kicks)
	}
	if len(*pushCaptured) != 1 {
		t.Fatalf("push dispatched %d times, want 1", len(*pushCaptured))
	}
	wantSteps := []string{"review", "code", "review", "test", "push"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran = %+v, want %v", res.ran, wantSteps)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s {
			t.Fatalf("ran[%d].stage = %q, want %q", i, res.ran[i].stage, s)
		}
	}
	if summary := renderCascadeSummary("tele/fix-it", res); !strings.Contains(summary, "— shipped") {
		t.Fatalf("summary = %q, want a shipped cascade", summary)
	}
}

// TestRunKickbackSessionThreadsHeadless: runKickbackSession routes the
// headless flag through to the recovery session, and re-offers the
// blocked stage via NextStageOverride, for both modes.
func TestRunKickbackSessionThreadsHeadless(t *testing.T) {
	for _, headless := range []bool{true, false} {
		name := "interactive"
		if headless {
			name = "headless"
		}
		t.Run(name, func(t *testing.T) {
			var gotOpts stageSessionOpts
			var gotDoc string
			prev := runStageSession
			runStageSession = func(_, _, docID string, opts stageSessionOpts, _, _ io.Writer) int {
				gotDoc = docID
				gotOpts = opts
				return 0
			}
			t.Cleanup(func() { runStageSession = prev })

			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			var stdout, stderr bytes.Buffer
			if code := runKickbackSession(md, "code", "review", blockedReviewCanvas, headless, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if gotDoc != "code" {
				t.Fatalf("document = %q, want code", gotDoc)
			}
			if gotOpts.Headless != headless {
				t.Fatalf("opts.Headless = %v, want %v", gotOpts.Headless, headless)
			}
			if gotOpts.NextStageOverride != "review" {
				t.Fatalf("opts.NextStageOverride = %q, want review", gotOpts.NextStageOverride)
			}
			if !gotOpts.NeedsSandbox {
				t.Fatalf("recovery session must run with the sandbox on")
			}
			if !strings.Contains(gotOpts.InitialPrompt, "The retry loop in foo.go:42") {
				t.Fatalf("kickoff must inline the blocking canvas, got: %q", gotOpts.InitialPrompt)
			}
		})
	}
}
