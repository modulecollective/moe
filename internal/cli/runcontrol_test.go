package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
)

// controlFixture is a real bureaucracy holding one in-progress sdlc
// run, with cwd inside it so the cascade seams find the root.
func controlFixture(t *testing.T) (string, *run.Metadata) {
	t.Helper()
	root := spawnFixture(t)
	t.Chdir(root)
	return root, seedParkedRun(t, root, "change-auth")
}

func controlRecord(t *testing.T, root string) runcontrol.File {
	t.Helper()
	f, err := runcontrol.Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func submitControl(t *testing.T, root string, kind runcontrol.Kind, id string) {
	t.Helper()
	req := runcontrol.Request{ID: id, Kind: kind}
	if kind == runcontrol.KindPark {
		req.Owner = ownerGeneration(root, "moe", "change-auth") // a park addresses the live owner
	}
	if _, err := runcontrol.Submit(root, "moe", "change-auth", req); err != nil {
		t.Fatalf("submit %s: %v", kind, err)
	}
}

// ownForTest takes this invocation's guard on the run, as a stage would.
func ownForTest(t *testing.T, root string) {
	t.Helper()
	rel, err := ownRun(root, "moe", "change-auth", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rel() })
}

// A hold that lands while a stage runs stops the walk at the next
// boundary: the stage in flight settles, nothing after it dispatches,
// the receipt names the boundary, and the summary says why.
func TestHoldStopsCascadeAtNextBoundary(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	captured := stubOpenSdlcStage(t, nil)
	push := stubPushFromCascade(t, 0, nil)
	inner := openSdlcStage
	openSdlcStage = func(stage, p, r string, headless bool, o, e io.Writer) int {
		if stage == "build" {
			submitControl(t, root, runcontrol.KindPause, "p1")
		}
		return inner(stage, p, r, headless, o, e)
	}

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("build", "", false, true, md, &stdout, &stderr)
	if code != exitYielded {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, exitYielded, stderr.String())
	}
	if n := countInvocations(*captured, "build"); n != 1 {
		t.Fatalf("build dispatched %d times, want 1", n)
	}
	for _, s := range []string{"review"} {
		if n := countInvocations(*captured, s); n != 0 {
			t.Fatalf("%s dispatched after the hold", s)
		}
	}
	if len(*push) != 0 {
		t.Fatal("pushed after the hold")
	}
	p1, _ := controlRecord(t, root).Find("p1")
	if p1.State != runcontrol.StateCompleted || p1.Boundary != "stage review" {
		t.Fatalf("receipt = %+v, want completed at stage review", p1)
	}
	if got := renderCascadeSummary("moe/change-auth", res); !strings.Contains(got, "review held by operator control — stopped") {
		t.Fatalf("summary = %q", got)
	}
}

// A finish accepted during a `!!!` ships this run through the ordinary
// route and withholds the chain ride; the ship that had begun is
// reported as done.
func TestFinishWithholdsChainRideAfterShip(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	stubOpenSdlcStage(t, nil)
	push := stubPushFromCascade(t, 0, nil)
	submitControl(t, root, runcontrol.KindFinish, "f1")

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("review", "", false, true, md, &stdout, &stderr)
	if code != 0 || !res.shipped || len(*push) != 1 {
		t.Fatalf("exit %d shipped %v pushes %d; stderr:\n%s", code, res.shipped, len(*push), stderr.String())
	}
	if !strings.Contains(stdout.String(), "chain continuation withheld") {
		t.Fatalf("no withheld-ride line:\n%s", stdout.String())
	}
	f1, _ := controlRecord(t, root).Find("f1")
	if f1.State != runcontrol.StateCompleted || !strings.Contains(f1.Outcome, "chain continuation withheld") {
		t.Fatalf("finish = %+v", f1)
	}
}

// A machine kick of a held run starts nothing, and the planner's floor
// says why; a yield costs the sweep nothing.
func TestHeldRunRefusesKick(t *testing.T) {
	root, md := controlFixture(t)
	captured := stubOpenSdlcStage(t, nil)
	submitControl(t, root, runcontrol.KindHandoff, "h1")

	var stdout, stderr bytes.Buffer
	if code := chainKickRun(root, "moe", "change-auth", rideDynamic, false, &stdout, &stderr); code != exitYielded {
		t.Fatalf("kick exit = %d; stderr:\n%s", code, stderr.String())
	}
	if len(*captured) != 0 {
		t.Fatalf("held kick dispatched %+v", *captured)
	}
	if phrase := controlHoldPhrase(root, md); !strings.Contains(phrase, "held by the operator (handoff)") {
		t.Fatalf("floor phrase = %q", phrase)
	}
	if combinePulseExit(0, exitYielded) != 0 || combinePulseExit(1, exitYielded) != 1 {
		t.Fatal("a yield must neither fail the sweep nor mask a failure")
	}
	if !controlRecord(t, root).Held() {
		t.Fatal("a machine entry cleared the hold")
	}
}

