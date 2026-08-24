package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// closeGatedCodeCanvas is what a code stage writes when it concludes the
// run itself is moot: real reasoning, then a close nomination in the
// gate the canvas grammar already carries.
const closeGatedCodeCanvas = `# Code

## What I checked

The retry reset the design asks for already landed in foo.go:42 — the
counter is cleared on every exit path. The smallest correct diff is no
diff.

## Gate

` + "```json" + `
{"status":"close"}
` + "```" + `
`

const closeGatedReviewCanvas = `# Review

## Findings

Nothing to review — the run's premise doesn't survive the code.

## Gate

` + "```json" + `
{"status":"close"}
` + "```" + `
`

// stubSdlcStageWritingCanvas swaps openSdlcStage for a recorder that
// writes body to stage's canvas when that stage dispatches — the stand-in
// for a headless turn concluding and committing. Unlike stubOpenSdlcStage
// it leaves checkCascadeStageGate alone, so a cascade that walks past the
// close nomination would hit the real gates and be visible.
func stubSdlcStageWritingCanvas(t *testing.T, root string, md *run.Metadata, stage, body string) *[]string {
	t.Helper()
	var dispatched []string
	prev := openSdlcStage
	openSdlcStage = func(s, _, _ string, _ bool, _, _ io.Writer) int {
		dispatched = append(dispatched, s)
		if s == stage {
			writeStageCanvas(t, root, md, stage, body)
		}
		return 0
	}
	t.Cleanup(func() { openSdlcStage = prev })
	return &dispatched
}

// TestCascadeHonorsCloseNomination is the headline shape: a stage that
// nominates the run's close gets it closed, the walk stops there, and
// the cascade exits 0 — a concluded outcome, not a failure, so a ride
// that did exactly the right thing never feeds the heartbeat's failure
// backoff.
func TestCascadeHonorsCloseNomination(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	dispatched := stubSdlcStageWritingCanvas(t, root, md, "code", closeGatedCodeCanvas)
	closeCaptured := stubGroupCloseCommand(t, "sdlc", 0)
	pushCaptured := stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d, want 0; stderr=%q", code, stderr.String())
	}
	if got := strings.Join(*dispatched, ","); got != "code" {
		t.Fatalf("dispatched %q, want just code — the walk must stop at the nomination", got)
	}
	if len(*pushCaptured) != 0 {
		t.Fatalf("a closed run must not ship: %+v", *pushCaptured)
	}
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	if got, want := strings.Join((*closeCaptured)[0].args, " "), "--no-edit tele/already-done"; got != want {
		t.Fatalf("close args = %q, want %q", got, want)
	}
	wantSteps := []string{"code", "close"}
	if len(res.ran) != len(wantSteps) {
		t.Fatalf("ran = %+v, want %v", res.ran, wantSteps)
	}
	for i, s := range wantSteps {
		if res.ran[i].stage != s || res.ran[i].code != 0 {
			t.Fatalf("ran[%d] = %+v, want %s ok", i, res.ran[i], s)
		}
	}
	// shipped marks the walk terminal so dispatchCascade's tail doesn't
	// re-enter the chain prompt on a run that no longer advances.
	if !res.shipped {
		t.Fatalf("a nominated close must mark the cascade terminal: %+v", res)
	}
	if !strings.Contains(stdout.String(), "code nominated close") {
		t.Fatalf("expected the nomination named on stdout, got %q", stdout.String())
	}
}

// TestCascadeCloseNominationFailureLeavesRunOpen: close failing (lock
// contention, dirty tree) warns and propagates non-zero rather than
// swallowing it. Escalation by visibility — the run stays open for the
// operator, and there is no retry loop.
func TestCascadeCloseNominationFailureLeavesRunOpen(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	stubSdlcStageWritingCanvas(t, root, md, "code", closeGatedCodeCanvas)
	stubGroupCloseCommand(t, "sdlc", 1)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cascade exit=%d, want 1; stderr=%q", code, stderr.String())
	}
	if res.shipped {
		t.Fatalf("a failed close must not mark the cascade terminal: %+v", res)
	}
	last := res.ran[len(res.ran)-1]
	if last.stage != "close" || last.code != 1 {
		t.Fatalf("last step = %+v, want close exit 1", last)
	}
	if !strings.Contains(stderr.String(), "close failed (exit 1); tele/already-done left open") {
		t.Fatalf("expected a warn naming the run left open, got stderr=%q", stderr.String())
	}
}

