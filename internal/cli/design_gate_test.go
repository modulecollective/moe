package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

const readyDesign = "# Design\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\nA bounded plan.\n"

// Extract the example we actually teach, rather than duplicating its grammar.
func TestDesignGateProducerContract(t *testing.T) {
	fragment := moe.Stage("sdlc", "design")
	start := strings.Index(fragment, "    ## Gate\n")
	if start < 0 {
		t.Fatal("embedded design prompt has no complete gate example")
	}
	example := fragment[start:]
	if end := strings.Index(example, "\n- **blocked:"); end >= 0 {
		example = example[:end]
	}
	example = strings.ReplaceAll(example, "    ", "")
	for name, body := range map[string]string{"prompt": example, "skeleton": designCanvasSkeleton} {
		t.Run(name, func(t *testing.T) {
			for _, status := range []string{"blocked", "ready", "close"} {
				edited := strings.Replace(body, `"status":"blocked"`, `"status":"`+status+`"`, 1)
				if got, ok := stageGateStatus(edited); !ok || got != status {
					t.Fatalf("%s producer = (%q,%v)", status, got, ok)
				}
			}
			if _, ok := stageGateStatus(strings.Replace(body, "```json", "", 1)); ok {
				t.Fatal("missing opening JSON fence must fail")
			}
		})
	}
}

func TestDesignStageGate(t *testing.T) {
	wf, _ := LookupWorkflow("sdlc")
	for _, tc := range []struct {
		name, body string
		ready      bool
	}{
		{"ready", readyDesign, true},
		{"blocked", designCanvasSkeleton, false},
		{"close", strings.Replace(readyDesign, `"ready"`, `"close"`, 1), false},
		{"missing", "", false},
		{"bare JSON", "# Design\n\n## Gate\n\n{\"status\":\"ready\"}\n", false},
		{"malformed", strings.Replace(readyDesign, `"ready"`, `ready`, 1), false},
		{"unknown", strings.Replace(readyDesign, `"ready"`, `"done"`, 1), false},
		{"quoted later ready", designCanvasSkeleton + "\n## Evidence\n\n````\n" + readyDesign + "````\n", false},
		{"quoted later blocked", readyDesign + "\n## Evidence\n\n````\n" + designCanvasSkeleton + "````\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			md := stageMetadata("p", "r", "sdlc")
			if tc.body != "" {
				writeStageCanvas(t, root, md, "design", tc.body)
			}
			if ok, err := wf.CheckStageGate(root, md, "design"); err != nil || ok != tc.ready {
				t.Fatalf("gate=(%v,%v), want %v", ok, err, tc.ready)
			}
		})
	}
	t.Run("read error", func(t *testing.T) {
		root := t.TempDir()
		md := stageMetadata("p", "r", "sdlc")
		if err := os.MkdirAll(filepath.Join(root, run.ContentPath("p", "r", "design")), 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := wf.CheckStageGate(root, md, "design"); err == nil {
			t.Fatal("real read failure must propagate")
		}
	})
}

func TestDesignPromptParksBeforeEveryOverride(t *testing.T) {
	for _, override := range []string{"", "review", "push"} {
		for _, body := range []string{designCanvasSkeleton, "## Gate\n\n{\"status\":\"ready\"}\n"} {
			t.Run(override+body[:6], func(t *testing.T) {
				root := isolateCascadeMoeHome(t)
				md := stageMetadata("tele", "parked", "sdlc")
				writeStageCanvas(t, root, md, "design", body)
				calls := stubOpenSdlcStage(t, nil)
				push := stubPushFromCascade(t, 0, nil)
				var out, errs bytes.Buffer
				if code := promptNextStageOverride(root, md, "design", override, false, &out, &errs); code != 0 {
					t.Fatalf("exit %d: %s", code, &errs)
				}
				if !strings.Contains(out.String(), "design not ready — parked at design") || !strings.Contains(out.String(), "next: moe sdlc design tele/parked") {
					t.Fatalf("missing park reason/resume: %s", &out)
				}
				for _, gone := range []string{"next: moe sdlc build", "next: moe sdlc review", "next: moe sdlc push", "advance", "run now?"} {
					if strings.Contains(out.String(), gone) {
						t.Fatalf("offered %q: %s", gone, &out)
					}
				}
				if len(*calls) != 0 || len(*push) != 0 {
					t.Fatal("unready design dispatched work")
				}
			})
		}
	}
	root := isolateCascadeMoeHome(t)
	md := stageMetadata("tele", "ready", "sdlc")
	writeStageCanvas(t, root, md, "design", readyDesign)
	var out, errs bytes.Buffer
	if code := promptNextStageOverride(root, md, "design", "", true, &out, &errs); code != 0 || !strings.Contains(out.String(), "next: moe sdlc build tele/ready") {
		t.Fatalf("ready tail: %d %s %s", code, &out, &errs)
	}
}

