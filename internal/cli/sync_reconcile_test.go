package cli

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// sha1New is a thin alias so sha1Sum doesn't have to import crypto/sha1
// directly in each place it's called.
func sha1New() hash.Hash { return sha1.New() }

// deleteSpy records gh api DELETE calls the shim has handled. Call
// spy.Lines() after the code under test has run to inspect them.
type deleteSpy struct {
	logPath  string
	lockPath string
}

// Lines returns one entry per `gh api --method DELETE` invocation
// captured so far — each is the path argument, verbatim.
func (s *deleteSpy) Lines() []string {
	if s == nil {
		return nil
	}
	return readLogLines(s.logPath)
}

// LockProbes returns one entry per gh invocation, of the form
// "<verb> locked" or "<verb> unlocked", recording whether the
// bureaucracy's repolock file existed at the moment gh ran. Empty
// unless the test called probeLockDuringGh first.
func (s *deleteSpy) LockProbes() []string {
	if s == nil {
		return nil
	}
	return readLogLines(s.lockPath)
}

// probeLockDuringGh arms the shim's repolock probe against root. Set
// before the code under test runs; the shim reads the env var at exec
// time, so ordering against fakeGh doesn't matter.
func probeLockDuringGh(t *testing.T, root string) {
	t.Helper()
	t.Setenv("MOE_FAKE_GH_LOCK_ROOT", root)
}

// readLogLines reads one of the shim's append-only logs, dropping
// blanks. A missing file reads as no entries — "gh was never called"
// and "the probe was never armed" are both legitimately empty.
func readLogLines(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// fakeGh installs a shim gh on PATH that dispatches on argv. state
// maps PR URL → JSON body for `gh pr view`. The returned deleteSpy
// lets the test inspect every `gh api --method DELETE` path after
// the exercised code runs, and — once probeLockDuringGh has armed it —
// whether the repolock was held while each call ran.
//
// Two behaviours model GitHub rather than the caller. Deleting a ref
// the shim has already deleted fails the way GitHub does ("Reference
// does not exist", which DeleteRemoteBranch reads as success), so a
// test can exercise a repeated delete without a knob. And
// $MOE_FAKE_GH_HOOK, when set to a script, runs before every dispatch
// — that's the seam for modelling a concurrent actor mutating the
// bureaucracy underneath an in-flight gh call.
//
// The shim is a tiny shell script — no helper binary needed. Skipped
// on Windows; reconcile runs against the real gh there.
func fakeGh(t *testing.T, state map[string]string) *deleteSpy {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim gh fake only works on unix-y OSes")
	}
	dir := t.TempDir()
	// One file per URL, keyed by a hex digest of the URL so the shim
	// can look it up with a plain `cat $(hash)`. Avoids quoting/escape
	// concerns entirely.
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for url, body := range state {
		if err := os.WriteFile(filepath.Join(stateDir, urlKey(url)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "delete.log")
	lockPath := filepath.Join(dir, "lock.log")
	script := `#!/bin/sh
set -e
hash_url() {
  printf '%s' "$1" | ` + shaCmd() + ` | cut -d' ' -f1
}
if [ -n "$MOE_FAKE_GH_LOCK_ROOT" ]; then
  if [ -f "$MOE_FAKE_GH_LOCK_ROOT/.moe/lock" ]; then
    echo "$1 $2 locked" >> "` + lockPath + `"
  else
    echo "$1 $2 unlocked" >> "` + lockPath + `"
  fi
fi
if [ -n "$MOE_FAKE_GH_HOOK" ]; then
  sh "$MOE_FAKE_GH_HOOK"
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  key=$(hash_url "$3")
  path="` + stateDir + `/${key}"
  if [ ! -f "$path" ]; then
    echo "fake gh: no state for $3 (key=$key)" >&2
    exit 1
  fi
  cat "$path"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  # no open PRs by default — runPush then calls pr create
  echo "[]"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  # Print a stable fake URL so the caller captures and commits a trailer.
  echo "https://github.com/owner/repo/pull/99"
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "--method" ] && [ "$3" = "DELETE" ]; then
  if [ -f "` + logPath + `" ] && grep -qxF "$4" "` + logPath + `"; then
    echo "$4" >> "` + logPath + `"
    echo "gh: Reference does not exist (HTTP 422)" >&2
    exit 1
  fi
  echo "$4" >> "` + logPath + `"
  exit 0
fi
echo "fake gh: unsupported invocation: $*" >&2
exit 2
`
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	return &deleteSpy{logPath: logPath, lockPath: lockPath}
}

// urlKey returns the same hash the shim computes — sha1 hex of the
// raw URL bytes. Keeps Go and shell in lockstep.
func urlKey(url string) string {
	h := sha1Sum(url)
	return h
}

// sha1Sum is a tiny wrapper so we can hash in Go without dragging in
// a dependency. crypto/sha1 is stdlib.
func sha1Sum(s string) string {
	h := sha1New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// shaCmd picks the right cli: shasum on darwin, sha1sum on linux.
func shaCmd() string {
	if runtime.GOOS == "darwin" {
		return "shasum"
	}
	return "sha1sum"
}

// reconcileFixture sets up a bureaucracy with one pushed run carrying
// a MoE-PR trailer, plus a sandbox clone on disk so teardown assertions
// can confirm cleanup.
type reconcileFixture struct {
	t         *testing.T
	root      string
	projectID string
	runID     string
	prURL     string
}

func newReconcileFixture(t *testing.T, status string) *reconcileFixture {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	projectID := "tele"
	runID := "fix-it"
	prURL := "https://github.com/owner/repo/pull/7"

	// project.json for ghRepoSpec / deleteRemoteBranchForRun.
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "projects", projectID, "project.json"),
		`{"id":"`+projectID+`","remote":"https://github.com/owner/repo.git","default_branch":"main"}`+"\n")

	md := &run.Metadata{
		ID:        runID,
		Project:   projectID,
		Status:    status,
		Workflow:  "sdlc",
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", filepath.Join("projects", projectID, "project.json"),
		filepath.Join("projects", projectID, "runs", runID, "run.json"))
	gittest.Run(t, root, "commit", "-m", "Open run "+projectID+"/"+runID+"\n\nMoE-Run: "+runID+"\nMoE-Project: "+projectID+"\n")

	// push-record commit carrying the MoE-PR trailer — mirrors what
	// runPush writes after opening the PR.
	trailerstest.CommitTrailer(t, root, "push: "+projectID+"/"+runID,
		"MoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Document: push\nMoE-PR: "+prURL,
		time.Now().UTC())

	// Place a fake sandbox so the teardown has something to remove.
	clonePath := sandbox.Path(root, projectID, runID)
	if err := os.MkdirAll(clonePath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clonePath, "marker"), "")

	return &reconcileFixture{t: t, root: root, projectID: projectID, runID: runID, prURL: prURL}
}

