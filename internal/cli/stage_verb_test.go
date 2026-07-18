package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestOperatorCascadesPredicate pins the rows the whole design keys on:
// a workflow participates in the operator cascade vocabulary iff it
// registers a cascade dispatcher and is neither perpetual nor
// machine-paced. Every surface (stage-verb flags, chain membership,
// serve chips) derives from this one predicate, so the rows here are
// the structural guarantee that a future workflow can't be half-wired.
func TestOperatorCascadesPredicate(t *testing.T) {
	cases := []struct {
		workflow string
		want     bool
	}{
		{"sdlc", true},
		{"twin", true},
		{"kb", true},
		{"hooks", true},
		{"chores", true},
		{"chat", false},   // perpetual — "ship" is meaningless
		{"pulse", false},  // machine-paced — moe drives it
		{"idea", false},   // no cascade dispatcher
		{"intent", false}, // no cascade dispatcher
		{"nope", false},   // not a registered workflow
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			if got := operatorCascades(tc.workflow); got != tc.want {
				t.Fatalf("operatorCascades(%q) = %v, want %v", tc.workflow, got, tc.want)
			}
		})
	}
}

// stubCascadeDispatcher swaps a workflow's registered cascade
// dispatcher for a recorder, so a cascade-flag invocation can be
// asserted on without standing up real stage sessions. perStageExit
// pins a non-zero exit for a named stage to halt the walk. Also stubs
// the stage-gate check to always pass. The returned slice records
// dispatches in order. Mirrors stubOpenSdlcStage but keys on the
// registry, so it works for any workflow.
func stubCascadeDispatcher(t *testing.T, workflow string, perStageExit map[string]int) *[]openSdlcStageInvocation {
	t.Helper()
	var captured []openSdlcStageInvocation
	prev := cascadeDispatchers[workflow]
	cascadeDispatchers[workflow] = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
		captured = append(captured, openSdlcStageInvocation{stage, projectID, runID, headless})
		return perStageExit[stage]
	}
	t.Cleanup(func() { cascadeDispatchers[workflow] = prev })
	prevGate := checkCascadeStageGate
	checkCascadeStageGate = func(_ *Workflow, _ *run.Metadata, _ string, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { checkCascadeStageGate = prevGate })
	return &captured
}

// mintTwinReflect mints an in-progress twin reflect run for project via
// the parked reflect path and returns its slug, so cascade-flag tests
// have a real twin run to drive.
func mintTwinReflect(t *testing.T, root, project string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run([]string{"twin", "reflect", "--park", project}, &out, &errb); code != 0 {
		t.Fatalf("twin reflect --park exit=%d stderr=%q", code, errb.String())
	}
	slug, err := findInProgressTwinRun(root, project)
	if err != nil || slug == "" {
		t.Fatalf("no in-progress twin run after reflect: slug=%q err=%v", slug, err)
	}
	return slug
}

// TestStageVerbCascadeRoutesTwin: `moe twin <stage> --once` walks
// exactly one stage headless through the twin dispatcher — the generic
// runStageVerb body reaching a non-sdlc workflow's registered cascade,
// proving the vocabulary follows the operatorCascades property rather
// than an sdlc hardcoding.
func TestStageVerbCascadeRoutesTwin(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	suppressNextStagePrompt(t)

	slug := mintTwinReflect(t, root, "tele")
	captured := stubCascadeDispatcher(t, "twin", nil)

	var out, errb bytes.Buffer
	if code := Run([]string{"twin", "vision", "--once", "tele/" + slug}, &out, &errb); code != 0 {
		t.Fatalf("twin vision --once exit=%d stderr=%q", code, errb.String())
	}
	if len(*captured) != 1 {
		t.Fatalf("twin dispatcher invocations = %d, want 1 (%+v)", len(*captured), *captured)
	}
	inv := (*captured)[0]
	if inv.stage != "vision" || inv.projectID != "tele" || inv.runID != slug || !inv.headless {
		t.Fatalf("dispatch = %+v, want {vision tele %s headless}", inv, slug)
	}
}

// TestStageVerbMutualExclusionAcrossWorkflows: every adopting
// workflow's stage verbs refuse two cascade flags with exit 2 and the
// shared message — the generic runStageVerb body, not per-verb code.
func TestStageVerbMutualExclusionAcrossWorkflows(t *testing.T) {
	verbs := [][]string{
		{"twin", "vision"},
		{"kb", "research"},
		{"hooks", "code"},
		{"chores", "code"},
	}
	for _, v := range verbs {
		t.Run(v[0]+"/"+v[1], func(t *testing.T) {
			var out, errb bytes.Buffer
			args := append(append([]string{}, v...), "--once", "--chain", "tele/ghost")
			code := Run(args, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
			}
			if !bytes.Contains(errb.Bytes(), []byte("mutually exclusive")) {
				t.Fatalf("expected mutually-exclusive error, got: %q", errb.String())
			}
		})
	}
}