// TestCascadeCloseNominationReallyCloses is the wiring the stubbed
// sibling above can't see: with the real close command and a full close
// fixture, the nomination flips the run terminal on disk.
func TestCascadeCloseNominationReallyCloses(t *testing.T) {
	root := seedCloseFixture(t, "tele", "already-done", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	md, err := run.Load(root, "tele", "already-done")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sandbox.Path(root, "tele", "already-done"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The real close refuses a dirty tree, so the canvas the stage turn
	// "wrote" has to arrive committed — exactly as a real stage commit
	// lands it before the cascade reads the gate.
	writeStageCanvas(t, root, md, "code", closeGatedCodeCanvas)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "work: code canvas nominating close")
	prev := openSdlcStage
	openSdlcStage = func(string, string, string, bool, io.Writer, io.Writer) int { return 0 }
	t.Cleanup(func() { openSdlcStage = prev })

	var stdout, stderr bytes.Buffer
	if _, code := cascadeFromGate("code", "", false, false, md, &stdout, &stderr); code != 0 {
		t.Fatalf("cascade exit=%d, want 0; stderr=%q", code, stderr.String())
	}
	reloaded, err := run.Load(root, "tele", "already-done")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != run.StatusClosed {
		t.Fatalf("run status = %q, want closed — the nominated close did not land", reloaded.Status)
	}
}

// TestPromptNextStageHonorsCloseNomination covers the interactive /
// parked tail: a stage that just committed a close nomination closes the
// run instead of offering its successor, and names reopen as the
// takeback.
func TestPromptNextStageHonorsCloseNomination(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "code", closeGatedCodeCanvas)
	closeCaptured := stubGroupCloseCommand(t, "sdlc", 0)

	var stdout, stderr bytes.Buffer
	if code := promptNextStageOverride(root, md, "code", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, stderr.String())
	}
	if len(*closeCaptured) != 1 {
		t.Fatalf("close dispatched %d times, want 1: %+v", len(*closeCaptured), *closeCaptured)
	}
	out := stdout.String()
	if !strings.Contains(out, "moe sdlc reopen tele/already-done") {
		t.Fatalf("expected reopen named as the takeback, got %q", out)
	}
	if strings.Contains(out, "next: moe sdlc test") {
		t.Fatalf("a closed run must not be offered its successor: %q", out)
	}
}

// TestPromptNextStageCloseNominationSkippedOnOverride: the push-gate
// recovery re-enters promptNextStageOverride with the stage it wants
// re-offered, not a fresh verdict to read. Same gating the blocked
// reshape uses.
func TestPromptNextStageCloseNominationSkippedOnOverride(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "code", closeGatedCodeCanvas)
	closeCaptured := stubGroupCloseCommand(t, "sdlc", 0)

	var stdout, stderr bytes.Buffer
	if code := promptNextStageOverride(root, md, "code", "push", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, stderr.String())
	}
	if len(*closeCaptured) != 0 {
		t.Fatalf("override must not read the nomination: %+v", *closeCaptured)
	}
}

// TestCloseGateDoesNotMisfireThroughExistingReaders: the close value
// composes with every existing gate reader by construction — they all
// compare against a literal "ready" or "blocked". A close gate can
// therefore neither advance a run nor trigger a kickback turn.
func TestCloseGateDoesNotMisfireThroughExistingReaders(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", closeGatedReviewCanvas)
	writeStageCanvas(t, root, md, "test", strings.Replace(closeGatedReviewCanvas, "# Review", "# Test", 1))

	var stderr bytes.Buffer
	if _, blocked := cascadeStageBlocked(md, "review", &stderr); blocked {
		t.Fatal("a close gate must not read as blocked — that would burn a kickback turn")
	}
	if ok, err := reviewStageGate(root, md); err != nil || ok {
		t.Fatalf("reviewStageGate = (%v, %v), want (false, nil) — a close gate must not advance", ok, err)
	}
	if ok, err := testStageGate(root, md); err != nil || ok {
		t.Fatalf("testStageGate = (%v, %v), want (false, nil) — a close gate must not advance", ok, err)
	}
	if testGateShipNone(root, md) {
		t.Fatal("a close gate must not read as ship:none")
	}
}

