package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

func TestHeartbeatKeepsOfferingTerminalPRUntilReconciled(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			root, current := pushedRepairFixture(t)
			current.State = state
			stubPRChecks(t, &current)
			head := gittest.HeadSHA(t, root)
			gate := newHeartbeatGate(root)
			gate.Swept("moe", true)
			for _, g := range []*heartbeatGate{gate, newHeartbeatGate(root)} {
				for attempt := range 2 {
					decisions := dueDecisions(t, g, 0)
					if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" || !strings.Contains(reasonFor(decisions, "moe"), "reconciliation pending") {
						t.Fatalf("attempt %d: terminal PR did not wake reconciliation: %+v", attempt, decisions)
					}
					// A successful survey can follow a warn-only reconcile failure.
					g.Swept("moe", true)
				}
			}
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("heartbeat observation changed the journal")
			}
			md, err := run.Load(root, "moe", "fix-ci")
			if err != nil || md.Status != run.StatusPushed {
				t.Fatalf("observation changed status: %+v, %v", md, err)
			}
			if current.actionable() || prFailureSnapshot([]ownedPRChecks{current}) != "" {
				t.Fatal("terminal observation entered the CI repair fingerprint")
			}
		})
	}
}

func TestHeartbeatTerminalPRRequiresValidObservation(t *testing.T) {
	root, _ := pushedRepairFixture(t)
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	for _, tc := range []struct {
		name, response string
		err            error
	}{
		{"open-success", `{"state":"OPEN","url":"` + repairURL + `","headRefOid":"sha","statusCheckRollup":[{"__typename":"StatusContext","context":"ci","state":"SUCCESS"}]}`, nil},
		{"open-pending", `{"state":"OPEN","url":"` + repairURL + `","headRefOid":"sha","statusCheckRollup":[{"__typename":"StatusContext","context":"ci","state":"PENDING"}]}`, nil},
		{"unknown-state", `{"state":"UNKNOWN","url":"` + repairURL + `","headRefOid":"sha"}`, nil},
		{"missing-state", `{"url":"` + repairURL + `","headRefOid":"sha"}`, nil},
		{"missing-head", `{"state":"MERGED","url":"` + repairURL + `"}`, nil},
		{"missing-url", `{"state":"CLOSED","headRefOid":"sha"}`, nil},
		{"wrong-url", `{"state":"MERGED","url":"https://github.com/other/repo/pull/1","headRefOid":"sha"}`, nil},
		{"malformed", `{"state":"MERGED"`, nil},
		{"offline", "", errors.New("offline")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := runGH
			runGH = func(...string) ([]byte, error) { return []byte(tc.response), tc.err }
			t.Cleanup(func() { runGH = prev })
			if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
				t.Fatalf("unknown or nonterminal observation woke survey: %v", got)
			}
		})
	}
}

func TestHeartbeatTerminalPRPreservesHolds(t *testing.T) {
	root, current := pushedRepairFixture(t)
	current.State = "MERGED"
	stubPRChecks(t, &current)
	readPR := runGH
	reads := 0
	runGH = func(args ...string) ([]byte, error) { reads++; return readPR(args...) }
	sc := mustPulseScan(t, root)
	gate := newHeartbeatGate(root)
	gate.Swept("moe", true)
	for _, tc := range []struct {
		name      string
		mode      project.Mode
		occupied  bool
		tick      time.Duration
		wantSweep bool
		wantReads int
	}{
		{"paused", project.ModePaused, false, 0, false, 0},
		{"quiet-window", project.ModeAuto, false, testTick, false, 0},
		{"occupied", project.ModeAuto, true, 0, false, 0},
		{"safe", project.ModeSafe, false, 0, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads = 0
			sweep, _, reason := gate.projectDue(sc, &project.Metadata{ID: "moe", Mode: string(tc.mode)}, map[string]bool{"moe": tc.occupied}, tc.tick, time.Now())
			if sweep != tc.wantSweep || reads != tc.wantReads {
				t.Fatalf("sweep=%t, reads=%d, reason=%s; want %t/%d", sweep, reads, reason, tc.wantSweep, tc.wantReads)
			}
		})
	}
}