// A finish turns a kick — even a one-stage one — into this run's `!!`:
// it walks to the ship and rides nothing, recorded applied then done.
func TestFinishWidensKickToBangBang(t *testing.T) {
	root, _ := controlFixture(t)
	captured := stubOpenSdlcStage(t, nil)
	push := stubPushFromCascade(t, 0, nil)
	submitControl(t, root, runcontrol.KindFinish, "f1")

	var stdout, stderr bytes.Buffer
	if code := chainKickRun(root, "moe", "change-auth", rideDynamic, true, &stdout, &stderr); code != 0 {
		t.Fatalf("kick exit = %d; stderr:\n%s", code, stderr.String())
	}
	for _, s := range []string{"design", "build", "review"} {
		if countInvocations(*captured, s) != 1 {
			t.Fatalf("%s dispatched %d times, want 1 (%+v)", s, countInvocations(*captured, s), *captured)
		}
	}
	if len(*push) != 1 {
		t.Fatalf("pushes = %d, want 1", len(*push))
	}
	f1, _ := controlRecord(t, root).Find("f1")
	if f1.State != runcontrol.StateCompleted {
		t.Fatalf("finish = %+v", f1)
	}
	log := gittest.Output(t, root, "log", "--format=%s%n%b", "-12")
	if !strings.Contains(log, "control: applied f1") || !strings.Contains(log, "MoE-Consent: dynamic") {
		t.Fatalf("applied receipt missing or lost machine attribution:\n%s", log)
	}
}

// A park yields its owner once and holds nothing; a fresh owner retires
// a park its predecessor never reached.
func TestParkYieldsOnceAndRetiresForSuccessor(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	submitControl(t, root, runcontrol.KindPark, "k1")
	if code := controlCheckpoint(root, md, "stage test", io.Discard); code != exitYielded {
		t.Fatalf("checkpoint = %d, want yield", code)
	}
	if code := controlCheckpoint(root, md, "stage test", io.Discard); code != 0 {
		t.Fatalf("second checkpoint = %d; a park yields once", code)
	}
	submitControl(t, root, runcontrol.KindPark, "k2")
	if _, code := admitStageEntry(root, md, "stage test", true, false, time.Now().Add(2*time.Second), io.Discard); code != 0 {
		t.Fatalf("fresh entry = %d, want admitted", code)
	}
	k2, _ := controlRecord(t, root).Find("k2")
	if k2.State != runcontrol.StateCompleted || !strings.Contains(k2.Outcome, "retired") {
		t.Fatalf("stale park = %+v, want retired", k2)
	}
}

// An applied finish nobody completed is reconciled by the next guard
// holder: unknown outcome raises recovery, which stops motion, and is
// never replayed.
func TestOrphanedFinishRaisesRecovery(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	submitControl(t, root, runcontrol.KindFinish, "f1")
	if err := runcontrol.Settle(root, "moe", "change-auth", "f1", runcontrol.Settlement{
		State: runcontrol.StateApplied, Boundary: "stage code"}, "static"); err != nil {
		t.Fatal(err)
	}
	if code := controlCheckpoint(root, md, "stage test", io.Discard); code != exitYielded {
		t.Fatalf("checkpoint = %d, want yield on recovery", code)
	}
	f := controlRecord(t, root)
	if f.Recovery == nil || f.Recovery.Request != "f1" {
		t.Fatalf("recovery = %+v", f.Recovery)
	}
	if _, ok := openFinish(root, md); ok {
		t.Fatal("orphaned finish is still open")
	}
}

// Only a fresh interactive entry may resume a held run, and it clears
// the hold only once admitted.
func TestInteractiveEntryResumesAfterAdmission(t *testing.T) {
	root, md := controlFixture(t)
	submitControl(t, root, runcontrol.KindPause, "p1")
	ownForTest(t, root)
	if _, code := admitStageEntry(root, md, "stage code", false, true, time.Now(), io.Discard); code != exitYielded {
		t.Fatal("a borrowed interactive entry was admitted past a hold")
	}
	if _, code := admitStageEntry(root, md, "stage code", true, false, time.Now(), io.Discard); code != exitYielded {
		t.Fatal("a fresh headless entry was admitted past a hold")
	}
	e, code := admitStageEntry(root, md, "stage code", true, true, time.Now(), io.Discard)
	if code != 0 || !e.release {
		t.Fatalf("fresh interactive entry = %d", code)
	}
	if !controlRecord(t, root).Held() {
		t.Fatal("hold cleared before admission")
	}
	if code := e.admitted("stage code", io.Discard); code != 0 {
		t.Fatalf("admitted = %d", code)
	}
	if controlRecord(t, root).Held() {
		t.Fatal("hold survived an admitted interactive entry")
	}
}

// At the next-stage prompt a control wins: a buffered Enter does not
// advance past a hold or a finish.
func TestDecisionPromptYieldsToControlOverBufferedAnswer(t *testing.T) {
	root, md := controlFixture(t)
	submitControl(t, root, runcontrol.KindPause, "p1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	_, _, err = readStageDecision(root, md, nil, "next?", "", nil, io.Discard)
	if !errors.Is(err, errControlWake) {
		t.Fatalf("err = %v, want errControlWake", err)
	}
	var stderr bytes.Buffer
	if code := controlAtDecision(root, md, "review", false, io.Discard, &stderr); code != exitYielded {
		t.Fatalf("controlAtDecision = %d; stderr:\n%s", code, stderr.String())
	}
}

// A hold during a quota wait stops the wait at once instead of after
// the reset; the stage was never admitted.
func TestQuotaWaitWakesOnHold(t *testing.T) {
	g, now, _ := testCooldown(t)
	g.read = func(context.Context, string, string) (agent.Quota, error) {
		return quota(99, now.Add(4*time.Hour)), nil
	}
	g.after = func(time.Duration) <-chan time.Time { return nil } // never fires
	held := make(chan struct{})
	close(held)
	g.openHold = func() (<-chan struct{}, func()) { return held, func() {} }
	if got, _ := g.check(context.Background(), "claude", ""); got != cooldownHeld {
		t.Fatalf("outcome = %v, want cooldownHeld", got)
	}
}
