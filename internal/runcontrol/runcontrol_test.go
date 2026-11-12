package runcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/session"
)

func seedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	write(t, root, "bureaucracy.conf", "")
	write(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed bureaucracy")
	if _, err := run.New(root, "moe", run.Options{ID: "r1", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, root string) File {
	t.Helper()
	f, err := Load(root, "moe", "r1")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func submit(t *testing.T, root string, r Request) Request {
	t.Helper()
	got, err := Submit(root, "moe", "r1", r)
	if err != nil {
		t.Fatalf("Submit(%+v): %v", r, err)
	}
	return got
}

func head(t *testing.T, root string) string {
	t.Helper()
	return strings.TrimSpace(gittest.Output(t, root, "rev-parse", "HEAD"))
}

// own holds the run's guard the way a running stage would.
func own(t *testing.T, root string) *runowner.Handle {
	t.Helper()
	h, err := runowner.Acquire(root, "moe", "r1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release() })
	return h
}

func setStatus(t *testing.T, root, status string) {
	t.Helper()
	md, err := run.Load(root, "moe", "r1")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// A pause sets the hold at acceptance — before any owner boundary — as
// one commit carrying the run's trailers and no consent. With an owner
// running, the receipt waits for that owner to settle.
func TestPauseHoldsAtAcceptance(t *testing.T) {
	root := seedRoot(t)
	own(t, root)
	r := submit(t, root, Request{ID: "p1", Kind: KindPause})
	if r.State != StateAccepted || r.AcceptedAt.IsZero() {
		t.Fatalf("request = %+v, want accepted with a time", r)
	}
	f := load(t, root)
	if !f.Held() || f.Hold.Request != "p1" || f.Hold.Kind != KindPause || f.Revision != 1 {
		t.Fatalf("record = %+v, want held by p1 at revision 1", f)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "control: pause p1 moe/r1") || !strings.Contains(msg, "MoE-Run: r1") {
		t.Fatalf("commit message:\n%s", msg)
	}
	if strings.Contains(msg, "MoE-Consent") {
		t.Fatalf("operator request carries consent:\n%s", msg)
	}
}

// With no owner there is no turn to settle: the hold is the whole
// outcome, so the receipt completes at once — and the hold stays.
func TestPauseOnUnownedRunCompletesAtOnce(t *testing.T) {
	root := seedRoot(t)
	r := submit(t, root, Request{ID: "h1", Kind: KindHandoff, Stage: "code", Agent: "codex"})
	if r.State != StateCompleted {
		t.Fatalf("state = %s, want completed", r.State)
	}
	f := load(t, root)
	if !f.Held() || f.Hold.Kind != KindHandoff || f.Hold.Stage != "code" || f.Hold.Agent != "codex" {
		t.Fatalf("hold = %+v, want a handoff to code/codex", f.Hold)
	}
}

// A second pause or handoff while held is already satisfied: it gets
// the standing hold back and moves the journal not at all.
func TestRepeatedHoldIsANoOp(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "p1", Kind: KindPause})
	before := head(t, root)
	got := submit(t, root, Request{ID: "h2", Kind: KindHandoff})
	if got.ID != "p1" {
		t.Fatalf("got %+v, want the standing hold p1", got)
	}
	if head(t, root) != before {
		t.Fatal("a satisfied hold made a commit")
	}
	if _, ok := load(t, root).Find("h2"); ok {
		t.Fatal("a satisfied hold was recorded")
	}
}

// A retry with the same id reads back the recorded request — even after
// the run has gone terminal — and the same id with different intent is
// a conflict.
func TestSameIDIsIdempotentAndConflictsOnNewIntent(t *testing.T) {
	root := seedRoot(t)
	first := submit(t, root, Request{ID: "x", Kind: KindPause})
	setStatus(t, root, run.StatusClosed)
	before := head(t, root)
	again := submit(t, root, Request{ID: "x", Kind: KindPause})
	if again != first {
		t.Fatalf("retry = %+v, want %+v", again, first)
	}
	if head(t, root) != before {
		t.Fatal("a retry made a commit")
	}
	if _, err := Submit(root, "moe", "r1", Request{ID: "x", Kind: KindFinish}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := Submit(root, "moe", "r1", Request{ID: "y", Kind: KindPause}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("new request on a closed run: err = %v, want ErrNotLive", err)
	}
}

// Park binds to a live owner: with none, there is nothing to yield.
func TestParkNeedsAnOwner(t *testing.T) {
	root := seedRoot(t)
	if _, err := Submit(root, "moe", "r1", Request{ID: "k1", Kind: KindPark}); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("err = %v, want ErrNoOwner", err)
	}
	own(t, root)
	r := submit(t, root, Request{ID: "k2", Kind: KindPark, Owner: "gen-7"})
	if r.State != StateAccepted || r.Owner != "gen-7" || load(t, root).Held() {
		t.Fatalf("park = %+v (held %v), want accepted, bound to gen-7, nothing held", r, load(t, root).Held())
	}
}

// A hold wins over finish: finish is refused while held, and a hold
// accepted after a finish supersedes it. Resume does not resurrect it.
func TestHoldWinsOverFinish(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "f1", Kind: KindFinish})
	submit(t, root, Request{ID: "p1", Kind: KindPause})
	f1, _ := load(t, root).Find("f1")
	if f1.State != StateCompleted || !strings.Contains(f1.Outcome, "superseded by pause p1") {
		t.Fatalf("finish = %+v, want superseded", f1)
	}
	if _, err := Submit(root, "moe", "r1", Request{ID: "f2", Kind: KindFinish}); !errors.Is(err, ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
	if _, _, err := Resume(root, "moe", "r1", own(t, root), Request{ID: "r"}); err != nil {
		t.Fatal(err)
	}
	if open := load(t, root).Open(KindFinish); len(open) != 0 {
		t.Fatalf("open finishes after resume = %+v, want none", open)
	}
}

// An applied finish is the owner's to settle: a later hold leaves it
// for that owner's next boundary rather than rewriting it.
func TestHoldLeavesAppliedFinishToItsOwner(t *testing.T) {
	root := seedRoot(t)
	own(t, root)
	submit(t, root, Request{ID: "f1", Kind: KindFinish})
	if err := Settle(root, "moe", "r1", "f1", Settlement{State: StateApplied, Boundary: "stage test"}, "dynamic"); err != nil {
		t.Fatal(err)
	}
	submit(t, root, Request{ID: "p1", Kind: KindPause})
	if f1, _ := load(t, root).Find("f1"); f1.State != StateApplied {
		t.Fatalf("finish = %+v, want still applied", f1)
	}
}

// A repeated finish while one is open is that one; a pushed run with
// nothing repairing it has already shipped.
func TestFinishRules(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "f1", Kind: KindFinish})
	before := head(t, root)
	if got := submit(t, root, Request{ID: "f2", Kind: KindFinish}); got.ID != "f1" {
		t.Fatalf("got %+v, want the open f1", got)
	}
	if head(t, root) != before {
		t.Fatal("a repeated finish made a commit")
	}

	root = seedRoot(t)
	setStatus(t, root, run.StatusPushed)
	if _, err := Submit(root, "moe", "r1", Request{ID: "f1", Kind: KindFinish}); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("idle pushed: err = %v, want ErrNoOwner", err)
	}
	own(t, root) // a CI repair holds the guard
	submit(t, root, Request{ID: "f2", Kind: KindFinish})
	// And a pushed run may be held against future repair.
	submit(t, root, Request{ID: "p1", Kind: KindPause})
}

