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
