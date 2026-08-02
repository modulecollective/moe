package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// seedTwinRun stamps a single run in the given workflow and status onto
// a fresh bureaucracy, with MOE_HOME pointed at it — the "operator
// re-typed a twin stage verb on a pass that already ended" fixture.
func seedTwinRun(t *testing.T, projectID, runID, workflow, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, projectID)
	trailerstest.SeedRun(t, root, projectID, runID, workflow, status)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	return root
}

// TestTwinStageRefusesTerminalRunAtInteractiveDoor is the regression
// this guard exists for. `moe twin architecture moe/<closed-pass>` used
// to open a full reflect session on a sealed pass — and a twin stage
// stages the shared wiki dir and edits the *live* twin docs, so the
// edits landed attributed to a pass that had already finalized. All six
// verbs refuse, across the whole terminal class.
func TestTwinStageRefusesTerminalRunAtInteractiveDoor(t *testing.T) {
	cases := []struct {
		stage  string
		status string
	}{
		{stage: "vision", status: run.StatusClosed},
		{stage: "architecture", status: run.StatusClosed},
		{stage: "patterns", status: run.StatusClosed},
		{stage: "operations", status: run.StatusClosed},
		{stage: "glossary", status: run.StatusClosed},
		{stage: "finalize", status: run.StatusClosed},
		{stage: "architecture", status: run.StatusMerged},
		{stage: "architecture", status: run.StatusPromoted},
	}
	for _, tc := range cases {
		t.Run(tc.stage+"-"+tc.status, func(t *testing.T) {
			seedTwinRun(t, "moe", "reflect-2026-07-30", "twin", tc.status)

			var opened int
			prev := openTwinStage
			openTwinStage = func(_, _, _ string, _ bool, _ string, _, _ io.Writer) int {
				opened++
				return 0
			}
			t.Cleanup(func() { openTwinStage = prev })

			var out, errb bytes.Buffer
			code := Run([]string{"twin", tc.stage, "moe/reflect-2026-07-30"}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s run; stdout=%q", tc.status, out.String())
			}
			if opened != 0 {
				t.Fatalf("guard must refuse before the opener runs; opened=%d", opened)
			}
			want := "twin " + tc.stage + ": moe/reflect-2026-07-30 is " + tc.status + "; a reflect pass is not reopened"
			if !strings.Contains(errb.String(), want) {
				t.Fatalf("missing refusal %q:\n%s", want, errb.String())
			}
			if !strings.Contains(errb.String(), "hint: moe twin reflect moe") {
				t.Fatalf("missing fresh-pass hint:\n%s", errb.String())
			}
		})
	}
}

// TestGuardTwinReentryPassesThrough: an in-progress pass is the common
// case and costs one run.Load. A run that doesn't load, or isn't a twin
// run, also passes through — requireTwinRun in the opener owns those two
// refusals and their wording, and duplicating them here would double the
// message.
func TestGuardTwinReentryPassesThrough(t *testing.T) {
	cases := []struct {
		name     string
		seedID   string
		seedWf   string
		seedStat string
		typed    string
	}{
		{name: "in-progress", seedID: "live", seedWf: "twin", seedStat: run.StatusInProgress, typed: "live"},
		{name: "missing-run", seedID: "live", seedWf: "twin", seedStat: run.StatusInProgress, typed: "nope"},
		{name: "wrong-workflow-closed", seedID: "notatwin", seedWf: "sdlc", seedStat: run.StatusClosed, typed: "notatwin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedTwinRun(t, "moe", tc.seedID, tc.seedWf, tc.seedStat)

			var out, errb bytes.Buffer
			resolved, code := guardTwinReentry("twin architecture", "moe", tc.typed, &out, &errb)
			if code != 0 {
				t.Fatalf("code=%d, want 0; stderr=%q", code, errb.String())
			}
			if resolved != tc.typed {
				t.Fatalf("resolved=%q, want %q", resolved, tc.typed)
			}
			if errb.Len() != 0 {
				t.Fatalf("pass-through must stay silent; stderr=%q", errb.String())
			}
		})
	}
}

// TestTwinStageCascadeLegStillRefusesTerminal: the guard is wired on the
// no-flag leg only. The cascade legs keep their own refusal
// (resolveAndGuardForCascade), with its own wording — this pins that
// adding reentryGuard didn't reroute them.
func TestTwinStageCascadeLegStillRefusesTerminal(t *testing.T) {
	seedTwinRun(t, "moe", "reflect-2026-07-30", "twin", run.StatusClosed)

	var out, errb bytes.Buffer
	code := Run([]string{"twin", "architecture", "--once", "moe/reflect-2026-07-30"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero; stdout=%q", out.String())
	}
	want := "twin architecture: moe/reflect-2026-07-30 is closed; nothing to cascade"
	if !strings.Contains(errb.String(), want) {
		t.Fatalf("missing cascade refusal %q:\n%s", want, errb.String())
	}
}
