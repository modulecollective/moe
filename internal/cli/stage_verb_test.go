package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestOperatorCascadesPredicate pins the rows the whole design keys on:
// a workflow participates in the operator cascade vocabulary iff it
// registers a cascade dispatcher and is neither perpetual nor
// machine-paced. Both surfaces (stage-verb flags, chain membership)
// derive from this one predicate, so the rows here are the structural
// guarantee that a future workflow can't be half-wired.
func TestOperatorCascadesPredicate(t *testing.T) {
	cases := []struct {
		workflow string
		want     bool
	}{
		{"sdlc", true},
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

// TestStageVerbMutualExclusionAcrossWorkflows: every adopting
// workflow's stage verbs refuse two cascade flags with exit 2 and the
// shared message — the generic runStageVerb body, not per-verb code.
func TestStageVerbMutualExclusionAcrossWorkflows(t *testing.T) {
	verbs := [][]string{
		{"sdlc", "design"},
		{"sdlc", "code"},
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
// verb's own workflow ladder, and a --to at or behind the verb's own stage
// names the stages that *would* work. Both fire before any run lookup,
// so no fixture is needed.
// Both fire before any run lookup, so no fixture is needed.
func TestStageVerbToNamesWorkflowLadder(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "design", "--to=nonsense", "tele/ghost"}, &out, &errb); code != 2 {
		t.Fatalf("sdlc --to=nonsense exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("is not a stage of sdlc")) {
		t.Fatalf("expected sdlc ladder in error, got: %q", errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("design, code, test, review, push")) {
		t.Fatalf("expected sdlc stage list, got: %q", errb.String())
	}

	errb.Reset()
	if code := Run([]string{"sdlc", "test", "--to=design", "tele/ghost"}, &out, &errb); code != 2 {
		t.Fatalf("sdlc --to=design exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("is at or behind test — pick a stage past test (try: review, push)")) {
		t.Fatalf("expected the at-or-behind branch naming the remaining stages, got: %q", errb.String())
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
