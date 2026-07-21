package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// seedChoreRoot builds a minimal bureaucracy with one project and one
// trigger-only chore, then makes the chore due by stamping a
// MoE-Chore-Touched commit. MOE_HOME points the command's root
// discovery at it.
func seedChoreRoot(t *testing.T) string {
	t.Helper()
	return seedChoreRootWith(t, "")
}

// seedChoreRootWith is seedChoreRoot with an optional cooldown. When
// cooldown is non-empty the chore.json carries that cooldown and a recent
// MoE-Chore-Skipped completion is stamped after the touch, so it lands
// cooling-down and not-due — the state `--now` exists to override.
func seedChoreRootWith(t *testing.T, cooldown string) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bureaucracy.conf", "")
	write("projects/moe/project.json", `{"id":"moe"}`)
	if cooldown != "" {
		write("projects/moe/chores/readme-refresh/chore.json",
			`{"trigger":"README.md","cooldown":"`+cooldown+`"}`)
	} else {
		write("projects/moe/chores/readme-refresh/chore.json", `{"trigger":"README.md"}`)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed bureaucracy")
	// Make the chore due: a changed-path touch with no completion yet.
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"work: touch\n\nMoE-Chore-Touched: moe/readme-refresh\n")
	if cooldown != "" {
		// A recent completion after the touch: cooldown blocks, and the
		// completion clears the changed-path due reason, so the chore is
		// both cooling-down and not-due.
		gittest.Run(t, root, "commit", "--allow-empty", "-m",
			"chore skip\n\nMoE-Chore-Skipped: moe/readme-refresh\n")
	}
	t.Setenv("MOE_HOME", root)
	return root
}

func choreDue(t *testing.T, root, name string) bool {
	t.Helper()
	states, err := gatherChoreStates(root, "moe")
	if err != nil {
		t.Fatalf("gatherChoreStates: %v", err)
	}
	for _, s := range states {
		if s.Definition.Name == name {
			return s.Due
		}
	}
	t.Fatalf("chore %q not found in states", name)
	return false
}

func TestRunChoreSkipClearsDueChore(t *testing.T) {
	root := seedChoreRoot(t)
	if !choreDue(t, root, "readme-refresh") {
		t.Fatalf("precondition: chore should be due before skip")
	}

	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"moe/readme-refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runChoreSkip = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("skipped chore moe/readme-refresh")) {
		t.Errorf("stdout missing confirmation: %q", stdout.String())
	}

	// The skip commit's MoE-Chore-Skipped trailer must round-trip
	// through the index and Evaluate to drop the row.
	if choreDue(t, root, "readme-refresh") {
		t.Errorf("chore still due after skip")
	}
	// The marker is an empty commit carrying the trailer.
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !bytes.Contains([]byte(body), []byte("MoE-Chore-Skipped: moe/readme-refresh")) {
		t.Errorf("HEAD commit missing skip trailer:\n%s", body)
	}
}

func TestRunChoreSkipUnknownChore(t *testing.T) {
	seedChoreRoot(t)
	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"moe/nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("runChoreSkip = %d, want 1", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("not found")) {
		t.Errorf("stderr should report not found: %q", stderr.String())
	}
}