// Settling records the receipt once; a repeat is a no-op, and an
// unknown outcome raises recovery, which blocks finish until resume.
func TestSettleAndRecovery(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "f1", Kind: KindFinish})
	s := Settlement{State: StateApplied, Boundary: "stage code"}
	if err := Settle(root, "moe", "r1", "f1", s, "dynamic"); err != nil {
		t.Fatal(err)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Consent: dynamic") {
		t.Fatalf("settlement lost the walk's consent:\n%s", msg)
	}
	before := head(t, root)
	if err := Settle(root, "moe", "r1", "f1", s, "dynamic"); err != nil {
		t.Fatal(err)
	}
	if head(t, root) != before {
		t.Fatal("a repeated settlement made a commit")
	}
	if err := Settle(root, "moe", "r1", "f1", Settlement{State: StateFailed, Boundary: "push",
		Outcome: "owner exited after dispatch; ship outcome unknown", Recovery: true}, "dynamic"); err != nil {
		t.Fatal(err)
	}
	f := load(t, root)
	if f.Recovery == nil || f.Recovery.Request != "f1" {
		t.Fatalf("recovery = %+v, want f1", f.Recovery)
	}
	if _, err := Submit(root, "moe", "r1", Request{ID: "f2", Kind: KindFinish}); !errors.Is(err, ErrRecovery) {
		t.Fatalf("err = %v, want ErrRecovery", err)
	}
	_, released, err := Resume(root, "moe", "r1", own(t, root), Request{ID: "r"})
	if err != nil || !released {
		t.Fatalf("Resume = %v, %v; want released", released, err)
	}
	f = load(t, root)
	if f.Recovery != nil {
		t.Fatalf("recovery survived resume: %+v", f.Recovery)
	}
	if f1, _ := f.Find("f1"); f1.State != StateFailed {
		t.Fatalf("resume restored finish authority: %+v", f1)
	}
}

