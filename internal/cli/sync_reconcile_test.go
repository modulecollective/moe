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
	logPath string
}

// Lines returns one entry per `gh api --method DELETE` invocation
// captured so far — each is the path argument, verbatim.
func (s *deleteSpy) Lines() []string {
	if s == nil || s.logPath == "" {
		return nil
	}
	b, err := os.ReadFile(s.logPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// fakeGh installs a shim gh on PATH that dispatches on argv. state
// maps PR URL → JSON body for `gh pr view`. The returned deleteSpy
// lets the test inspect every `gh api --method DELETE` path after
// the exercised code runs.
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
	script := `#!/bin/sh
set -e
hash_url() {
  printf '%s' "$1" | ` + shaCmd() + ` | cut -d' ' -f1
}
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
	return &deleteSpy{logPath: logPath}
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
