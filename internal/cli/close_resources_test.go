package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/workspace"
)

// Real hooks keep the resource address in the cache. Teardown must consume
// that address while both the clone and cache still exist, before removal.
func installCloseResourceHooks(t *testing.T, root, projectID string) string {
	t.Helper()
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "sibling"), "keep\n")
	t.Setenv("TEST_CLOSE_PARENT", parent)
	t.Setenv("TEST_CLOSE_ROOT", filepath.Join(parent, "sibling"))
	hooks := filepath.Join("projects", projectID, "hooks")
	for dir, script := range map[string]string{
		devEnvDirRel: `#!/bin/sh
set -eu
owned=$(mktemp -d "$TEST_CLOSE_PARENT/owned.XXXXXX")
printf '%s\n' "$owned" >> "$TEST_CLOSE_PARENT/setup"
printf 'TEST_CLOSE_ROOT=%s\nTEST_CLOSE_RECEIPT=%s\n' "$owned" "$TEST_CLOSE_PARENT/teardown"
`,
		devEnvTeardownDirRel: `#!/bin/sh
set -eu
test -d "$MOE_SANDBOX"
test -f "$MOE_SANDBOX/.moe/dev-env.env"
test -d "$TEST_CLOSE_ROOT"
printf '%s\n' "$TEST_CLOSE_ROOT" >> "$TEST_CLOSE_RECEIPT"
rm -r -- "$TEST_CLOSE_ROOT"
`,
	} {
		path := filepath.Join(root, hooks, dir, "10-resources.sh")
		writeFile(t, path, script)
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, root, "add", hooks)
	gittest.Run(t, root, "commit", "-m", "test: install resource hooks")
	return parent
}

type closeResources struct{ clone, owned, receipt, sibling string }

// Use a real enrolled clone and the production ownership path: a directory
// fixture cannot prove preservation or external-resource cleanup authorization.
func allocateCloseResources(t *testing.T, root, projectID, runID string) closeResources {
	t.Helper()
	cleanTestClone(t, root, projectID, runID)
	md := loadRun(t, root, projectID, runID)
	h, err := runowner.Acquire(root, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	clone, err := ensureRunClone(h)
	if err != nil {
		t.Fatal(err)
	}
	env, fresh, err := devEnvSetupOwned(h, md, io.Discard, io.Discard)
	if err != nil || !fresh {
		t.Fatalf("setup: fresh=%v err=%v", fresh, err)
	}
	return closeResources{clone, env["TEST_CLOSE_ROOT"], env["TEST_CLOSE_RECEIPT"], filepath.Join(os.Getenv("TEST_CLOSE_PARENT"), "sibling")}
}

func (r closeResources) assertReleased(t *testing.T) {
	t.Helper()
	for _, path := range []string{r.clone, r.owned} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("resource survives close: %s (err=%v)", path, err)
		}
	}
	body, err := os.ReadFile(r.receipt)
	if err != nil || strings.Count(string(body), r.owned+"\n") != 1 {
		t.Errorf("teardown receipt=%q err=%v; want exactly one %s", body, err, r.owned)
	}
	if body, err := os.ReadFile(r.sibling); err != nil || string(body) != "keep\n" {
		t.Errorf("sibling changed: %q, %v", body, err)
	}
}

func (r closeResources) assertRetained(t *testing.T) {
	t.Helper()
	for _, path := range []string{r.clone, r.owned, filepath.Join(r.clone, devEnvCacheRel), r.sibling} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("resource lost: %s: %v", path, err)
		}
	}
	if _, err := os.Stat(r.receipt); !os.IsNotExist(err) {
		t.Errorf("teardown ran on refused close: %v", err)
	}
}

func assertCloseRecorded(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	if got := loadRun(t, root, projectID, runID).Status; got != run.StatusClosed {
		t.Errorf("status=%s, want closed", got)
	}
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	for _, want := range []string{"Close " + workflow + " run " + projectID + "/" + runID, "MoE-Workflow: " + workflow, "MoE-Run: " + runID, "MoE-Project: " + projectID} {
		if !strings.Contains(head, want) {
			t.Errorf("close commit missing %q: %s", want, head)
		}
	}
	if got := strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain", "--untracked-files=no")); got != "" {
		t.Errorf("close left dirty tree: %s", got)
	}
}