// reload gets the current on-disk metadata.
func (f *reconcileFixture) reload() *run.Metadata {
	f.t.Helper()
	md, err := run.Load(f.root, f.projectID, f.runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return md
}

// TestReconcileTransitionsMerged: GitHub reports MERGED → run flips
// to StatusMerged, commit carries MoE-Merged trailer with the merge
// SHA, remote branch delete is issued, sandbox is gone.
func TestReconcileTransitionsMerged(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	spy := fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("reconcile: %v\nstderr=%s", err, stderr.String())
	}

	md := f.reload()
	if md.Status != run.StatusMerged {
		t.Fatalf("status: want merged, got %s", md.Status)
	}
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-Merged: abc1234deadbeef") {
		t.Fatalf("merge trailer missing:\n%s", body)
	}
	want := "fix-it: pushed -> merged (abc1234)\n"
	if stdout.String() != want {
		t.Fatalf("stdout: want %q, got %q", want, stdout.String())
	}
	if sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox should be removed")
	}
	// One-line-per-transition: exactly one delete issued.
	deletes := spy.Lines()
	if len(deletes) != 1 || !strings.HasSuffix(deletes[0], "/git/refs/heads/moe/fix-it") {
		t.Fatalf("expected one branch delete, got %v", deletes)
	}
}

// TestReconcileTransitionsClosed: CLOSED → StatusClosed, MoE-Closed
// carries the PR URL, cleanup happens the same as merged.
func TestReconcileTransitionsClosed(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	fakeGh(t, map[string]string{
		f.prURL: `{"state":"CLOSED","mergeCommit":null}`,
	})

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("reconcile: %v\nstderr=%s", err, stderr.String())
	}

	md := f.reload()
	if md.Status != run.StatusClosed {
		t.Fatalf("status: want closed, got %s", md.Status)
	}
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-Closed: "+f.prURL) {
		t.Fatalf("closed trailer missing:\n%s", body)
	}
	want := "fix-it: pushed -> closed\n"
	if stdout.String() != want {
		t.Fatalf("stdout: want %q, got %q", want, stdout.String())
	}
	if sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox should be removed")
	}
}

