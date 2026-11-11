package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
)

// holdElsewhere takes a run's guard on its own descriptor, outside this
// invocation's table — to the code under test it is indistinguishable
// from another process holding the run.
func holdElsewhere(t *testing.T, root, projectID, runID string) *runowner.Handle {
	t.Helper()
	h, err := runowner.Acquire(root, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release() })
	return h
}

func TestOwnRunBorrowsTheSameRunAndAcquiresOthers(t *testing.T) {
	root := t.TempDir()
	outer, err := ownRun(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := ownRun(root, "p", "r")
	if err != nil {
		t.Fatalf("a same-run nested entry must borrow: %v", err)
	}
	if err := inner(); err != nil {
		t.Fatal(err)
	}
	if busy, _ := runowner.Busy(root, "p", "r"); !busy || heldRun(root, "p", "r") == nil {
		t.Fatal("releasing a borrow dropped the outer guard")
	}
	other, err := ownRun(root, "p", "other")
	if err != nil {
		t.Fatal(err)
	}
	if busy, _ := runowner.Busy(root, "p", "other"); !busy {
		t.Fatal("a different run was not acquired separately")
	}
	other()
	if err := outer(); err != nil {
		t.Fatal(err)
	}
	if busy, _ := runowner.Busy(root, "p", "r"); busy || heldRun(root, "p", "r") != nil {
		t.Fatal("outer release left the run owned")
	}
	// A guard held outside the table is another owner, never a borrow.
	holdElsewhere(t, root, "p", "r")
	if _, err := ownRun(root, "p", "r"); !runowner.IsBusy(err) {
		t.Fatalf("ownRun against another holder = %v, want busy", err)
	}
}

// The stage verbs, close and push all refuse a run another invocation is
// sitting in, before admission or any write.
func TestVerbsRefuseARunAnotherInvocationOwns(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedRun(t, root, "moe", "held", "sdlc", run.StatusInProgress, time.Now(), map[string]string{"design": "# d\n"})
	holdElsewhere(t, root, "moe", "held")
	head, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(stdout, stderr io.Writer) int{
		"stage": func(stdout, stderr io.Writer) int {
			return runStageSession("moe", "held", "code", stageSessionOpts{Headless: true}, stdout, stderr)
		},
		"close": func(stdout, stderr io.Writer) int {
			reg, _ := lookupCloseRegistration("sdlc")
			return runClose("sdlc", reg.subject, reg.cleanup, []string{"moe/held"}, stdout, stderr)
		},
		"push": func(stdout, stderr io.Writer) int {
			code, _ := runPushTyped("sdlc", []string{"--merge", "moe/held"}, stdout, stderr)
			return code
		},
		"chain-kick": func(stdout, stderr io.Writer) int {
			return chainKickRun(root, "moe", "held", rideStatic, false, stdout, stderr)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := call(io.Discard, &stderr); code == 0 {
				t.Fatalf("exit 0 against an owned run; stderr=%q", stderr.String())
			}
			if !strings.Contains(stderr.String(), "owned by another moe invocation") {
				t.Fatalf("stderr = %q, want the owner refusal", stderr.String())
			}
			if branches, err := session.RunReservations(root, "moe", "held"); err != nil || len(branches) != 0 {
				t.Fatalf("session reserved behind an owned run: %v %v", branches, err)
			}
			if after, _ := git.HEAD(root); after != head {
				t.Fatal("journal moved behind an owned run")
			}
		})
	}
}

// A sync that finds a pushed run owned elsewhere leaves it exactly as it
// was — status, clone, remote branch — and retries on the next sync.
func TestSyncDefersARunAnotherInvocationOwns(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	spy := fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})
	h := holdElsewhere(t, f.root, f.projectID, f.runID)

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if md := f.reload(); md.Status != run.StatusPushed {
		t.Fatalf("status = %s, want pushed while owned", md.Status)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatal("clone removed while owned")
	}
	if len(spy.Lines()) != 0 {
		t.Fatalf("remote branch deleted while owned: %v", spy.Lines())
	}
	if !strings.Contains(stderr.String(), "retry next sync") {
		t.Fatalf("stderr = %q, want the deferral", stderr.String())
	}

	h.Release()
	stdout.Reset()
	if _, err := reconcilePushedRuns(f.root, "", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if md := f.reload(); md.Status != run.StatusMerged {
		t.Fatalf("retry after release: status = %s, want merged", md.Status)
	}
}