// TestStageNominatedCloseScope pins the reader's edges: sdlc only (twin's
// ladder has its own finalize seal), and every absent or unreadable
// signal reads as no nomination — this is permission to do something
// terminal.
func TestStageNominatedCloseScope(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	sdlcMD := &run.Metadata{ID: "already-done", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	twinMD := &run.Metadata{ID: "reflect-2026-05-17", Project: "moe", Workflow: "twin", Status: run.StatusInProgress}
	writeStageCanvas(t, root, sdlcMD, "code", closeGatedCodeCanvas)
	writeStageCanvas(t, root, twinMD, "vision", closeGatedCodeCanvas)
	writeStageCanvas(t, root, sdlcMD, "design", readyReviewCanvas)
	writeStageCanvas(t, root, sdlcMD, "review", "# Review\n\n## Gate\n\n```json\n{\"status\":\n```\n")

	cases := []struct {
		name  string
		md    *run.Metadata
		stage string
		want  bool
	}{
		{"sdlc close gate", sdlcMD, "code", true},
		{"twin is out of scope", twinMD, "vision", false},
		{"ready gate", sdlcMD, "design", false},
		{"unparseable gate", sdlcMD, "review", false},
		{"missing canvas", sdlcMD, "test", false},
		{"no just-finished stage", sdlcMD, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageNominatedClose(root, tc.md, tc.stage); got != tc.want {
				t.Fatalf("stageNominatedClose(%q) = %v, want %v", tc.stage, got, tc.want)
			}
		})
	}
}

// TestReopenSeedDropsCloseNomination: reopen seeds the successor's
// design canvas from the prior run's, so a *design* stage that nominated
// the close would otherwise hand its successor a canvas that closes
// itself — and the operator's takeback would be a no-op. The reasoning
// survives into the seed; the gate does not.
func TestReopenSeedDropsCloseNomination(t *testing.T) {
	nominated := `# Design

## Problem

The retry reset this run asks for already landed in foo.go:42.

## Gate

` + "```json" + `
{"status":"close"}
` + "```" + `
`
	got := stripCloseGateSection(nominated)
	if strings.Contains(got, "## Gate") || strings.Contains(got, `"close"`) {
		t.Fatalf("close gate survived the seed:\n%s", got)
	}
	if !strings.Contains(got, "already landed in foo.go:42") {
		t.Fatalf("the reasoning must survive into the seed:\n%s", got)
	}
	if _, ok := stageGateStatus(got); ok {
		t.Fatal("the stripped seed must carry no gate at all")
	}

	// A gate section followed by more prose keeps the tail.
	got = stripCloseGateSection(nominated + "\n## Out of scope\n\nNothing.\n")
	if !strings.Contains(got, "## Out of scope") {
		t.Fatalf("prose after the gate section must survive:\n%s", got)
	}
	if strings.Contains(got, "## Gate") {
		t.Fatalf("gate section not removed:\n%s", got)
	}

	// Every other canvas is returned byte-for-byte.
	for name, body := range map[string]string{
		"ready gate":  readyReviewCanvas,
		"blocked":     blockedReviewCanvas,
		"no gate":     "# Design\n\n## Problem\n\nStill open.\n",
		"unparseable": "# Design\n\n## Gate\n\n```json\n{\"status\":\n```\n",
	} {
		if out := stripCloseGateSection(body); out != body {
			t.Fatalf("%s: canvas must pass through unchanged, got:\n%s", name, out)
		}
	}
}