func TestHeartbeatTerminalPRReconcileRetryConverges(t *testing.T) {
	defer withRideMode(rideDynamic)()
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			f := newReconcileFixture(t, run.StatusPushed)
			fakeGh(t, map[string]string{f.prURL: fmt.Sprintf(`{"state":%q,"url":%q,"headRefOid":"sha","mergeCommit":{"oid":"abc1234deadbeef"}}`, state, f.prURL)})
			gate := newHeartbeatGate(f.root)
			gate.Swept(f.projectID, true)
			if got := dueDecisions(t, gate, 0); len(sweepIDs(got)) != 1 {
				t.Fatalf("terminal PR did not wake: %+v", got)
			}
			lift := failCommits(t, f.root)
			var log bytes.Buffer
			reconcileAtPulse(f.root, f.projectID, nil, io.Discard, &log)
			if f.reload().Status != run.StatusPushed || !strings.Contains(log.String(), "pulse: reconcile pushed runs") {
				t.Fatalf("failed reconcile must warn and retain pushed status: %s", &log)
			}
			gate.Swept(f.projectID, true)
			if got := dueProjectsPastTheWindow(t, gate); len(got) != 1 {
				t.Fatalf("successful survey swallowed failed reconciliation: %v", got)
			}
			lift()
			log.Reset()
			reconcileAtPulse(f.root, f.projectID, nil, io.Discard, &log)
			want := run.StatusMerged
			if state == "CLOSED" {
				want = run.StatusClosed
			}
			if f.reload().Status != want {
				t.Fatalf("retry status=%s, want %s; %s", f.reload().Status, want, &log)
			}
			if !strings.Contains(lastCommitMessage(t, f.root), "MoE-Consent: dynamic") {
				t.Fatal("machine reconciliation did not stamp its consent")
			}
			head := gittest.HeadSHA(t, f.root)
			reconcileAtPulse(f.root, f.projectID, nil, io.Discard, &log)
			if gittest.HeadSHA(t, f.root) != head {
				t.Fatal("repeated reconciliation added another terminal commit")
			}
			gate.Swept(f.projectID, true)
			if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
				t.Fatalf("reconciled board did not converge: %v", got)
			}
		})
	}
}

func TestHeartbeatTerminalPRDrivesPulseBeforeSurvey(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			f := newReconcileFixture(t, run.StatusPushed)
			writeFile(t, filepath.Join(f.root, ".gitignore"), ".moe/\n")
			seedSdlcOneShotProject(t, f.root, f.projectID)
			writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"),
				`{"id":"tele","submodule":"projects/tele/src","remote":"https://github.com/owner/repo.git","default_branch":"main","mode":"safe"}`+"\n")
			t.Setenv("MOE_HOME", f.root)
			t.Setenv("MOE_FORCE_AGENT", "claude")
			t.Setenv("NO_COLOR", "1")
			md := f.reload()
			md.Created = time.Now().Format("2006-01-02")
			if err := run.Save(f.root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, f.root, "add", "-A")
			gittest.Run(t, f.root, "commit", "-m", "test: recent pushed run")
			spy := fakeGh(t, map[string]string{f.prURL: fmt.Sprintf(`{"state":%q,"url":%q,"headRefOid":"sha","mergeCommit":{"oid":"abc1234deadbeef"}}`, state, f.prURL)})
			argvPath := filepath.Join(t.TempDir(), "survey-argv")
			t.Setenv("MOE_TEST_ARGV_DUMP", argvPath)
			fakeClaudeOnPath(t, `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOE_TEST_ARGV_DUMP"
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
[ -n "$canvas" ] || exit 1
fence=$(printf '\140\140\140')
printf '# Pulse\n\nTerminal PR surveyed.\n\n## Gate\n\n%sjson\n{"status":"ok"}\n%s\n' "$fence" "$fence" > "$canvas"
`)
			gate := newHeartbeatGate(f.root)
			gate.Swept(f.projectID, true)
			decisions := dueDecisions(t, gate, 0)
			if len(decisions) != 1 || !decisions[0].Sweep {
				t.Fatalf("real gate did not dispatch: %+v", decisions)
			}
			var out, log bytes.Buffer
			if code := runPulseNew([]string{"--dynamic", "--heartbeat", "--pr-checks", decisions[0].PRChecks, f.projectID}, &out, &log); code != 0 {
				t.Fatalf("pulse exit=%d; stdout=%s; stderr=%s", code, &out, &log)
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			want := run.StatusMerged
			trailer := "MoE-Merged: abc1234deadbeef"
			if state == "CLOSED" {
				want, trailer = run.StatusClosed, "MoE-Closed: "+f.prURL
			}
			if f.reload().Status != want || !strings.Contains(string(argv), "`"+f.runID+"` (sdlc, "+want+")") {
				t.Fatalf("survey did not receive reconciled state %s; argv=%s", want, argv)
			}
			messages := gittest.Output(t, f.root, "log", "--format=%B")
			if strings.Count(messages, trailer) != 1 || len(spy.Lines()) != 1 {
				t.Fatalf("expected one terminal transition: deletes=%v; journal=%s", spy.Lines(), messages)
			}
			row, found, err := GatherRunRow(f.root, f.projectID, f.runID, time.Now())
			if err != nil || !found || row.Bucket != dash.BucketCompletedRuns || row.Stage == "awaiting merge" {
				t.Fatalf("board still waits for merge: %+v, found=%t, err=%v", row, found, err)
			}
			gate.Swept(f.projectID, true)
			if got := dueProjectsPastTheWindow(t, gate); len(got) != 0 {
				t.Fatalf("completed survey did not converge: %v", got)
			}
		})
	}
}
