package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// markRunStatus rewrites run.json's status field directly. Test helper
// for the terminal-status refusal tests, which need a "merged" or
// "closed" run without driving the full close path. Lifted from the
// deleted sdlc resume test when the cascade-mode flags subsumed that
// verb.
func markRunStatus(t *testing.T, root, projectID, runID, status string) {
	t.Helper()
	path := filepath.Join(root, "projects", projectID, "runs", runID, "run.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var md map[string]any
	if err := json.Unmarshal(b, &md); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	md["status"] = status
	out, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stageVerb names the cascade-flag-bearing verbs the table tests
// drive. name doubles as the verb's position in the sdlc stage ladder
// (the cascade's start when a mode flag is set); run is the Command.Run
// binding for the verb under test.
type stageVerb struct {
	name string
	run  func(args []string, stdout, stderr io.Writer) int
}

var sdlcStageVerbs = []stageVerb{
	{name: "design", run: runDesign},
	{name: "code", run: runCode},
	{name: "review", run: runReview},
	{name: "test", run: runTest},
}

// TestCascadeAnswerFromFlagsMapping pins the four flag-to-bang
// translations and the no-flag empty-answer path. The mutual-exclusion
// refusal (two or more flags set) is covered separately at the verb
// boundary, where the user-facing error fires.
func TestCascadeAnswerFromFlagsMapping(t *testing.T) {
	cases := []struct {
		name  string
		once  bool
		to    string
		ship  bool
		chain bool
		want  string
	}{
		{name: "no-flag", want: ""},
		{name: "once", once: true, want: "!"},
		{name: "to-test", to: "test", want: "!test"},
		{name: "to-push", to: "push", want: "!push"},
		{name: "ship", ship: true, want: "!!"},
		{name: "chain", chain: true, want: "!!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cascadeAnswerFromFlags(tc.once, tc.to, tc.ship, tc.chain)
			if !ok {
				t.Fatalf("ok=false for single-flag case %+v", tc)
			}
			if got != tc.want {
				t.Fatalf("answer = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCascadeAnswerFromFlagsMutualExclusion pins ok=false whenever
// two or more mode flags are set. Every pair plus a three-flag
// combination, since the verb prints the same error for any
// combination and the helper is the only thing that decides.
func TestCascadeAnswerFromFlagsMutualExclusion(t *testing.T) {
	cases := []struct {
		name              string
		once, ship, chain bool
		to                string
	}{
		{name: "once-and-ship", once: true, ship: true},
		{name: "once-and-chain", once: true, chain: true},
		{name: "once-and-to", once: true, to: "push"},
		{name: "ship-and-chain", ship: true, chain: true},
		{name: "ship-and-to", ship: true, to: "push"},
		{name: "chain-and-to", chain: true, to: "push"},
		{name: "three-flags", once: true, ship: true, to: "push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := cascadeAnswerFromFlags(tc.once, tc.to, tc.ship, tc.chain); ok {
				t.Fatalf("expected ok=false for combo %+v", tc)
			}
		})
	}
}

// TestSDLCStageFlagMutualExclusion: each stage verb refuses with exit
// 2 and the mutual-exclusion message when more than one cascade mode
// flag is set on the same invocation. Run setup is deliberately
// minimal — the refusal fires at parse time, before any run lookup
// or slug resolution.
func TestSDLCStageFlagMutualExclusion(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := sv.run([]string{"--once", "--chain", "tele/ghost"}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "mutually exclusive") {
				t.Fatalf("expected mutually-exclusive error, got: %q", errb.String())
			}
			if !strings.Contains(errb.String(), "sdlc "+sv.name+":") {
				t.Fatalf("expected verb prefix %q in error, got: %q", "sdlc "+sv.name, errb.String())
			}
		})
	}
}

// TestSDLCStagePersistsAgentWithCascadeFlag: combining --agent with a
// cascade mode flag flips run.json before dispatch, so every cascaded
// stage resolves the same persisted backend.
func TestSDLCStagePersistsAgentWithCascadeFlag(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedSdlcOneShotProject(t, root, "tele")
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			stubEditor(t)
			suppressNextStagePrompt(t)

			var out, errb bytes.Buffer
			slug := "cascade-agent-" + sv.name
			if code := runNew("sdlc", []string{"tele/" + slug}, &out, &errb); code != 0 {
				t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
			}

			stages := stubOpenSdlcStage(t, nil)
			stubPushFromCascade(t, 0, nil)

			out.Reset()
			errb.Reset()
			code := sv.run([]string{"--once", "--agent", "codex", "tele/" + slug}, &out, &errb)
			if code != 0 {
				t.Fatalf("exit=%d, want 0; stderr=%q stdout=%q", code, errb.String(), out.String())
			}
			if !strings.Contains(out.String(), "switched run agent to codex") {
				t.Fatalf("expected switch announcement, got stdout=%q", out.String())
			}
			md, err := run.Load(root, "tele", slug)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if md.Agent != "codex" {
				t.Fatalf("run agent = %q, want codex", md.Agent)
			}
			if len(*stages) != 1 {
				t.Fatalf("openSdlcStage invocations = %d, want 1", len(*stages))
			}
			log := gitLogFormat(t, root, 1, "HEAD", "%s%n%B")
			for _, want := range []string{
				"switch agent: tele/" + slug + " to codex",
				"MoE-Run: " + slug,
				"MoE-Project: tele",
				"MoE-Workflow: sdlc",
				"MoE-Document: " + sv.name,
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("switch-agent commit missing %q; log=%q", want, log)
				}
			}
		})
	}
}