// Resume is an act of admission: it needs this run's guard, no
// unresolved session, and the hold revision it was shown.
func TestResumeGuards(t *testing.T) {
	root := seedRoot(t)
	h := own(t, root) // the paused owner, which then exits without settling
	submit(t, root, Request{ID: "p1", Kind: KindPause})

	if _, _, err := Resume(root, "moe", "r1", nil, Request{ID: "r0"}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("no guard: err = %v, want ErrNotOwner", err)
	}
	if _, _, err := Resume(root, "moe", "r1", h, Request{ID: "r1", Revision: 99}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: err = %v, want ErrStale", err)
	}
	gittest.Run(t, root, "branch", session.BranchName("moe", "r1", "code"))
	if _, _, err := Resume(root, "moe", "r1", h, Request{ID: "r2"}); err == nil || !strings.Contains(err.Error(), "unresolved sessions") {
		t.Fatalf("reserved: err = %v, want unresolved sessions", err)
	}
	gittest.Run(t, root, "branch", "-D", session.BranchName("moe", "r1", "code"))
	if !load(t, root).Held() {
		t.Fatal("a refused resume released the hold")
	}

	got, released, err := Resume(root, "moe", "r1", h, Request{ID: "r3", Revision: 1})
	if err != nil || !released {
		t.Fatalf("Resume = %v, %v", released, err)
	}
	f := load(t, root)
	if f.Held() || f.Revision != 2 {
		t.Fatalf("record = %+v, want released at revision 2", f)
	}
	if p1, _ := f.Find("p1"); p1.State != StateCompleted || !strings.Contains(p1.Outcome, "resume r3") {
		t.Fatalf("pause receipt = %+v, want completed by resume", p1)
	}
	if got.State != StateCompleted || !strings.Contains(got.Outcome, "released pause p1") {
		t.Fatalf("resume = %+v", got)
	}

	// Releasing what is already released moves nothing; a retried id
	// reads back its receipt.
	before := head(t, root)
	if _, released, err := Resume(root, "moe", "r1", h, Request{ID: "r4"}); err != nil || released {
		t.Fatalf("idle Resume = %v, %v; want no-op", released, err)
	}
	again, released, err := Resume(root, "moe", "r1", h, Request{ID: "r3", Revision: 1})
	if err != nil || released || again != got {
		t.Fatalf("retried Resume = %+v, %v, %v", again, released, err)
	}
	if head(t, root) != before {
		t.Fatal("a no-op resume made a commit")
	}
}

// A malformed record refuses every write and reads as an error, never
// as "no hold".
func TestMalformedRefusesMotion(t *testing.T) {
	root := seedRoot(t)
	write(t, root, Path("moe", "r1"), `{"revision":1,"hold":{"request":"ghost","kind":"pause"},"requests":[]}`)
	if _, err := Load(root, "moe", "r1"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Load err = %v, want ErrMalformed", err)
	}
	if _, err := Submit(root, "moe", "r1", Request{ID: "p", Kind: KindPause}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Submit err = %v, want ErrMalformed", err)
	}
}

// A commit that fails acknowledges nothing and leaves no trace: the
// record and index are as they were, so the request cannot become an
// invisible grant, and a retry with the same id then succeeds.
func TestFailedCommitRestores(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "p1", Kind: KindPause})
	before, err := os.ReadFile(filepath.Join(root, Path("moe", "r1")))
	if err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	write(t, hooks, "pre-commit", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	h := own(t, root)
	if _, _, err := Resume(root, "moe", "r1", h, Request{ID: "r1"}); err == nil {
		t.Fatal("Resume succeeded through a failing commit")
	}
	after, err := os.ReadFile(filepath.Join(root, Path("moe", "r1")))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("record changed by a failed commit:\n%s", after)
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("tree dirty after a failed commit:\n%s", st)
	}

	// A first-ever record is removed again rather than left untracked.
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	if _, err := run.New(root, "moe", run.Options{ID: "r2", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	if _, err := Submit(root, "moe", "r2", Request{ID: "p", Kind: KindPause}); err == nil {
		t.Fatal("Submit succeeded through a failing commit")
	}
	if _, err := os.Stat(filepath.Join(root, Path("moe", "r2"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first record survived a failed commit: %v", err)
	}

	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	if _, released, err := Resume(root, "moe", "r1", h, Request{ID: "r1"}); err != nil || !released {
		t.Fatalf("retry Resume = %v, %v", released, err)
	}
}

// The record is read inside the lock: a writer that landed between a
// caller's read and its acquire is built on, not clobbered.
func TestWritesReadInsideTheLock(t *testing.T) {
	root := seedRoot(t)
	own(t, root)
	submit(t, root, Request{ID: "k1", Kind: KindPark})
	testLocked = func() {
		testLocked = nil
		// Stand in for another process's write between our read and lock.
		f := load(t, root)
		f.Requests = append(f.Requests, Request{ID: "other", Kind: KindPark, State: StateAccepted})
		if err := writeFile(filepath.Join(root, Path("moe", "r1")), f); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testLocked = nil })
	submit(t, root, Request{ID: "p1", Kind: KindPause})
	f := load(t, root)
	for _, id := range []string{"k1", "other", "p1"} {
		if _, ok := f.Find(id); !ok {
			t.Fatalf("request %s lost; record = %+v", id, f)
		}
	}
}
