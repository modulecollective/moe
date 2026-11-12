package cli

import (
	"bytes"
	"fmt"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFinishArrivingDuringOneStageKickApplies(t *testing.T) {
	root, _ := controlFixture(t)
	captured := stubOpenSdlcStage(t, nil)
	push := stubPushFromCascade(t, 0, nil)
	inner := openSdlcStage
	openSdlcStage = func(s, p, r string, h bool, o, e io.Writer) int {
		submitControl(t, root, runcontrol.KindFinish, "mid-turn-finish")
		return inner(s, p, r, h, o, e)
	}
	var out, errs bytes.Buffer
	code := chainKickRun(root, "moe", "change-auth", rideDynamic, true, &out, &errs)
	if code != 0 {
		t.Fatalf("kick=%d: %s", code, errs.String())
	}
	f, _ := controlRecord(t, root).Find("mid-turn-finish")
	if len(*push) != 1 || f.State != runcontrol.StateCompleted {
		t.Fatalf("Finish ignored at next boundary: dispatches=%+v pushes=%d receipt=%+v", *captured, len(*push), f)
	}
}

func TestOrphanedFinishUnrecordableReconcileStops(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	submitControl(t, root, runcontrol.KindFinish, "f1")
	if err := runcontrol.Settle(root, md.Project, md.ID, "f1", runcontrol.Settlement{State: runcontrol.StateApplied, Boundary: "stage code"}, "static"); err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	var errs bytes.Buffer
	code := controlCheckpoint(root, md, "stage test", &errs)
	if code != exitYielded {
		t.Fatalf("admitted despite unreconciled applied Finish: code=%d record=%+v stderr=%s", code, controlRecord(t, root), errs.String())
	}
}

func TestPushedStatusDoesNotProveRepairFinishShipped(t *testing.T) {
	root, md := controlFixture(t)
	md.Status = run.StatusPushed
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "projects/moe/runs/change-auth/run.json")
	gittest.Run(t, root, "commit", "-m", "push: prior shipment\n\nMoE-Run: change-auth\nMoE-Project: moe\nMoE-Status: pushed")
	ownForTest(t, root)
	submitControl(t, root, runcontrol.KindFinish, "repair-finish")
	if err := runcontrol.Settle(root, md.Project, md.ID, "repair-finish", runcontrol.Settlement{State: runcontrol.StateApplied, Boundary: "push"}, "dynamic"); err != nil {
		t.Fatal(err)
	}
	code := controlCheckpoint(root, md, "stage code", io.Discard)
	if code != exitYielded || controlRecord(t, root).Recovery == nil {
		t.Fatalf("old pushed status treated as new repair shipment: code=%d record=%+v", code, controlRecord(t, root))
	}
}

func TestParkRefusedWithoutAnsweringOwner(t *testing.T) {
	root, md := controlFixture(t)
	h, err := runowner.Acquire(root, md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	got, note, err := deliverControl(root, md, runcontrol.Request{ID: "park-without-contact", Kind: runcontrol.KindPark})
	if err == nil {
		t.Fatalf("accepted unbound Park on unreachable owner: request=%+v note=%s", got, note)
	}
}

func TestHandoffNamesTheLiveOwnersConversation(t *testing.T) {
	root, md := controlFixture(t)
	for _, s := range []string{"code", "review"} {
		doc, _, err := run.EnsureDocument(root, md, s)
		if err != nil {
			t.Fatal(err)
		}
		doc.SetSession("codex", s+"-session")
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	ownForTest(t, root)
	publishPhase(root, md, "agent running", "code", "")
	code, out, errs := runControl(t, "handoff", "moe/change-auth", "--id", "handoff-during-kickback")
	if code != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(out, "moe sdlc code moe/change-auth --agent codex") {
		t.Fatalf("handoff points at old review while code is active: %s", out)
	}
}

func TestLostAckNeverConfirmsAnUncommittedControl(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	oldTimeout := controlDialTimeout
	controlDialTimeout = 500 * time.Millisecond
	defer func() { controlDialTimeout = oldTimeout }()
	hooks := t.TempDir()
	entered, release := filepath.Join(hooks, "entered"), filepath.Join(hooks, "release")
	script := fmt.Sprintf("#!/bin/sh\ntouch '%s'\nwhile [ ! -f '%s' ]; do sleep 0.01; done\nexit 1\n", entered, release)
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	defer os.WriteFile(release, nil, 0644)
	type result struct {
		r    runcontrol.Request
		note string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r, n, e := deliverControl(root, md, runcontrol.Request{ID: "lost-ack", Kind: runcontrol.KindPause})
		done <- result{r, n, e}
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("hook not reached")
		case <-time.After(10 * time.Millisecond):
		}
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery result")
	}
	if err := os.WriteFile(release, nil, 0644); err != nil {
		t.Fatal(err)
	}
	runEndpoint(root, md.Project, md.ID).Checkpoint(func() bool { return true }, nil)
	f := controlRecord(t, root)
	if _, ok := f.Find("lost-ack"); !ok && got.err == nil {
		t.Fatalf("reported success for rolled-back control: receipt=%+v note=%s durable=%+v", got.r, got.note, f)
	}
}

func TestDirectPushHonoursHold(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	if _, err := runcontrol.Submit(f.root, f.projectID, f.runID, runcontrol.Request{ID: "pause-before-push", Kind: runcontrol.KindPause}); err != nil {
		t.Fatal(err)
	}
	out, errs, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != exitYielded || f.reloadRun().Status != run.StatusInProgress {
		t.Fatalf("held run passed direct push: code=%d status=%s stdout=%s stderr=%s", code, f.reloadRun().Status, out, errs)
	}
}

// With no live owner, handoff names the last turn that actually settled,
// not the furthest stage with a session: after a review→code kickback
// the code conversation is the live one.
func TestHandoffNamesTheLastSettledTurn(t *testing.T) {
	root, md := controlFixture(t)
	for _, s := range []string{"code", "review"} {
		doc, _, err := run.EnsureDocument(root, md, s)
		if err != nil {
			t.Fatal(err)
		}
		doc.SetSession("codex", s+"-session")
	}
	md.LastTurn = &run.Turn{Stage: "code", Agent: "codex"}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runControl(t, "handoff", "moe/change-auth", "--id", "handoff-after-kickback")
	if code != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(out, "moe sdlc code moe/change-auth --agent codex") {
		t.Fatalf("handoff ignored the last settled turn: %s", out)
	}
	h, _ := controlRecord(t, root).Find("handoff-after-kickback")
	if h.Stage != "code" || h.Agent != "codex" {
		t.Fatalf("saved address = %s/%s, want code/codex", h.Stage, h.Agent)
	}
}