func TestSDLCStagePersistsAgentWithoutCascadeFlag(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/single-agent"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	prev := runStageSession
	runStageSession = func(projectID, runID, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		if projectID != "tele" || runID != "single-agent" || docID != "design" {
			t.Fatalf("session target = %s/%s %s, want tele/single-agent design", projectID, runID, docID)
		}
		if opts.Agent != "codex" {
			t.Fatalf("session agent override = %q, want codex", opts.Agent)
		}
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })

	out.Reset()
	errb.Reset()
	if code := runDesign([]string{"--agent", "codex", "tele/single-agent"}, &out, &errb); code != 0 {
		t.Fatalf("runDesign exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "switched run agent to codex") {
		t.Fatalf("expected switch announcement, got stdout=%q", out.String())
	}
	md, err := run.Load(root, "tele", "single-agent")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if md.Agent != "codex" {
		t.Fatalf("run agent = %q, want codex", md.Agent)
	}
}

func TestSDLCStageAgentNoOpWhenAlreadyPersisted(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"--agent", "codex", "tele/agent-noop"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	headBefore := gitLogFormat(t, root, 1, "HEAD", "%H")

	prev := runStageSession
	runStageSession = func(_, _, _ string, _ stageSessionOpts, _, _ io.Writer) int {
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })

	out.Reset()
	errb.Reset()
	if code := runDesign([]string{"--agent", "codex", "tele/agent-noop"}, &out, &errb); code != 0 {
		t.Fatalf("runDesign exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	headAfter := gitLogFormat(t, root, 1, "HEAD", "%H")
	if headAfter != headBefore {
		t.Fatalf("HEAD changed for no-op agent switch: before=%s after=%s", headBefore, headAfter)
	}
	if strings.Contains(out.String(), "switched run agent") {
		t.Fatalf("unexpected switch announcement for no-op: stdout=%q", out.String())
	}
}

// TestSDLCStageUnknownDestinationStage: --to=<unknown> exits 2 and
// names the valid sdlc stages, so the operator's next attempt has the
// answer in stderr. The check fires up front in
// dispatchCascadeForStage, before the run-resolution step — no
// scaffolding needed.
func TestSDLCStageUnknownDestinationStage(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := sv.run([]string{"--to=nonsense", "tele/ghost"}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "--to=nonsense is not a stage of sdlc") {
				t.Fatalf("expected unknown-stage error, got: %q", errb.String())
			}
			if !strings.Contains(errb.String(), "design, code, review, test, push") {
				t.Fatalf("expected sdlc stage list in error, got: %q", errb.String())
			}
		})
	}
}