// TestReconcileOpenIsNoop: OPEN → nothing on stdout, status stays
// pushed, sandbox preserved, no commit added.
func TestReconcileOpenIsNoop(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	fakeGh(t, map[string]string{
		f.prURL: `{"state":"OPEN","mergeCommit":null}`,
	})

	before := lastCommitMessage(t, f.root)

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("reconcile: %v\nstderr=%s", err, stderr.String())
	}

	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout for open PR, got %q", stdout.String())
	}
	if md := f.reload(); md.Status != run.StatusPushed {
		t.Fatalf("status: want pushed (unchanged), got %s", md.Status)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox should be preserved for open PR")
	}
	if after := lastCommitMessage(t, f.root); after != before {
		t.Fatalf("no-op reconcile made a commit:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestReconcileSkipsNonPushedRuns: in_progress / merged / closed
// runs are left alone — only StatusPushed runs are queried.
func TestReconcileSkipsNonPushedRuns(t *testing.T) {
	f := newReconcileFixture(t, run.StatusMerged)
	// No fakeGh — any call would error out with exit 2 and the test
	// would fail. That's the assertion: we shouldn't call gh at all.

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("reconcile: %v\nstderr=%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected silence on non-pushed run, got %q", stdout.String())
	}
}

// TestReconcileAtPulseHoldsNoLockDuringGitHubCalls is the point of the
// two-phase walk. Every `gh` invocation the reconcile makes — the PR
// query and the branch delete — must run with the repolock free, so a
// UI action or a CLI verb landing mid-sweep isn't stuck behind half a
// second of network per pushed run. The transition itself still takes
// the lock; the assertion here is only about what gh saw.
//
// Driven through reconcileAtPulse rather than the walk directly,
// because the pulse is where the lock used to be: the walk called bare
// has never held one.
func TestReconcileAtPulseHoldsNoLockDuringGitHubCalls(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	spy := fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})
	probeLockDuringGh(t, f.root)

	var stdout, stderr bytes.Buffer
	reconcileAtPulse(f.root, f.projectID, nil /*pi*/, &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if md := f.reload(); md.Status != run.StatusMerged {
		t.Fatalf("status = %q, want merged — the walk never ran", md.Status)
	}

	probes := spy.LockProbes()
	if len(probes) == 0 {
		t.Fatal("no gh invocations recorded; the probe never armed")
	}
	for _, p := range probes {
		if strings.HasSuffix(p, " locked") {
			t.Errorf("gh ran with the repolock held: %q (all probes: %v)", p, probes)
		}
	}
	// Sanity: the walk really did do both legs outside the lock.
	if len(probes) != 2 {
		t.Errorf("probes = %v, want the pr view and the branch delete", probes)
	}
}

