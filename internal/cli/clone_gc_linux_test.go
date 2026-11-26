//go:build linux

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
			early := collectClone(root, "tele", "finish", false, true, at.Add(run.CloneRetention-time.Nanosecond), io.Discard, io.Discard)
			if status == run.StatusMerged {
				if early.Outcome != "eligible" || early.Reason != "merged_disposable" {
					t.Fatalf("merged disposal: %+v", early)
				}
			} else if early.Outcome != "held" {
				t.Fatalf("early disposal: %+v", early)
			}
			deadline := collectClone(root, "tele", "finish", true, true, at.Add(run.CloneRetention), io.Discard, io.Discard)
			if run.IsTerminal(status) {
				want := "retention_expired"
				if status == run.StatusMerged {
					want = "merged_disposable"
				}
				if deadline.Outcome != "removed" || deadline.Reason != want {
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
	if err := clonereclaim.CheckRunRecovery(h); err != nil {
		t.Fatalf("recovered legacy clone still held at admission: %v", err)
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

func TestCloneGCRealResourcesAndNoisyJSON(t *testing.T) {
	root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	parent := installCloseResourceHooks(t, root, "tele")
	script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, script, string(body)+"printf 'noisy cleanup output\\n'\n")
	gittest.Run(t, root, "add", script)
	gittest.Commit(t, root, "noisy teardown fixture")
	resources := allocateCloseResources(t, root, "tele", "finish")
	commitGCState(t, root, run.StatusMerged, time.Now().UTC().Add(-time.Hour))
	cache, err := os.ReadFile(filepath.Join(path, devEnvCacheRel))
	if err != nil {
		t.Fatal(err)
	}
	report, code := gcReport(t, "tele/finish")
	if code != 0 || report.Candidates[0].Outcome != "eligible" {
		t.Fatalf("inspect: %d %+v", code, report)
	}
	if _, err := os.Stat(filepath.Join(parent, "teardown")); !os.IsNotExist(err) {
		t.Fatal("inspection ran teardown")
	}
	after, err := os.ReadFile(filepath.Join(path, devEnvCacheRel))
	if err != nil || !bytes.Equal(cache, after) {
		t.Fatal("inspection changed cache")
	}
	var stdout, stderr bytes.Buffer
	if code := runCloneGC([]string{"--apply", "--json", "tele/finish"}, &stdout, &stderr); code != 0 {
		t.Fatalf("apply %d %s %s", code, &stdout, &stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal("hook corrupted JSON:", err)
	}
	if strings.Contains(stdout.String(), "noisy") || !strings.Contains(stderr.String(), "noisy cleanup output") {
		t.Fatalf("hook streams stdout=%s stderr=%s", &stdout, &stderr)
	}
	if report.Candidates[0].Outcome != "removed" {
		t.Fatalf("resource apply: %+v", report)
	}
	resources.assertReleased(t)
}

func TestCloneRecoveryRepairsActiveRefreshWithoutClosingOrDeleting(t *testing.T) {
	root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	parent := installCloseResourceHooks(t, root, "tele")
	script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, script, strings.Replace(string(body), "set -eu\n", "set -eu\nif [ -f \"$TEST_CLOSE_PARENT/fail\" ]; then exit 17; fi\n", 1))
	gittest.Run(t, root, "add", script)
	gittest.Commit(t, root, "retryable fixture contract")
	resources := allocateCloseResources(t, root, "tele", "finish")
	writeFile(t, filepath.Join(parent, "fail"), "fail")
	h, err := runowner.Acquire(root, "tele", "finish")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	md := loadRun(t, root, "tele", "finish")
	if err := devEnvTeardownOwned(context.Background(), h, md, "refresh", false, io.Discard, io.Discard); err == nil {
		t.Fatal("failure fixture succeeded")
	}
	if err := os.Remove(filepath.Join(parent, "fail")); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := recoverClone(h, bufio.NewReader(strings.NewReader("tele/finish stopped\n")), &log, &log); err != nil {
		t.Fatal(err)
	}
	if loadRun(t, root, "tele", "finish").Status != run.StatusInProgress {
		t.Fatal("refresh recovery terminalized active run")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refresh recovery deleted clone")
	}
	if _, err := os.Stat(resources.owned); !os.IsNotExist(err) {
		t.Fatal("refresh recovery did not execute disposal contract")
	}
	if _, err := os.Stat(filepath.Join(path, devEnvCacheRel)); !os.IsNotExist(err) {
		t.Fatal("refresh recovery left reusable old cache")
	}
	if _, fresh, err := devEnvSetupOwned(h, md, io.Discard, io.Discard); err != nil || !fresh {
		t.Fatalf("resume setup fresh=%v %v", fresh, err)
	}
	if err := devEnvTeardownOwned(context.Background(), h, md, "refresh", false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

type recoveryInput struct {
	first func()
	text  *strings.Reader
}

func (r *recoveryInput) Read(p []byte) (int, error) {
	if r.first != nil {
		f := r.first
		r.first = nil
		f()
	}
	return r.text.Read(p)
}

func TestCloneRecoveryRefusesStalePromptAndIndependentDebts(t *testing.T) {
	for _, kind := range []string{"stale", "session-ref", "pending-close", "ship"} {
		t.Run(kind, func(t *testing.T) {
			root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
			h, err := runowner.Acquire(root, "tele", "finish")
			if err != nil {
				t.Fatal(err)
			}
			defer h.Release()
			input := &recoveryInput{text: strings.NewReader("tele/finish stopped\n")}
			switch kind {
			case "stale":
				input.first = func() {
					md := loadRun(t, root, "tele", "finish")
					md.Agent = "fresh"
					if err := run.Save(root, md); err != nil {
						t.Fatal(err)
					}
				}
			case "session-ref":
				gittest.Run(t, root, "update-ref", "refs/heads/session/tele/finish/old/claude", "HEAD")
			case "pending-close":
				writeFile(t, filepath.Join(root, ".moe", "session-closes", "tele", "finish", "old.json"), "{}")
			case "ship":
				writeFile(t, mergeRecordPendingPath(root, loadRun(t, root, "tele", "finish")), "{}")
			}
			var log bytes.Buffer
			if err := recoverClone(h, bufio.NewReader(input), &log, &log); err == nil {
				t.Fatal("unsafe recovery admitted:", &log)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("refused recovery lost clone")
			}
			if loadRun(t, root, "tele", "finish").Status != run.StatusInProgress {
				t.Fatal("refused recovery closed run")
			}
		})
	}
}

func TestCloneGCPostTeardownOutputRetainsTreeWithoutRepeatingHooks(t *testing.T) {
	root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
	installCloseResourceHooks(t, root, "tele")
	script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, script, string(body)+"printf 'late evidence\\n' > \"$MOE_SANDBOX/after-hook\"\n")
	gittest.Run(t, root, "add", script)
	gittest.Commit(t, root, "cleanup emits local evidence")
	resources := allocateCloseResources(t, root, "tele", "finish")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	commitGCState(t, root, run.StatusClosed, at)
	result := collectClone(root, "tele", "finish", true, true, at.Add(time.Hour), io.Discard, io.Discard)
	if result.Outcome != "held" || result.Reason != "post_teardown_work_retained" {
		t.Fatalf("late output discarded: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(path, "after-hook")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resources.owned); !os.IsNotExist(err) {
		t.Fatal("teardown did not run")
	}
	result = collectClone(root, "tele", "finish", true, true, at.Add(2*time.Hour), io.Discard, io.Discard)
	if result.Outcome != "held" {
		t.Fatalf("repeat discarded output: %+v", result)
	}
	result = collectClone(root, "tele", "finish", true, true, at.Add(run.CloneRetention), io.Discard, io.Discard)
	if result.Outcome != "removed" {
		t.Fatalf("expiry could not finish disposal: %+v", result)
	}
	resources.assertReleased(t)
}

func TestCloneShellAndCloseRefuseIndependentDebts(t *testing.T) {
	for _, kind := range []string{"session-ref", "pending-close", "ship"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := gcFixture(t, run.StatusInProgress, time.Now().UTC())
			want := ""
			switch kind {
			case "session-ref":
				gittest.Run(t, root, "update-ref", "refs/heads/session/tele/finish/old/claude", "HEAD")
				want = "unresolved sessions"
			case "pending-close":
				writeFile(t, filepath.Join(root, ".moe", "session-closes", "tele", "finish", "old.json"), "{}")
				want = "pending session close"
			case "ship":
				writeFile(t, mergeRecordPendingPath(root, loadRun(t, root, "tele", "finish")), "{}")
				want = "unrecorded shipment"
			}
			marker := filepath.Join(t.TempDir(), "shell-started")
			script := filepath.Join(t.TempDir(), "shell")
			writeFile(t, script, "#!/bin/sh\nprintf started > \"$MOE_TEST_SHELL_MARKER\"\n")
			if err := os.Chmod(script, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SHELL", script)
			t.Setenv("MOE_TEST_SHELL_MARKER", marker)
			var log bytes.Buffer
			if code := shellRunWorkspace(root, "tele", "finish", &log, &log); code != 1 || !strings.Contains(log.String(), want) {
				t.Fatalf("shell debt: %d %s", code, &log)
			}
			log.Reset()
			if code := Run([]string{"sdlc", "close", "tele/finish"}, &log, &log); code != 1 || !strings.Contains(log.String(), want) {
				t.Fatalf("close debt: %d %s", code, &log)
			}
			if kind == "ship" {
				log.Reset()
				if code := Run([]string{"sdlc", "design", "--once", "tele/finish"}, &log, &log); code != 1 || !strings.Contains(log.String(), want) {
					t.Fatalf("stage debt: %d %s", code, &log)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("shell launched through unresolved debt")
			}
			if loadRun(t, root, "tele", "finish").Status != run.StatusInProgress {
				t.Fatal("debt refusal terminalized run")
			}
		})
	}
}

func TestCloneGCProbeFailuresExitOneAndRetainResources(t *testing.T) {
	t.Run("missing allocated cache", func(t *testing.T) {
		root, path := gcFixture(t, run.StatusInProgress, time.Now().UTC())
		installCloseResourceHooks(t, root, "tele")
		resources := allocateCloseResources(t, root, "tele", "finish")
		commitGCState(t, root, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
		if err := os.Remove(filepath.Join(path, devEnvCacheRel)); err != nil {
			t.Fatal(err)
		}
		report, code := gcReport(t, "--apply", "tele/finish")
		if code != 1 || report.Candidates[0].Outcome != "failed" || report.Candidates[0].Reason != "cleanup_probe_failed" {
			t.Fatalf("missing cache treated as a no-op: %d %+v", code, report)
		}
		for _, p := range []string{path, resources.owned, resources.sibling} {
			if _, err := os.Stat(p); err != nil {
				t.Fatal("probe lost resources:", err)
			}
		}
		if _, err := os.Stat(resources.receipt); !os.IsNotExist(err) {
			t.Fatal("probe failure launched cleanup")
		}
	})
	t.Run("symlinked inventory", func(t *testing.T) {
		_, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
		parent := filepath.Dir(path)
		if err := os.Rename(parent, parent+"-retained"); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		writeFile(t, filepath.Join(outside, "sentinel"), "survives")
		if err := os.Symlink(outside, parent); err != nil {
			t.Fatal(err)
		}
		report, code := gcReport(t, "--apply")
		if code != 1 || report.Error != "inventory_failed" || len(report.Candidates) != 0 {
			t.Fatalf("partial inventory accepted: %d %+v", code, report)
		}
		if b, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(b) != "survives" {
			t.Fatal("inventory followed outside link")
		}
	})
}

func TestCloneGCSkipsInvalidInventoryNames(t *testing.T) {
	root, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	var strays []string
	for _, base := range []string{"clones", "clone-reclamation"} {
		for _, suffix := range []string{".claude", "tele/.claude"} {
			stray := filepath.Join(root, ".moe", base, suffix)
			if err := os.MkdirAll(filepath.Join(stray, ".cc-writes"), 0o700); err != nil {
				t.Fatal(err)
			}
			strays = append(strays, stray)
		}
	}
	for _, mode := range []string{"text inspect", "json inspect", "json apply", "json retry"} {
		t.Run(mode, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			args := []string{}
			asJSON := strings.HasPrefix(mode, "json")
			if asJSON {
				args = append(args, "--json")
			}
			if mode == "json apply" || mode == "json retry" {
				args = append(args, "--apply")
			}
			if code := runCloneGC(args, &out, &diagnostics); code != 0 {
				t.Fatalf("exit %d: stdout %s stderr %s", code, &out, &diagnostics)
			}
			want := "eligible"
			if mode == "json apply" {
				want = "removed"
			} else if mode == "json retry" {
				want = "held"
			}
			if asJSON {
				var report cloneReport
				decoder := json.NewDecoder(&out)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&report); err != nil {
					t.Fatal(err)
				}
				if err := decoder.Decode(new(any)); err != io.EOF {
					t.Fatalf("extra stdout: %v", err)
				}
				if report.Version != 1 || report.Root != root || report.Invocation == "" || report.Error != "" || len(report.Candidates) != 1 || report.Candidates[0].Project != "tele" || report.Candidates[0].Run != "finish" || report.Candidates[0].Outcome != want {
					t.Fatalf("report = %+v; want %s", report, want)
				}
				if mode == "json retry" && report.Candidates[0].Reason != "already_absent" {
					t.Fatalf("retry: %+v", report)
				}
			} else if !strings.Contains(out.String(), "tele/finish: eligible (") || strings.Contains(out.String(), ".claude") {
				t.Fatalf("text output: %s", &out)
			}
			if strings.Count(diagnostics.String(), "clone gc inventory: skipping ") != len(strays) {
				t.Fatalf("warnings: %s", &diagnostics)
			}
			for _, stray := range strays {
				if !strings.Contains(diagnostics.String(), fmt.Sprintf("%q", stray)) {
					t.Errorf("missing diagnostic for %s: %s", stray, &diagnostics)
				}
				if entries, err := os.ReadDir(filepath.Join(stray, ".cc-writes")); err != nil || len(entries) != 0 {
					t.Errorf("stray changed: %s: %v %v", stray, entries, err)
				}
			}
			_, err := os.Stat(path)
			if want == "eligible" && err != nil {
				t.Fatal("inspection removed clone:", err)
			} else if want != "eligible" && !os.IsNotExist(err) {
				t.Fatal("apply retained clone:", err)
			}
		})
	}
}

func TestCloneGCAllInvalidInventorySucceeds(t *testing.T) {
	root := seedCloseFixture(t, "tele", "finish", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	stray := filepath.Join(root, ".moe", "clones", ".claude", ".cc-writes")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--apply"}, {"--json"}, {"--json", "--apply"}} {
		var out, diagnostics bytes.Buffer
		if code := runCloneGC(args, &out, &diagnostics); code != 0 {
			t.Fatalf("args %v: exit %d: %s", args, code, &diagnostics)
		}
		if slices.Contains(args, "--json") {
			var report cloneReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Version != 1 || report.Error != "" || len(report.Candidates) != 0 {
				t.Fatalf("empty JSON: %+v %v", report, err)
			}
		} else if out.String() != "clone gc: no candidates\n" {
			t.Fatalf("empty text: %q", &out)
		}
		if strings.Count(diagnostics.String(), "invalid project ID") != 1 {
			t.Fatalf("warnings: %s", &diagnostics)
		}
		if _, err := os.Stat(stray); err != nil {
			t.Fatal("stray removed:", err)
		}
	}
}

func TestCloneGCSkippedEntryBeforeFatalInventoryError(t *testing.T) {
	root, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-time.Hour))
	stray := filepath.Join(root, ".moe", "clones", ".claude")
	writeFile(t, filepath.Join(stray, ".cc-writes", "sentinel"), "retained stray")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "sentinel"), "outside survives")
	if err := os.Symlink(outside, filepath.Join(root, ".moe", "clone-reclamation", "valid-link")); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	code := runCloneGC([]string{"--json", "--apply"}, &out, &diagnostics)
	var report cloneReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 1 || report.Error != "inventory_failed" || len(report.Candidates) != 0 {
		t.Fatalf("partial inventory accepted: %d %+v", code, report)
	}
	warning := fmt.Sprintf("clone gc inventory: skipping %q: invalid project ID\n", stray)
	if !strings.HasPrefix(diagnostics.String(), warning) || !strings.Contains(strings.TrimPrefix(diagnostics.String(), warning), "clone gc inventory: ") {
		t.Fatalf("lost warning or fatal error: %s", &diagnostics)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("fatal scan removed eligible clone:", err)
	}
	for file, want := range map[string]string{filepath.Join(stray, ".cc-writes", "sentinel"): "retained stray", filepath.Join(outside, "sentinel"): "outside survives"} {
		if b, err := os.ReadFile(file); err != nil || string(b) != want {
			t.Fatalf("changed %s: %q %v", file, b, err)
		}
	}
}