func TestDesignCascadeVerdicts(t *testing.T) {
	for _, verdict := range []string{"ready", "blocked", "bare", "close"} {
		for _, yolo := range []bool{false, true} {
			t.Run(verdict+map[bool]string{false: "/bounded", true: "/ship"}[yolo], func(t *testing.T) {
				root := isolateCascadeMoeHome(t)
				md := stageMetadata("tele", "r", "sdlc")
				body := strings.Replace(designCanvasSkeleton, `"blocked"`, `"`+verdict+`"`, 1)
				if verdict == "bare" {
					body = strings.Replace(body, "```json", "", 1)
				}
				calls := stubSdlcStageWritingCanvas(t, root, md, "design", body)
				closes := stubGroupCloseCommand(t, "sdlc", 0)
				destination := "build"
				if yolo {
					destination = ""
				}
				if verdict == "ready" && yolo {
					inner := openSdlcStage
					openSdlcStage = func(s, p, r string, h bool, o, e io.Writer) int {
						code := inner(s, p, r, h, o, e)
						if s == "build" {
							writeStampedBuild(t, root, md)
						}
						if s == "review" {
							writeStageCanvas(t, root, md, "review", readyDesign)
						}
						return code
					}
				}
				pushes := stubPushFromCascade(t, 0, nil)
				res, code := cascadeFromGate("design", destination, false, false, md, io.Discard, io.Discard)
				wantCode := 0
				if verdict == "blocked" || verdict == "bare" {
					wantCode = 1
				}
				if code != wantCode {
					t.Fatalf("exit %d want %d", code, wantCode)
				}
				wantCalls := 1
				if verdict == "ready" && yolo {
					wantCalls = 3
				}
				if len(*calls) != wantCalls || (*calls)[0] != "design" {
					t.Fatalf("extra dispatch: %v", *calls)
				}
				wantClose := 0
				if verdict == "close" {
					wantClose = 1
				}
				wantPush := 0
				if verdict == "ready" && yolo {
					wantPush = 1
				}
				if len(*pushes) != wantPush {
					t.Fatalf("pushes=%d want %d", len(*pushes), wantPush)
				}
				wantShip := wantClose == 1 || (verdict == "ready" && yolo)
				if len(*closes) != wantClose || res.shipped != wantShip {
					t.Fatalf("close calls=%v result=%+v", *closes, res)
				}
			})
		}
	}
}

func TestDesignCloseBeforeReadinessAndControlBeforeClose(t *testing.T) {
	for _, closeCode := range []int{0, 9} {
		t.Run(string(rune('0'+closeCode)), func(t *testing.T) {
			root := isolateCascadeMoeHome(t)
			md := stageMetadata("tele", "r", "sdlc")
			writeStageCanvas(t, root, md, "design", strings.Replace(designCanvasSkeleton, `"blocked"`, `"close"`, 1))
			calls := stubGroupCloseCommand(t, "sdlc", closeCode)
			var out, errs bytes.Buffer
			if code := promptNextStageOverride(root, md, "design", "", false, &out, &errs); code != closeCode || len(*calls) != 1 {
				t.Fatalf("exit=%d closes=%v %s", code, *calls, &errs)
			}
			if strings.Contains(out.String(), "next:") {
				t.Fatalf("offered continuation: %s", &out)
			}
		})
	}
	root, md := controlFixture(t)
	writeStageCanvas(t, root, md, "design", strings.Replace(designCanvasSkeleton, `"blocked"`, `"close"`, 1))
	closes := stubGroupCloseCommand(t, "sdlc", 0)
	submitControl(t, root, runcontrol.KindPause, "design-hold")
	if code := promptNextStageOverride(root, md, "design", "", false, io.Discard, io.Discard); code != exitYielded || len(*closes) != 0 {
		t.Fatalf("hold exit=%d closes=%v", code, *closes)
	}
}