// TestReconcileRejudgesUnderTheLock pins the line the two-phase split
// is only safe because of. Phase one reads the status outside any
// window, so phase two must re-read run.json under the lock and bail
// when someone else already finalised the run. The shim plays the
// concurrent actor: its side-effect script flips the fixture's
// run.json to merged while gh is answering, which is exactly "another
// sync got there between my read and my lock."
//
// Without the re-judge this walk would harvest and commit a second
// transition on an already-merged run, and print a second stdout line
// for it.
func TestReconcileRejudgesUnderTheLock(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})

	// The concurrent actor, as a script the shim runs before it
	// answers: rewrite run.json to merged, in place, uncommitted.
	runJSON := filepath.Join(f.root, run.Dir(f.projectID, f.runID), "run.json")
	hook := filepath.Join(t.TempDir(), "finalise.sh")
	writeFile(t, hook, "#!/bin/sh\nsed -i.bak 's/\"pushed\"/\"merged\"/' "+runJSON+"\n")
	t.Setenv("MOE_FAKE_GH_HOOK", hook)

	before := lastCommitMessage(t, f.root)

	var stdout, stderr bytes.Buffer
	moved, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr)
	if err != nil {
		t.Fatalf("reconcile: %v\nstderr=%s", err, stderr.String())
	}

	if moved != 0 {
		t.Errorf("moved = %d, want 0 — the run was already finalised", moved)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want silence; the other actor owns the line", stdout.String())
	}
	if after := lastCommitMessage(t, f.root); after != before {
		t.Errorf("a second transition was committed:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestReconcileDeletesBranchBeforeTheFlip pins the reorder and its
// safety net together. The delete used to sit between harvest and
// commit; it now runs before the lock, so a transition that never
// lands still leaves a `pushed` run with no remote branch — the state
// the design accepted as recoverable.
//
// The fixture makes the harvest itself fail (a follow-up to fan out,
// with every commit refused), which is the case that discriminates:
// under the old order a failed harvest returned before the delete was
// ever issued. Then the retry re-issues the delete against a ref
// GitHub has already dropped, 422s, and DeleteRemoteBranch swallows it
// — the idempotency the whole reorder leans on — so the run still
// reaches merged with exactly one line of output.
func TestReconcileDeletesBranchBeforeTheFlip(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	spy := fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})
	writeFollowups(t, f.root, f.projectID, f.runID,
		"# Follow-ups\n\n- [ ] `cleanup-foo` — Clean up foo helper\n\n")
	unfail := failCommits(t, f.root)

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("a failed harvest should warn and continue, not error: %v", err)
	}
	if !strings.Contains(stderr.String(), "harvest failed") {
		t.Fatalf("stderr = %q, want the harvest failure named", stderr.String())
	}
	if md := f.reload(); md.Status != run.StatusPushed {
		t.Fatalf("status = %q, want pushed after the failed harvest", md.Status)
	}
	if got := spy.Lines(); len(got) != 1 {
		t.Fatalf("deletes after the failed attempt = %v, want exactly one issued before the flip", got)
	}

	unfail()
	stdout.Reset()
	stderr.Reset()
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err != nil {
		t.Fatalf("retry: %v\nstderr=%s", err, stderr.String())
	}
	if md := f.reload(); md.Status != run.StatusMerged {
		t.Fatalf("status = %q after retry, want merged", md.Status)
	}
	if want := "fix-it: pushed -> merged (abc1234)\n"; stdout.String() != want {
		t.Fatalf("stdout: want %q, got %q", want, stdout.String())
	}
	if strings.Contains(stderr.String(), "Reference does not exist") {
		t.Errorf("the 422 leaked to stderr as a warning:\n%s", stderr.String())
	}
	if got := spy.Lines(); len(got) != 2 {
		t.Errorf("deletes across both attempts = %v, want one per attempt", got)
	}
}

// TestSyncReconcilesWithoutNestingTheLock is the end-to-end guard on
// the caller split. `moe sync` used to wrap pull → bump → reconcile →
// push in one window; now the reconcile takes its own per-transition
// lock, so a sync that nested would fail hard with a NestedError
// rather than degrade. Also pins that the reconcile's commit still
// reaches origin on the same invocation, and that gh still ran unlocked
// when driven through the real verb.
func TestSyncReconcilesWithoutNestingTheLock(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	spy := fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})
	probeLockDuringGh(t, f.root)
	origin := initBureaucracyOriginAt(t, f.root)
	t.Setenv("MOE_HOME", f.root)
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"sync"}, &stdout, &stderr); code != 0 {
		t.Fatalf("moe sync exit=%d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "nested") {
		t.Fatalf("sync nested the repolock:\n%s", stderr.String())
	}

	if md := f.reload(); md.Status != run.StatusMerged {
		t.Fatalf("status = %q, want merged; stderr=%s", md.Status, stderr.String())
	}
	if !strings.Contains(stdout.String(), "fix-it: pushed -> merged") {
		t.Errorf("transition line missing from sync output:\n%s", stdout.String())
	}
	// The push at the tail of sync carries the transition to origin.
	head := gittest.Output(t, f.root, "rev-parse", "HEAD")
	if got := gittest.Output(t, origin, "rev-parse", "main"); got != head {
		t.Errorf("origin/main = %s, want the local HEAD %s", got, head)
	}
	for _, p := range spy.LockProbes() {
		if strings.HasSuffix(p, " locked") {
			t.Errorf("gh ran with the repolock held under `moe sync`: %q", p)
		}
	}
}