// TestSDLCStageRejectsBangDestination: --to=! and --to=!! must not slip
// past validation into a headless ship. cascadeAnswerFromFlags composes
// --to=<v> as "!"+v, so --to=! composes to the same "!!" string --ship
// produces and --to=!! to "!!!"; keying validation on the raw flag
// rejects both as unknown stages (exit 2) before any dispatch, instead
// of escalating a malformed destination into a ship.
func TestSDLCStageRejectsBangDestination(t *testing.T) {
	for _, bang := range []string{"!", "!!"} {
		for _, sv := range sdlcStageVerbs {
			t.Run(bang+"/"+sv.name, func(t *testing.T) {
				stages := stubOpenSdlcStage(t, nil)
				pushes := stubPushFromCascade(t, 0, nil)

				var out, errb bytes.Buffer
				code := sv.run([]string{"--to=" + bang, "tele/ghost"}, &out, &errb)
				if code != 2 {
					t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
				}
				if !strings.Contains(errb.String(), "--to="+bang+" is not a stage of sdlc") {
					t.Fatalf("expected unknown-stage error for --to=%s, got: %q", bang, errb.String())
				}
				if len(*stages) != 0 || len(*pushes) != 0 {
					t.Fatalf("malformed destination must not dispatch: stages=%+v pushes=%+v", *stages, *pushes)
				}
			})
		}
	}
}

// TestSDLCStageRejectsToAtOrBehindStart: --to=<this-stage> and
// --to=<earlier-stage> both exit 2 with a clear "at or behind"
// message. The chain-prompt equivalent silently no-ops; the CLI
// flag is more honest with a parse-time refusal. Pinned per verb so
// the start-stage math stays correct as the ladder evolves.
func TestSDLCStageRejectsToAtOrBehindStart(t *testing.T) {
	cases := []struct {
		verb  stageVerb
		to    string
		label string
	}{
		{verb: sdlcStageVerbs[1], to: "code", label: "code-to-code"},
		{verb: sdlcStageVerbs[1], to: "design", label: "code-to-design"},
		{verb: sdlcStageVerbs[2], to: "review", label: "review-to-review"},
		{verb: sdlcStageVerbs[2], to: "code", label: "review-to-code"},
		{verb: sdlcStageVerbs[3], to: "test", label: "test-to-test"},
		{verb: sdlcStageVerbs[3], to: "review", label: "test-to-review"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := tc.verb.run([]string{"--to=" + tc.to, "tele/ghost"}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "at or behind") {
				t.Fatalf("expected at-or-behind error, got: %q", errb.String())
			}
		})
	}
}

// TestSDLCStageRefusesTerminalRunInCascadeMode: each verb's
// cascade-mode path refuses a terminal (merged/closed/promoted) or
// pushed run at the boundary, before any session work. Mirrors the
// shape sdlc resume used to enforce — the guards moved into
// resolveAndGuardForCascade when the cascade-flag rewrite landed.
func TestSDLCStageRefusesTerminalRunInCascadeMode(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedSdlcOneShotProject(t, root, "tele")
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			stubEditor(t)
			suppressNextStagePrompt(t)

			var out, errb bytes.Buffer
			if code := runNew("sdlc", []string{"tele/cascade-terminal-" + sv.name}, &out, &errb); code != 0 {
				t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
			}
			markRunStatus(t, root, "tele", "cascade-terminal-"+sv.name, run.StatusMerged)

			out.Reset()
			errb.Reset()
			code := sv.run([]string{"--once", "tele/cascade-terminal-" + sv.name}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on terminal run; stdout=%q", out.String())
			}
			if !strings.Contains(errb.String(), "merged") {
				t.Fatalf("expected merged-status refusal, got: %q", errb.String())
			}
			if !strings.Contains(errb.String(), "nothing to cascade") {
				t.Fatalf("expected nothing-to-cascade phrasing, got: %q", errb.String())
			}
		})
	}
}