func TestDesignGateSatisfactionRequiresVerdictAndNormalEvidence(t *testing.T) {
	wf, _ := LookupWorkflow("sdlc")
	for _, evidence := range []string{"none", "advance", "successor", "legacy"} {
		t.Run(evidence, func(t *testing.T) {
			root := newTestBureaucracy(t)
			md := stageMetadata("p", "r", "sdlc")
			t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			if evidence != "none" {
				trailerstest.CommitWorkTurnAt(t, root, "p", "r", "sdlc", "design", t0)
			}
			if evidence == "advance" {
				trailerstest.CommitAdvanceAt(t, root, "p", "r", "sdlc", "design", t0.Add(time.Minute))
			}
			if evidence == "successor" || evidence == "legacy" {
				stage := "build"
				if evidence == "legacy" {
					stage = "code"
				}
				trailerstest.CommitWorkTurnAt(t, root, "p", "r", "sdlc", stage, t0.Add(time.Minute))
			}
			for _, body := range []string{"", designCanvasSkeleton, readyDesign} {
				if body != "" {
					writeStageCanvas(t, root, md, "design", body)
				}
				ok, err := wf.stageSatisfied(root, md, "design", nil)
				want := body == readyDesign && evidence != "none"
				if err != nil || ok != want {
					t.Fatalf("evidence=%s ready=%v satisfied=(%v,%v)", evidence, body == readyDesign, ok, err)
				}
				next, kind, err := wf.Next(root, md)
				if err != nil || kind != NextKindStage || (next != "design") != want {
					t.Fatalf("Next=(%q,%v,%v) want past design=%v", next, kind, err, want)
				}
			}
		})
	}
	// A ready work turn by itself still parks; it needs advance/successor evidence.
	root := newTestBureaucracy(t)
	md := stageMetadata("p", "r", "sdlc")
	writeStageCanvas(t, root, md, "design", readyDesign)
	trailerstest.CommitWorkTurnAt(t, root, "p", "r", "sdlc", "design", time.Now())
	if ok, err := wf.stageSatisfied(root, md, "design", nil); err != nil || ok {
		t.Fatalf("ready alone satisfied=(%v,%v)", ok, err)
	}
}

// writeReadyDesign gives timeline fixtures the explicit judgment that a
// real successful design turn now carries, retaining their authored prose.
func writeReadyDesign(t *testing.T, root string, md *run.Metadata) {
	t.Helper()
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "design"))
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeStageCanvas(t, root, md, "design", strings.Replace(resetGate(string(body)), `"blocked"`, `"ready"`, 1))
}

func TestDesignGateStopsPendingFinish(t *testing.T) {
	root, md := controlFixture(t)
	writeStageCanvas(t, root, md, "design", designCanvasSkeleton)
	calls := stubOpenSdlcStage(t, nil)
	pushes := stubPushFromCascade(t, 0, nil)
	submitControl(t, root, runcontrol.KindFinish, "finish-design")
	if code := promptNextStageOverride(root, md, "design", "review", false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	f, _ := controlRecord(t, root).Find("finish-design")
	if f.State != runcontrol.StateFailed || !strings.Contains(f.Outcome, "gate blocked") {
		t.Fatalf("finish receipt=%+v", f)
	}
	if len(*calls) != 0 || len(*pushes) != 0 {
		t.Fatal("finish bypassed design gate")
	}
}

func TestDesignMachineKickCannotAdvanceAnUnreadyVerdict(t *testing.T) {
	root, md := controlFixture(t)
	md.SpawnedBy = "moe/pulse-one"
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	// Machine-baked seeds remain kickable, but their design turn must
	// produce a ready verdict before that ride can reach build.
	calls := stubSdlcStageWritingCanvas(t, root, md, "design", designCanvasSkeleton+"\nStill needs a contract decision.\n")
	pushes := stubPushFromCascade(t, 0, nil)
	if code := chainKickRun(root, md.Project, md.ID, rideDynamic, false, io.Discard, io.Discard); code == 0 {
		t.Fatal("unready machine design advanced")
	}
	if len(*calls) != 1 || (*calls)[0] != "design" || len(*pushes) != 0 {
		t.Fatalf("kick dispatches=%v pushes=%d", *calls, len(*pushes))
	}
}