func TestWorkflowCloseReleasesResources(t *testing.T) {
	for _, workflow := range []string{"sdlc", "pulse", "chat"} {
		for _, state := range []string{"cached", "no-cache", "no-clone"} {
			t.Run(workflow+"/"+state, func(t *testing.T) {
				root := seedCloseFixture(t, "tele", "finish", workflow, run.StatusInProgress)
				t.Setenv("MOE_HOME", root)
				parent := installCloseResourceHooks(t, root, "tele")
				var resources closeResources
				if state != "no-clone" {
					resources = allocateCloseResources(t, root, "tele", "finish")
					if state == "no-cache" {
						if err := devEnvClearCache(resources.clone); err != nil {
							t.Fatal(err)
						}
					}
				}
				if state != "cached" {
					path := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
					writeFile(t, path, "#!/bin/sh\nprintf called > \"$TEST_CLOSE_PARENT/teardown\"\nexit 1\n")
					gittest.Run(t, root, "add", path)
					gittest.Run(t, root, "commit", "-m", "test: fail if teardown runs without cache")
				}
				var log bytes.Buffer
				code := Run([]string{workflow, "close", "tele/finish"}, &log, &log)
				if state == "no-cache" {
					if code != 1 || !strings.Contains(log.String(), "original owned cache missing") {
						t.Fatalf("missing allocated cache was not held: %d %s", code, &log)
					}
					if loadRun(t, root, "tele", "finish").Status != run.StatusInProgress {
						t.Fatal("missing ownership admitted close")
					}
					for _, path := range []string{resources.clone, resources.owned} {
						if _, err := os.Stat(path); err != nil {
							t.Fatal("missing-cache hold lost resource:", err)
						}
					}
					if _, err := os.Stat(filepath.Join(parent, "teardown")); !os.IsNotExist(err) {
						t.Fatal("missing-cache hold ran teardown")
					}
					return
				}
				if code != 0 {
					t.Fatalf("close exit=%d: %s", code, &log)
				}
				assertCloseRecorded(t, root, "tele", "finish", workflow)
				if state == "cached" {
					resources.assertReleased(t)
				} else {
					if sandbox.Exists(root, "tele", "finish") {
						t.Error("close retained or allocated clone")
					}
					if _, err := os.Stat(filepath.Join(parent, "teardown")); !os.IsNotExist(err) {
						t.Errorf("teardown called without cache: %v", err)
					}
					if state == "no-cache" {
						if _, err := os.Stat(resources.owned); err != nil {
							t.Errorf("no-cache close guessed resource address: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestServeCloseReleasesResources(t *testing.T) {
	for _, workflow := range []string{"pulse", "chat"} {
		t.Run(workflow, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "finish", workflow, run.StatusInProgress)
			installCloseResourceHooks(t, root, "tele")
			resources := allocateCloseResources(t, root, "tele", "finish")
			if err := serveOptions(root, io.Discard).CloseRun("tele", "finish"); err != nil {
				t.Fatal(err)
			}
			resources.assertReleased(t)
			assertCloseRecorded(t, root, "tele", "finish", workflow)
		})
	}
}

func TestWorkflowClosePreservesReservedResources(t *testing.T) {
	for _, workflow := range []string{"pulse", "chat"} {
		t.Run(workflow, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "finish", workflow, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			installCloseResourceHooks(t, root, "tele")
			resources := allocateCloseResources(t, root, "tele", "finish")
			_, stop, err := session.Reserve(root, "tele", "finish", workflow, false)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			var log bytes.Buffer
			if code := Run([]string{workflow, "close", "tele/finish"}, &log, &log); code == 0 || !strings.Contains(log.String(), "session/") {
				t.Fatalf("reserved close exit=%d: %s", code, &log)
			}
			resources.assertRetained(t)
			if got := loadRun(t, root, "tele", "finish").Status; got != run.StatusInProgress {
				t.Fatalf("status=%s", got)
			}
		})
	}
}

func TestPulseStampedCloseCleanupFailureKeepsCommittedStamp(t *testing.T) {
	root := seedCloseFixture(t, "tele", "finish", pulseWorkflow, run.StatusInProgress)
	writePulseGate(t, root, "tele", "finish", `{"status":"ok"}`)
	installCloseResourceHooks(t, root, "tele")
	resources := allocateCloseResources(t, root, "tele", "finish")
	before := readPulseCanvas(t, root, "tele", "finish")
	reg := closeRegistrations[pulseWorkflow]
	t.Cleanup(func() { closeRegistrations[pulseWorkflow] = reg })
	called := false
	broken := reg
	broken.cleanup = func(root string, md *run.Metadata) error {
		called = true
		if !strings.Contains(readPulseCanvas(t, root, "tele", "finish"), "Stamped") {
			t.Error("cleanup ran before stamp")
		}
		if staged := gittest.Output(t, root, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("cleanup ran before stamp commit: %s", staged)
		}
		if md.Status != run.StatusClosed {
			t.Errorf("cleanup preceded terminal state: %s", md.Status)
		}
		if err := repolock.With(root, repolock.Options{Purpose: "test cleanup outside lock"}, func() error { return nil }); err != nil {
			t.Errorf("cleanup still holds repolock: %v", err)
		}
		return errors.New("cleanup refused")
	}
	closeRegistrations[pulseWorkflow] = broken
	err := closePulseRunStamped(root, "tele", "finish", "test", func(body []byte) []byte { return append(body, []byte("\nStamped\n")...) }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cleanup refused") || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	resources.assertRetained(t)
	if got := readPulseCanvas(t, root, "tele", "finish"); got != before+"\nStamped\n" {
		t.Errorf("committed stamp was changed: %q", got)
	}
	if got := loadRun(t, root, "tele", "finish").Status; got != run.StatusClosed {
		t.Errorf("status=%s", got)
	}
	if got := strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain", "--untracked-files=no")); got != "" {
		t.Errorf("rollback left dirty tree: %s", got)
	}
}

func TestPulseStampedCloseCommitFailurePreservesResourcesForRetry(t *testing.T) {
	root := seedCloseFixture(t, "tele", "finish", pulseWorkflow, run.StatusInProgress)
	writePulseGate(t, root, "tele", "finish", `{"status":"ok"}`)
	installCloseResourceHooks(t, root, "tele")
	resources := allocateCloseResources(t, root, "tele", "finish")
	before := readPulseCanvas(t, root, "tele", "finish")
	allow := failCommits(t, root)
	if err := disposePulseRunReason(root, "tele", "finish", "test disposal", io.Discard); err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	resources.assertRetained(t)
	if got := readPulseCanvas(t, root, "tele", "finish"); got != before {
		t.Errorf("canvas not restored: %q", got)
	}
	if got := loadRun(t, root, "tele", "finish").Status; got != run.StatusInProgress {
		t.Errorf("status=%s", got)
	}
	if got := strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain", "--untracked-files=no")); got != "" {
		t.Errorf("rollback left dirty tree: %s", got)
	}
	allow()
	if err := disposePulseRunReason(root, "tele", "finish", "test disposal", io.Discard); err != nil {
		t.Fatal(err)
	}
	resources.assertReleased(t)
	assertCloseRecorded(t, root, "tele", "finish", pulseWorkflow)
}

func TestPulseQuotaSkipReleasesResourcesAndRecordsRetry(t *testing.T) {
	root := seedCloseFixture(t, "tele", "finish", pulseWorkflow, run.StatusInProgress)
	writePulseGate(t, root, "tele", "finish", `{"status":"ok"}`)
	installCloseResourceHooks(t, root, "tele")
	resources := allocateCloseResources(t, root, "tele", "finish")
	q, err := newPulseQuotaRetries(root, "tele")
	if err != nil {
		t.Fatal(err)
	}
	q.pulse = "finish"
	retryAt := time.Now().UTC().Add(time.Hour)
	err = q.record(quotaDeferral{retryAt: retryAt, target: run.QuotaTarget{Project: "tele", Run: "finish", Stage: pulseDoc, Backend: "claude", Reason: "quota exhausted"}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	resources.assertReleased(t)
	assertCloseRecorded(t, root, "tele", "finish", pulseWorkflow)
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if retry, ok := idx.QuotaRetries["tele/finish"]; !ok || !retry.NotBefore.Equal(retryAt) {
		t.Fatalf("quota retry lost: %+v", idx.QuotaRetries)
	}
	if body := readPulseCanvas(t, root, "tele", "finish"); !strings.Contains(body, "## Quota deferrals") || !strings.Contains(body, "quota exhausted") {
		t.Fatalf("skip note lost: %s", body)
	}
}

func TestPulseSurveyResourceLifecycle(t *testing.T) {
	for _, scenario := range []string{"static", "dynamic", "skip", "invalid", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := newTestBureaucracy(t)
			markBureaucracy(t, root)
			trailerstest.SeedProject(t, root, "moe")
			installCloseResourceHooks(t, root, "moe")
			mode := rideDynamic
			if scenario == "dynamic" {
				mode = rideDynamic
			}
			defer withRideMode(mode)()
			var resources closeResources
			orig := openPulse
			t.Cleanup(func() { openPulse = orig })
			openPulse = func(projectID, runID string, _ bool, pi *pulseInterrupt, _, _ io.Writer) surveyOutcome {
				resources = allocateCloseResources(t, root, projectID, runID)
				if scenario == "skip" {
					pi.mark()
					return surveyOutcome{code: 1}
				}
				if scenario == "failed" {
					return surveyOutcome{code: 1, agentStarted: true}
				}
				gate := `{"status":"ok"}`
				if scenario == "invalid" {
					gate = `{"unknown":true}`
				}
				writePulseGate(t, root, projectID, runID, gate)
				return surveyOutcome{code: 0, agentStarted: true}
			}
			var log bytes.Buffer
			code := runPulseSurvey(root, "moe", "", newTestPulseInterrupt(t), io.Discard, &log)
			if scenario == "invalid" || scenario == "failed" {
				if code == 0 {
					t.Fatalf("invalid/failed survey succeeded: %s", &log)
				}
				resources.assertRetained(t)
				if len(openPulseRuns(t, root, "moe")) != 1 {
					t.Fatal("failed survey closed")
				}
				return
			}
			if code != 0 {
				t.Fatalf("survey exit=%d: %s", code, &log)
			}
			resources.assertReleased(t)
			closed := closedPulseRuns(t, root)
			if len(closed) != 1 {
				t.Fatalf("closed=%v", closed)
			}
			assertCloseRecorded(t, root, "moe", closed[0], pulseWorkflow)
			body := readPulseCanvas(t, root, "moe", closed[0])
			if scenario == "dynamic" && !strings.Contains(body, "## Next") {
				t.Fatalf("next section lost: %s", body)
			}
			if scenario == "skip" && !strings.Contains(body, "Sweep skipped") {
				t.Fatalf("skip note lost: %s", body)
			}
		})
	}
}

func TestPulseStampedCloseStampFailurePreservesResources(t *testing.T) {
	for _, failure := range []string{"write", "stage"} {
		t.Run(failure, func(t *testing.T) {
			if failure == "write" && os.Geteuid() == 0 {
				t.Skip("root ignores the write bit")
			}
			root := seedCloseFixture(t, "tele", "finish", pulseWorkflow, run.StatusInProgress)
			writePulseGate(t, root, "tele", "finish", `{"status":"ok"}`)
			installCloseResourceHooks(t, root, "tele")
			resources := allocateCloseResources(t, root, "tele", "finish")
			before := readPulseCanvas(t, root, "tele", "finish")
			canvas := filepath.Join(root, run.ContentPath("tele", "finish", pulseDoc))
			lock := filepath.Join(root, ".git", "index.lock")
			rewrite := func(body []byte) []byte {
				if failure == "stage" {
					writeFile(t, lock, "held\n")
				}
				return append(body, []byte("\nStamped\n")...)
			}
			if failure == "write" {
				if err := os.Chmod(canvas, 0o444); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(canvas, 0o644) })
			}
			err := closePulseRunStamped(root, "tele", "finish", "test", rewrite, io.Discard)
			if failure == "stage" {
				if removeErr := os.Remove(lock); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			if err == nil {
				t.Fatal("stamp unexpectedly succeeded")
			}
			resources.assertRetained(t)
			if got := readPulseCanvas(t, root, "tele", "finish"); got != before {
				t.Errorf("canvas not restored: %q", got)
			}
			if got := loadRun(t, root, "tele", "finish").Status; got != run.StatusInProgress {
				t.Errorf("status=%s", got)
			}
			if got := strings.TrimSpace(gittest.Output(t, root, "status", "--porcelain", "--untracked-files=no")); got != "" {
				t.Errorf("rollback left dirty tree: %s", got)
			}
		})
	}
}

func TestPulseRealStageAllocatesAndReleasesResources(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "claude")
	parent := installCloseResourceHooks(t, root, "tele")
	fakeClaudeOnPath(t, `#!/bin/sh
set -eu
prompt=
next=0
for a in "$@"; do
 if [ "$next" = "1" ]; then prompt=$a; next=0; fi
 case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
fence=$(printf '\140\140\140')
printf '# Pulse\n\n## Gate\n\n%sjson\n{"status":"ok"}\n%s\n' "$fence" "$fence" > "$canvas"
`)
	var log bytes.Buffer
	defer withRideMode(rideDynamic)()
	if code := runPulseSurvey(root, "tele", "", nil, &log, &log); code != 0 {
		t.Fatalf("real survey exit=%d: %s", code, &log)
	}
	closed := closedPulseRuns(t, root)
	if len(closed) != 1 {
		t.Fatalf("closed=%v: %s", closed, &log)
	}
	// The survey reads canonical checkouts, so it allocates no clone and
	// runs no dev-env setup.
	if _, err := os.Stat(filepath.Join(parent, "setup")); !os.IsNotExist(err) {
		t.Fatalf("survey ran dev-env setup: %v", err)
	}
	if _, err := os.Stat(sandbox.Path(root, "tele", closed[0])); !os.IsNotExist(err) {
		t.Fatalf("survey allocated a clone: %v", err)
	}
	assertCloseRecorded(t, root, "tele", closed[0], pulseWorkflow)
}

func TestChatCloseReopenRecreatesResourcesAndPreservesHistory(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	if _, err := run.New(root, "tele", run.Options{ID: "ponder", Workflow: chatWorkflow}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "claude")
	t.Setenv("MOE_SERVE_AGENT", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	parent := installCloseResourceHooks(t, root, "tele")
	// Chat's canvas is harness-authored. The fake just proves its cached env
	// is reachable from the agent process, and supplies a transcript to retain.
	fakeClaudeOnPath(t, `#!/bin/sh
set -eu
test -d "$TEST_CLOSE_ROOT"
sid=
next=0
for a in "$@"; do
 if [ "$next" = "1" ]; then sid=$a; next=0; fi
 case "$a" in --session-id|--resume) next=1 ;; esac
done
test -n "$sid"
bucket=$(printf '%s' "$PWD" | sed 's#[/.]#-#g')
mkdir -p "$CLAUDE_CONFIG_DIR/projects/$bucket"
printf '{"type":"user","message":{"role":"user","content":"conversation survives close"}}\n' >> "$CLAUDE_CONFIG_DIR/projects/$bucket/$sid.jsonl"
`)
	var log bytes.Buffer
	var previousCanvas, previousThread string
	var previousRoot string
	for turn := 0; turn < 2; turn++ {
		if code := Run([]string{"chat", "chat", "--agent", "claude", "tele/ponder"}, &log, &log); code != 0 {
			t.Fatalf("chat turn %d exit=%d: %s", turn, code, &log)
		}
		canvas := readChatCanvas(t, root, loadRun(t, root, "tele", "ponder"))
		threadPath := filepath.Join(root, run.DocDir("tele", "ponder", chatDoc), "thread-claude.jsonl")
		if previousCanvas != "" && !strings.Contains(canvas, previousCanvas) {
			t.Fatalf("old canvas lost: %s", canvas)
		}
		body, err := os.ReadFile(threadPath)
		if err != nil || !strings.Contains(string(body), "conversation survives close") || !strings.HasPrefix(string(body), previousThread) {
			t.Fatalf("transcript lost: %q, %v", body, err)
		}
		previousThread = string(body)
		previousCanvas = canvas
		clone := sandbox.Path(root, "tele", "ponder")
		env, ok, err := devEnvLoadCache(clone)
		if err != nil || !ok {
			t.Fatalf("new chat cache: %v, %v", ok, err)
		}
		owned := env["TEST_CLOSE_ROOT"]
		if owned == previousRoot {
			t.Fatal("reopen reused released dev root")
		}
		previousRoot = owned
		if code := Run([]string{"chat", "close", "tele/ponder"}, &log, &log); code != 0 {
			t.Fatalf("chat close exit=%d: %s", code, &log)
		}
		(closeResources{clone, owned, filepath.Join(parent, "teardown"), filepath.Join(parent, "sibling")}).assertReleased(t)
		assertCloseRecorded(t, root, "tele", "ponder", chatWorkflow)
	}
	setup, err := os.ReadFile(filepath.Join(parent, "setup"))
	if err != nil || len(strings.Fields(string(setup))) != 2 {
		t.Fatalf("setup should run once per sitting: %q, %v", setup, err)
	}
}

func TestWorkflowCloseKeepsNamedWorkspaceWarm(t *testing.T) {
	for _, workflow := range []string{"sdlc", "pulse", "chat"} {
		t.Run(workflow, func(t *testing.T) {
			root := stageWorkspaceFixture(t, "--workspace", "tele/dev")
			parent := installCloseResourceHooks(t, root, "tele")
			md := loadRun(t, root, "tele", "fix-it")
			md.Workflow = workflow
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "add", run.Dir("tele", "fix-it"))
			gittest.Run(t, root, "commit", "--allow-empty", "-m", "test: bind workflow to warm workspace")
			tree, err := attachTestRunWorkspace(t, root, md, branchPrefix+md.ID)
			if err != nil {
				t.Fatal(err)
			}
			env, _, err := devEnvSetupEnv(root, tree, md, io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			cache, err := os.ReadFile(filepath.Join(tree, devEnvCacheRel))
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			if code := Run([]string{workflow, "close", "tele/fix-it"}, &log, &log); code != 0 {
				t.Fatalf("close exit=%d: %s", code, &log)
			}
			if _, err := os.Stat(env["TEST_CLOSE_ROOT"]); err != nil {
				t.Errorf("named dev root removed: %v", err)
			}
			if after, err := os.ReadFile(filepath.Join(tree, devEnvCacheRel)); err != nil || !bytes.Equal(after, cache) {
				t.Errorf("warm cache changed: %s, %v", after, err)
			}
			if _, err := os.Stat(filepath.Join(parent, "teardown")); !os.IsNotExist(err) {
				t.Errorf("named close ran teardown: %v", err)
			}
			if claim, err := workspace.ReadClaim(root, "tele", "dev"); err != nil || claim != nil {
				t.Errorf("claim not released: %+v, %v", claim, err)
			}
			if got := currentBranch(t, tree); got != "main" {
				t.Errorf("workspace not parked: %s", got)
			}
			assertCloseRecorded(t, root, "tele", "fix-it", workflow)
		})
	}
}