// TestStageVerbToNamesWorkflowLadder: a bad --to destination names the
// verb's own workflow ladder, not sdlc's. The multi-stage case lists
// the twin stages; the single-stage case (hooks) reports "no stage
// follows code" since nothing succeeds the only stage. Both fire before
// any run lookup, so no fixture is needed.
func TestStageVerbToNamesWorkflowLadder(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"twin", "vision", "--to=nonsense", "tele/ghost"}, &out, &errb); code != 2 {
		t.Fatalf("twin --to=nonsense exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("is not a stage of twin")) {
		t.Fatalf("expected twin ladder in error, got: %q", errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("vision, architecture, patterns, operations, glossary, finalize")) {
		t.Fatalf("expected twin stage list, got: %q", errb.String())
	}

	errb.Reset()
	if code := Run([]string{"hooks", "code", "--to=code", "tele/ghost"}, &out, &errb); code != 2 {
		t.Fatalf("hooks --to=code exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("no stage follows code")) {
		t.Fatalf("expected no-successor branch for single-stage hooks, got: %q", errb.String())
	}
}

// TestStageVerbWrongWorkflowRefused: driving a twin stage verb against
// an sdlc run refuses at the cascade preflight, naming both workflows —
// the generic wrong-workflow guard in resolveAndGuardForCascade.
func TestStageVerbWrongWorkflowRefused(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/wrong-wf"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"twin", "vision", "--once", "tele/wrong-wf"}, &out, &errb); code == 0 {
		t.Fatalf("expected refusal driving a twin verb on an sdlc run; stdout=%q", out.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("is a sdlc run, not twin")) {
		t.Fatalf("expected wrong-workflow refusal, got: %q", errb.String())
	}
}

// TestChatNewShipRefusesPerpetual pins the one behavior change the
// design flagged: `moe chat new --ship` flips from "run then auto-close
// a perpetual chat" to a parse-time refusal, because operatorCascades
// excludes perpetual workflows. No fixture — the refusal fires before
// any disk work.
func TestChatNewShipRefusesPerpetual(t *testing.T) {
	var out, errb bytes.Buffer
	code := runNew("chat", []string{"--ship", "tele/nope"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("perpetual")) {
		t.Fatalf("expected perpetual refusal, got: %q", errb.String())
	}
}

// TestTwinReflectShipCascades: `moe twin reflect --ship` mints the pass
// and hands off to a headless cascade from the first stage (vision) —
// the shared mint tail's `!!` dispatch, now wired into reflect. The
// dispatcher is stubbed to halt at vision so the assertion is the
// handoff itself, not the full six-stage walk.
func TestTwinReflectShipCascades(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	captured := stubCascadeDispatcher(t, "twin", map[string]int{"vision": 1})

	var out, errb bytes.Buffer
	code := Run([]string{"twin", "reflect", "--ship", "tele"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (halted at stubbed vision); stderr=%q", code, errb.String())
	}
	if len(*captured) != 1 || (*captured)[0].stage != "vision" || !(*captured)[0].headless {
		t.Fatalf("reflect --ship dispatches = %+v, want one headless vision", *captured)
	}
}

// TestTwinReflectShipParkMutuallyExclusive: --ship and --park are
// opposite tails on reflect too, refused before the mint.
func TestTwinReflectShipParkMutuallyExclusive(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"twin", "reflect", "--ship", "--park", "tele"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("opposite tails")) {
		t.Fatalf("expected opposite-tails error, got: %q", errb.String())
	}
}

// TestChoreOpenShipCascades: `moe chore open --ship` opens the chore's
// run and hands off to the headless cascade from the first stage — the
// same shared mint tail, now on chore open. The chore defaults to the
// sdlc workflow, so the cascade starts at design; the dispatcher is
// stubbed to halt there.
func TestChoreOpenShipCascades(t *testing.T) {
	seedChoreRoot(t)
	captured := stubCascadeDispatcher(t, "sdlc", map[string]int{"design": 1})

	var out, errb bytes.Buffer
	code := runChoreOpen([]string{"--ship", "moe/readme-refresh"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (halted at stubbed design); stderr=%q", code, errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("opened chore moe/readme-refresh")) {
		t.Fatalf("missing open confirmation: %q", out.String())
	}
	if len(*captured) != 1 || (*captured)[0].stage != "design" || !(*captured)[0].headless {
		t.Fatalf("chore open --ship dispatches = %+v, want one headless design", *captured)
	}
}

// TestChoreOpenShipParkMutuallyExclusive: opposite tails on chore open,
// refused at parse time before the open.
func TestChoreOpenShipParkMutuallyExclusive(t *testing.T) {
	seedChoreRoot(t)
	var out, errb bytes.Buffer
	code := runChoreOpen([]string{"--ship", "--park", "moe/readme-refresh"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("opposite tails")) {
		t.Fatalf("expected opposite-tails error, got: %q", errb.String())
	}
}