func TestRunChoreSkipBadArg(t *testing.T) {
	seedChoreRoot(t)
	var stdout, stderr bytes.Buffer
	if code := runChoreSkip([]string{"no-slash"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runChoreSkip = %d, want 2 for malformed arg", code)
	}
}

// choreOpenRun returns the chore's currently-open linked run, "" if none.
// A non-empty result proves a run was opened with the MoE-Chore trailer:
// OpenRun is only set when the journal index links a non-terminal run
// back to the chore key.
func choreOpenRun(t *testing.T, root, name string) string {
	t.Helper()
	states, err := gatherChoreStates(root, "moe")
	if err != nil {
		t.Fatalf("gatherChoreStates: %v", err)
	}
	for _, s := range states {
		if s.Definition.Name == name {
			return s.OpenRun
		}
	}
	t.Fatalf("chore %q not found in states", name)
	return ""
}

// A cooling-down chore refuses a normal open but opens under --now, and
// the opened run is linked back to the chore (MoE-Chore trailer stamped).
func TestOpenChoreNowOpensCoolingChore(t *testing.T) {
	root := seedChoreRootWith(t, "720h")
	if choreDue(t, root, "readme-refresh") {
		t.Fatalf("precondition: cooling chore should not be due")
	}

	// Without --now: refused as cooling down, nothing opened.
	var stdout, stderr bytes.Buffer
	if _, code := openDueChore(root, "moe", "readme-refresh", false, &stdout, &stderr); code != 1 {
		t.Fatalf("openDueChore(force=false) = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("cooling down")) {
		t.Errorf("stderr should report cooldown refusal: %q", stderr.String())
	}
	if open := choreOpenRun(t, root, "readme-refresh"); open != "" {
		t.Fatalf("refused open should leave no open run, got %q", open)
	}

	// With --now: opens, and the run is linked to the chore.
	stdout.Reset()
	stderr.Reset()
	md, code := openDueChore(root, "moe", "readme-refresh", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("openDueChore(force=true) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if open := choreOpenRun(t, root, "readme-refresh"); open != md.ID {
		t.Errorf("chore open run = %q, want %q (MoE-Chore trailer not linked)", open, md.ID)
	}
}

// --now never bypasses the open-run guard: a chore that already has an
// open linked run still refuses, force or not. Open one via the normal
// due path, then attempt a forced second open.
func TestOpenChoreNowStillRefusesWhenRunAlreadyOpen(t *testing.T) {
	root := seedChoreRoot(t)

	var stdout, stderr bytes.Buffer
	if _, code := openDueChore(root, "moe", "readme-refresh", false, &stdout, &stderr); code != 0 {
		t.Fatalf("first open = %d, want 0; stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if _, code := openDueChore(root, "moe", "readme-refresh", true, &stdout, &stderr); code != 1 {
		t.Fatalf("forced second open = %d, want 1 (open-run guard must hold)", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("already has open run")) {
		t.Errorf("stderr should report the open-run guard: %q", stderr.String())
	}
}

// The --now flag parses and threads through runChoreOpen to the open
// pipeline, opening a cooling chore the bare verb would refuse.
func TestRunChoreOpenNowFlagOpensCoolingChore(t *testing.T) {
	root := seedChoreRootWith(t, "720h")

	var stdout, stderr bytes.Buffer
	if code := runChoreOpen([]string{"--now", "moe/readme-refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runChoreOpen --now = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("opened chore moe/readme-refresh")) {
		t.Errorf("stdout missing open confirmation: %q", stdout.String())
	}
	if open := choreOpenRun(t, root, "readme-refresh"); open == "" {
		t.Errorf("chore should have an open run after --now")
	}
}

// --park opens the chore run, prints the next-stage hint, and stops
// without the chain prompt. Chores default to the sdlc workflow, so the
// hint points at design. Mirrors TestRunNewParkPrintsHintAndExits.
func TestRunChoreOpenParkPrintsHintWithoutPrompt(t *testing.T) {
	seedChoreRoot(t)

	var stdout, stderr bytes.Buffer
	if code := runChoreOpen([]string{"--park", "moe/readme-refresh"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runChoreOpen --park = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("opened chore moe/readme-refresh")) {
		t.Errorf("stdout missing open confirmation: %q", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("next: moe sdlc design moe/readme-refresh")) {
		t.Errorf("stdout missing next-stage hint: %q", stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("run now?")) {
		t.Errorf("--park must not print the chain prompt: %q", stdout.String())
	}
}

// The consent ladder on `chore open` is the same ladder new and the
// stage verbs carry: --ship / --chain / --dynamic map to `!!` / `!!!` /
// `!!!!`. Chores open an sdlc run, so the cascade drives openSdlcStage +
// push; the assertion reads currentRideMode from inside the cascade, the
// seam that proves the flag's consent reached the ride.
func TestRunChoreOpenCascadeLadderCarriesConsent(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want rideMode
	}{
		{flag: "--ship", want: rideNone},
		{flag: "--chain", want: rideStatic},
		{flag: "--dynamic", want: rideDynamic},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			seedChoreRoot(t)

			var modes []rideMode
			prev := openSdlcStage
			openSdlcStage = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
				modes = append(modes, currentRideMode)
				return 0
			}
			t.Cleanup(func() { openSdlcStage = prev })
			prevGate := checkCascadeStageGate
			checkCascadeStageGate = func(_ *Workflow, _ *run.Metadata, _ string, _ io.Writer) (bool, int) {
				return true, 0
			}
			t.Cleanup(func() { checkCascadeStageGate = prevGate })
			stubPushFromCascade(t, 0, nil)

			var stdout, stderr bytes.Buffer
			if code := runChoreOpen([]string{tc.flag, "moe/readme-refresh"}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			if len(modes) == 0 {
				t.Fatalf("%s cascaded no stages", tc.flag)
			}
			for i, got := range modes {
				if got != tc.want {
					t.Fatalf("stage %d ran under ride mode %v, want %v", i, got, tc.want)
				}
			}
			if strings.Contains(stdout.String(), "run now?") {
				t.Errorf("%s must not print the chain prompt: %q", tc.flag, stdout.String())
			}
		})
	}
}

// The ladder is a ladder, not a set of composable modifiers — two rungs
// at once is a usage error, refused before any open.
func TestRunChoreOpenLadderRungsMutuallyExclusive(t *testing.T) {
	seedChoreRoot(t)

	var stdout, stderr bytes.Buffer
	code := runChoreOpen([]string{"--chain", "--dynamic", "moe/readme-refresh"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected usage exit (2), got %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "one ladder") {
		t.Errorf("expected the ladder error, got: %q", stderr.String())
	}
}

// --park is the opposite tail to every rung, not just --ship, and the
// message names the rung the operator actually typed.
func TestRunChoreOpenParkExcludesEveryCascadeRung(t *testing.T) {
	for _, flag := range []string{"--ship", "--chain", "--dynamic"} {
		t.Run(flag, func(t *testing.T) {
			seedChoreRoot(t)

			var stdout, stderr bytes.Buffer
			code := runChoreOpen([]string{flag, "--park", "moe/readme-refresh"}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("expected usage exit (2), got %d stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "opposite tails") || !strings.Contains(stderr.String(), flag) {
				t.Errorf("expected an opposite-tails error naming %s, got: %q", flag, stderr.String())
			}
		})
	}
}

// The pulse's chore step is the "automation acts on standing intent"
// half of a sweep: it opens every due chore's run and executes none.
// These ride seedChoreRoot and the real openChoreInProcess — the same
// pipeline `moe chore open` drives.

// addCoolingChore writes a second chore into a seeded chore root and
// stamps a recent completion, so its cooldown blocks and it evaluates
// not-due — the same shape seedChoreRootWith gives the first chore.
// Committed because the run-open below refuses a dirty tree.
func addCoolingChore(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "projects", "moe", "chores", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chore.json"), []byte(`{"trigger":"docs/**","cooldown":"720h"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed a second, cooling chore")
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chore skip\n\nMoE-Chore-Skipped: moe/"+name+"\n")
}

// TestAutoOpenDueChoresOpensOnlyDueOnes: the step's whole job. A due
// chore gets its run; a not-due sibling does not — the sweep never
// force-opens, which is what keeps `--now` an operator gesture.
func TestAutoOpenDueChoresOpensOnlyDueOnes(t *testing.T) {
	root := seedChoreRoot(t)
	addCoolingChore(t, root, "docs-sweep")
	if choreDue(t, root, "docs-sweep") {
		t.Fatal("precondition: the cooling chore should not be due")
	}

	var stdout, stderr bytes.Buffer
	autoOpenDueChores(root, "moe", nil /*pi*/, &stdout, &stderr)

	if open := choreOpenRun(t, root, "readme-refresh"); open == "" {
		t.Error("the due chore should have an open run after the sweep's chore step")
	}
	if open := choreOpenRun(t, root, "docs-sweep"); open != "" {
		t.Errorf("not-due chore opened run %q; automation acts on standing intent, not on every chore", open)
	}
}

// TestAutoOpenDueChoresSkipsAlreadyOpenSilently: the anti-pile-up
// invariant. A chore already holding a run must be a silent skip on
// every subsequent sweep — no second run, no warn line. Two guards sit
// in front of it (an open run clears every due reason, so the not-due
// continue fires first; openChoreInProcess's own open-run refusal is the
// belt behind it), and the assertion is the outcome across both.
func TestAutoOpenDueChoresSkipsAlreadyOpenSilently(t *testing.T) {
	root := seedChoreRoot(t)
	if _, err := openChoreInProcess(root, "moe", "readme-refresh", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("pre-open: %v", err)
	}
	first := choreOpenRun(t, root, "readme-refresh")
	if first == "" {
		t.Fatal("pre-open produced no run")
	}

	var stdout, stderr bytes.Buffer
	autoOpenDueChores(root, "moe", nil /*pi*/, &stdout, &stderr)

	if got := choreOpenRun(t, root, "readme-refresh"); got != first {
		t.Errorf("open run = %q, want the pre-existing %q — the sweep must not pile a second run on", got, first)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want silence: an already-open chore is expected, not a warning", stderr.String())
	}
}

// TestAutoOpenDueChoresSkipsWhenInterrupted: the step's Ctrl-C
// checkpoint sits inside the loop, so a latch set before the sweep opens
// nothing at all.
func TestAutoOpenDueChoresSkipsWhenInterrupted(t *testing.T) {
	root := seedChoreRoot(t)

	var stdout, stderr bytes.Buffer
	autoOpenDueChores(root, "moe", latchedPulseInterrupt(t), &stdout, &stderr)

	if open := choreOpenRun(t, root, "readme-refresh"); open != "" {
		t.Errorf("chore opened run %q despite the latch", open)
	}
}

// TestAutoOpenDueChoresWarnsOnUnreadableStates: a chore pile-up must
// never derail the sweep or the verb that triggered it. An unparseable
// chore definition warns and returns rather than propagating.
func TestAutoOpenDueChoresWarnsOnUnreadableStates(t *testing.T) {
	root := seedChoreRoot(t)
	writeFile(t, filepath.Join(root, "projects", "moe", "chores", "readme-refresh", "chore.json"), "{not json\n")

	var stdout, stderr bytes.Buffer
	autoOpenDueChores(root, "moe", nil /*pi*/, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "pulse: read chore states for moe:") {
		t.Fatalf("stderr = %q, want the chore-state read failure warned and named", stderr.String())
	}
}

// TestPulseNewOpensDueChoresBeforeTheSurvey is the wiring check on
// runPulse itself: every other `pulse new` test stubs above the chore
// step, so nothing today would fail if the sweep stopped calling it. The
// assertion reads the chore's run from inside the survey stub — the
// ordering ("open every due chore's run, then sweep") is what lets the
// survey see the runs it just caused.
func TestPulseNewOpensDueChoresBeforeTheSurvey(t *testing.T) {
	seedChoreRoot(t) // sets MOE_HOME; the verb finds the root itself
	t.Setenv("NO_COLOR", "1")

	openAtSurvey := ""
	orig := runPulseSurvey
	runPulseSurvey = func(root, projectID, spawner string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		openAtSurvey = choreOpenRun(t, root, "readme-refresh")
		return 0
	}
	t.Cleanup(func() { runPulseSurvey = orig })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pulse", "new", "moe"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if openAtSurvey == "" {
		t.Fatal("the due chore's run was not open by the time the survey ran; runPulse skipped its chore step")
	}
}
