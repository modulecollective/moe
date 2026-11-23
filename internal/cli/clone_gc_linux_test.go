//go:build linux

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func gcFixture(t *testing.T, status string, at time.Time) (string, string) {
	t.Helper()
	root := seedCloseFixture(t, "tele", "finish", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	path := cleanTestClone(t, root, "tele", "finish")
	commitGCState(t, root, status, at)
	return root, path
}
func commitGCState(t *testing.T, root, status string, at time.Time) {
	t.Helper()
	md := loadRun(t, root, "tele", "finish")
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", run.Dir("tele", "finish"))
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + at.Format(time.RFC3339)}, "commit", "--allow-empty", "-m", "state\n\nMoE-Project: tele\nMoE-Run: finish\nMoE-Workflow: sdlc\n")
}
func gcReport(t *testing.T, args ...string) (cloneReport, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runCloneGC(append(args, "--json"), &out, &errb)
	var report cloneReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("JSON exit %d: %s stderr %s: %v", code, &out, &errb, err)
	}
	return report, code
}

func TestCloneGCInspectApplyAndRetainedReceipts(t *testing.T) {
	root, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	before := gittest.HeadSHA(t, root)
	report, code := gcReport(t, "tele/finish")
	if code != 0 || len(report.Candidates) != 1 || report.Candidates[0].Outcome != "eligible" {
		t.Fatalf("inspect %d %+v", code, report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("inspection removed clone")
	}
	if report.Version != 1 || report.Invocation == "" || report.Root != root || report.Candidates[0].Terminal == nil {
		t.Fatalf("missing JSON authority: %+v", report)
	}
	report, code = gcReport(t, "--apply", "tele/finish")
	if code != 0 || report.Candidates[0].Outcome != "removed" {
		t.Fatalf("apply %d %+v", code, report)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("positive apply did not remove clone")
	}
	if gittest.HeadSHA(t, root) != before {
		t.Fatal("GC changed journal state")
	}
	receipt := report.Candidates[0].Receipt
	if _, err := os.Stat(receipt); err != nil {
		t.Fatal("receipt collected")
	}
	report, code = gcReport(t, "--apply", "tele/finish")
	if code != 0 || report.Candidates[0].Reason != "already_absent" {
		t.Fatalf("repeat %d %+v", code, report)
	}
}

func TestCloneGCTwoTiersAndIndefiniteNonterminalRetention(t *testing.T) {
	for _, status := range []string{run.StatusClosed, run.StatusMerged, run.StatusPromoted, run.StatusInProgress, run.StatusPushed} {
		t.Run(status, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			root, path := gcFixture(t, status, at)
			writeFile(t, filepath.Join(path, "experiment.out"), "local work")
			early := collectClone(root, "tele", "finish", true, true, at.Add(run.CloneRetention-time.Nanosecond), io.Discard, io.Discard)
			if early.Outcome != "held" {
				t.Fatalf("early disposal: %+v", early)
			}
			deadline := collectClone(root, "tele", "finish", true, true, at.Add(run.CloneRetention), io.Discard, io.Discard)
			if run.IsTerminal(status) {
				if deadline.Outcome != "removed" || deadline.Reason != "retention_expired" {
					t.Fatalf("expiry: %+v", deadline)
				}
			} else {
				if deadline.Outcome != "held" || deadline.Reason != "nonterminal" {
					t.Fatalf("nonterminal expiry: %+v", deadline)
				}
				if _, err := os.Stat(filepath.Join(path, "experiment.out")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCloneGCLegacyBusyAndRecovery(t *testing.T) {
	root := seedCloseFixture(t, "tele", "finish", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	// Build a genuinely legacy clone without enrollment, then prove only exact
	// interactive recovery may adopt it. No collector automatically enrolls.
	path := cleanTestClone(t, root, "tele", "finish")
	h, err := runowner.Acquire(root, "tele", "finish")
	if err != nil {
		t.Fatal(err)
	}
	r, err := clonereclaim.LoadRegistry(h)
	if err != nil {
		t.Fatal(err)
	}
	gen := r.Receipt()
	r.Close()
	h.Release()
	if err := os.Remove(filepath.Join(path, ".moe", "clone-identity.json")); err != nil {
		t.Fatal(err)
	}
	// Remove this fixture's enrollment as if an old binary had created the clone.
	if err := os.Remove(filepath.Join(gen, "identity.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gen); err != nil {
		t.Fatal(err)
	}
	commitGCState(t, root, run.StatusClosed, time.Now().UTC().Add(-10*24*time.Hour))
	report, code := gcReport(t, "--apply", "tele/finish")
	if code != 0 || report.Candidates[0].Reason != "generation_evidence_invalid" {
		t.Fatalf("legacy %d %+v", code, report)
	}
	if code := runCloneRecover([]string{"tele/finish"}, io.Discard, io.Discard); code != 1 {
		t.Fatal("non-TTY recovery admitted")
	}
	h, err = runowner.Acquire(root, "tele", "finish")
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := recoverClone(h, bufio.NewReader(strings.NewReader("tele/finish stopped\n")), &log, &log); err != nil {
		h.Release()
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("recovery deleted clone")
	}
	busy := collectClone(root, "tele", "finish", true, true, time.Now().UTC(), io.Discard, io.Discard)
	if busy.Reason != "owner_busy" {
		t.Fatalf("live owner admitted: %+v", busy)
	}
	h.Release()
	report, code = gcReport(t, "--apply", "tele/finish")
	if code != 0 || report.Candidates[0].Outcome != "removed" {
		t.Fatalf("after recovery %d %+v", code, report)
	}
}

func TestCloneGCArgumentsAndExactFilter(t *testing.T) {
	_, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	for _, args := range [][]string{{"--force"}, {"--apply", "a/b", "c/d"}, {"/tmp/path"}, {"tele/*"}, {"../finish"}} {
		if code := runCloneGC(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("args %v: %d", args, code)
		}
	}
	report, code := gcReport(t, "--apply", "tele/other")
	if code != 0 || len(report.Candidates) != 0 {
		t.Fatalf("filter %d %+v", code, report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("filter removed other clone")
	}
}