// TestSDLCStageRoutesEachCascadeMode pins, per verb, that each of the
// four cascade mode flags translates to the right cascadeFromGate
// dispatch — same dispatcher path the chain prompt's bang vocabulary
// already exercises, just entered from the CLI. Stub
// openSdlcStage / pushFromCascade so the test asserts on what the
// stages-side machinery received without standing up real sessions.
func TestSDLCStageRoutesEachCascadeMode(t *testing.T) {
	type expect struct {
		stages         []string // openSdlcStage stages dispatched, in order
		wantHeadless   bool     // headless flag every dispatched stage should carry
		wantShipPushed bool     // pushFromCascade dispatched exactly once
	}
	type modeCase struct {
		verb   stageVerb
		flags  []string
		expect expect
	}
	cases := []modeCase{
		// --once: exactly one stage at the verb's start; no push.
		{
			verb:   sdlcStageVerbs[0],
			flags:  []string{"--once"},
			expect: expect{stages: []string{"design"}, wantHeadless: true},
		},
		{
			verb:   sdlcStageVerbs[1],
			flags:  []string{"--once"},
			expect: expect{stages: []string{"code"}, wantHeadless: true},
		},
		{
			verb:   sdlcStageVerbs[2],
			flags:  []string{"--once"},
			expect: expect{stages: []string{"review"}, wantHeadless: true},
		},
		{
			verb:   sdlcStageVerbs[3],
			flags:  []string{"--once"},
			expect: expect{stages: []string{"test"}, wantHeadless: true},
		},
		// --to=<stage>: walk headless from start up to (but not
		// including) destination. Push is never dispatched on this path.
		{
			verb:   sdlcStageVerbs[0],
			flags:  []string{"--to=test"},
			expect: expect{stages: []string{"design", "code", "review"}, wantHeadless: true},
		},
		{
			verb:   sdlcStageVerbs[1],
			flags:  []string{"--to=push"},
			expect: expect{stages: []string{"code", "review", "test"}, wantHeadless: true},
		},
		// --ship: headless cascade through push, ship this run.
		{
			verb:   sdlcStageVerbs[0],
			flags:  []string{"--ship"},
			expect: expect{stages: []string{"design", "code", "review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[1],
			flags:  []string{"--ship"},
			expect: expect{stages: []string{"code", "review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[2],
			flags:  []string{"--ship"},
			expect: expect{stages: []string{"review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[3],
			flags:  []string{"--ship"},
			expect: expect{stages: []string{"test"}, wantHeadless: true, wantShipPushed: true},
		},
		// --chain: headless cascade through push, same dispatch shape as
		// --ship (the chain ride only fires if a live child exists, which
		// this fixture has none of — so the stage/push expectations match
		// --ship exactly).
		{
			verb:   sdlcStageVerbs[0],
			flags:  []string{"--chain"},
			expect: expect{stages: []string{"design", "code", "review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[1],
			flags:  []string{"--chain"},
			expect: expect{stages: []string{"code", "review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[2],
			flags:  []string{"--chain"},
			expect: expect{stages: []string{"review", "test"}, wantHeadless: true, wantShipPushed: true},
		},
		{
			verb:   sdlcStageVerbs[3],
			flags:  []string{"--chain"},
			expect: expect{stages: []string{"test"}, wantHeadless: true, wantShipPushed: true},
		},
	}
	for _, tc := range cases {
		name := tc.verb.name + "/" + strings.Join(tc.flags, "+")
		t.Run(name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			seedSdlcOneShotProject(t, root, "tele")
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			stubEditor(t)
			suppressNextStagePrompt(t)

			var out, errb bytes.Buffer
			slug := "cascade-route-" + tc.verb.name
			if code := runNew("sdlc", []string{"tele/" + slug}, &out, &errb); code != 0 {
				t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
			}

			stages := stubOpenSdlcStage(t, nil)
			pushes := stubPushFromCascade(t, 0, nil)

			args := append([]string{}, tc.flags...)
			args = append(args, "tele/"+slug)
			out.Reset()
			errb.Reset()
			if code := tc.verb.run(args, &out, &errb); code != 0 {
				t.Fatalf("verb exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
			}

			if len(*stages) != len(tc.expect.stages) {
				t.Fatalf("openSdlcStage invocations: got %d, want %d (got=%+v want=%v)",
					len(*stages), len(tc.expect.stages), *stages, tc.expect.stages)
			}
			for i, want := range tc.expect.stages {
				inv := (*stages)[i]
				if inv.stage != want {
					t.Fatalf("openSdlcStage[%d].stage = %q, want %q", i, inv.stage, want)
				}
				if inv.projectID != "tele" || inv.runID != slug {
					t.Fatalf("openSdlcStage[%d] (project, run) = (%q, %q), want (tele, %q)", i, inv.projectID, inv.runID, slug)
				}
				if inv.headless != tc.expect.wantHeadless {
					t.Fatalf("openSdlcStage[%d].headless = %v, want %v", i, inv.headless, tc.expect.wantHeadless)
				}
			}
			wantPushes := 0
			if tc.expect.wantShipPushed {
				wantPushes = 1
			}
			if len(*pushes) != wantPushes {
				t.Fatalf("pushFromCascade invocations = %d, want %d (got=%+v)", len(*pushes), wantPushes, *pushes)
			}
		})
	}
}

// TestSDLCStageNoFlagRoutesInteractive: with no cascade mode flag,
// the verb falls through to its standard interactive opener — same
// behaviour the rest of the sdlc tests cover end-to-end. Regression
// test for the runDesign/runCode/runTest restructure: no flag must
// not accidentally pick up a default cascade mode.
//
// Probe via openSdlcStage's *absence*: the interactive openers
// (openSdlcDesign/Code/Test) reach runStageSession directly, not
// through openSdlcStage. A no-flag invocation that mistakenly routes
// through dispatchCascade would surface openSdlcStage invocations
// here. The interactive opener's own session machinery is exercised
// by TestSDLCDesignWrongProjectFailsFast and friends; this test
// pins only the routing edge.
func TestSDLCStageNoFlagRoutesInteractive(t *testing.T) {
	for _, sv := range sdlcStageVerbs {
		t.Run(sv.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			stages := stubOpenSdlcStage(t, nil)
			pushes := stubPushFromCascade(t, 0, nil)

			// Wrong project so the interactive opener bails fast at
			// resolveSDLCRunSlug — we don't want to actually open a
			// session, only confirm cascade routing didn't fire.
			var out, errb bytes.Buffer
			_ = sv.run([]string{"wrongproj/ghost"}, &out, &errb)

			if len(*stages) != 0 {
				t.Fatalf("no-flag invocation reached openSdlcStage (cascade routing); got %+v", *stages)
			}
			if len(*pushes) != 0 {
				t.Fatalf("no-flag invocation reached pushFromCascade (cascade routing); got %+v", *pushes)
			}
		})
	}
}

// TestSDLCStageRejectsToWhenNothingFollows: a `--to=` value on the
// last stage that has no successors should print the "and no stage
// follows" branch. Today no sdlc stage has zero successors (push is
// the last and the test verb is the highest non-push start), so we
// pin this by invoking the test verb with `--to=test` — at-or-behind
// kicks in first and the message picks the past-stages branch. The
// no-successors branch is reachable from a future workflow extension
// and is covered as a smoke test for the verbatim error shape.
func TestSDLCStageRejectsToWhenNothingFollows(t *testing.T) {
	// Bind directly to dispatchCascadeForStage with a synthetic
	// start at the workflow's last stage (push). This lets us
	// exercise the "no past[] tail" branch without inventing a
	// stage. The workflow lookup inside dispatchCascadeForStage
	// reads the registered sdlc ladder, so "push" is the last
	// index and stages[startIdx+1:] is empty.
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	cfg := sdlcStageVerbCfg("push", nil, nil)
	code := dispatchCascadeForStage(cfg, "tele", "ghost", "!push", "push", &out, &errb)
	if code != 2 {
		t.Fatalf("exit=%d, want 2; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no stage follows push") {
		t.Fatalf("expected no-successor branch, got: %q", errb.String())
	}
}
