package git

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestStatus_SpacesAndRename pins down two facts that the project
// relies on: paths with spaces round-trip verbatim (no core-quoting,
// no octal escapes), and `-z` rename records arrive as NEW-then-OLD
// (the reverse of the human `R old -> new` form).
func TestStatus_SpacesAndRename(t *testing.T) {
	dir := gittest.Init(t)

	// Seed a tracked file with a space, then commit so we have a HEAD.
	writeFile(t, filepath.Join(dir, "kept name.txt"), "kept\n")
	gittest.Run(t, dir, "add", ".")
	gittest.Run(t, dir, "commit", "-m", "init")

	// Rename it through git so the rename record is staged.
	gittest.Run(t, dir, "mv", "kept name.txt", "renamed name.txt")
	// Drop an untracked file with a space so the ?? path is exercised too.
	writeFile(t, filepath.Join(dir, "stray two.txt"), "stray\n")

	entries, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	// Order is git's, not ours; sort by Path for a stable assert.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	want := []StatusEntry{
		{XY: "R ", Path: "renamed name.txt", From: "kept name.txt"},
		{XY: "??", Path: "stray two.txt"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
}

// TestStatus_CleanTreeReturnsNil confirms a clean working tree
// produces zero entries (and a nil slice, so callers can use
// len() == 0 as the dirty check).
func TestStatus_CleanTreeReturnsNil(t *testing.T) {
	dir := gittest.Init(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	gittest.Run(t, dir, "add", ".")
	gittest.Run(t, dir, "commit", "-m", "init")

	entries, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if entries != nil {
		t.Fatalf("clean tree should return nil entries, got %#v", entries)
	}
}

// TestStatus_PathsScope confirms scoping limits results to the
// requested pathspec.
func TestStatus_PathsScope(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub", "in.txt"), "in\n")
	writeFile(t, filepath.Join(dir, "out.txt"), "out\n")

	entries, err := Status(dir, "sub")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "sub/in.txt" {
		t.Fatalf("scoped Status got %#v, want only sub/in.txt", entries)
	}
}

// TestRun_RetriesIndexLockUntilCleared confirms Run waits out a busy
// index.lock and succeeds once another process releases it — the
// steady-state half of the moe/hunk worktree contention fix.
func TestRun_RetriesIndexLockUntilCleared(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	// Shrink the retry envelope so the test stays sub-second. The
	// production cap (2s) is policy, not behaviour we want to assert
	// here — we only need to prove the retry/clear/succeed path.
	withIndexLockTiming(t, 500*time.Millisecond, 10*time.Millisecond)

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	// Clear the lock partway through Run's retry loop. 80ms is well
	// inside the 500ms budget but past several 10ms ticks, so we know
	// the success came from a retry (not the first attempt).
	cleared := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.Remove(lock)
		close(cleared)
	}()

	if err := Run(dir, "commit", "--allow-empty", "-m", "after-lock"); err != nil {
		t.Fatalf("Run after lock cleared: %v", err)
	}
	<-cleared
}

// TestRun_IndexLockExhaustsCap confirms Run gives up at the cap and
// surfaces git's verbatim stderr — the operator-facing half of the
// fix, so a genuinely stuck lock from a crashed prior run doesn't
// disappear behind a synthetic error message.
func TestRun_IndexLockExhaustsCap(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	withIndexLockTiming(t, 80*time.Millisecond, 10*time.Millisecond)

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	start := time.Now()
	err := Run(dir, "commit", "--allow-empty", "-m", "should-fail")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run with persistent lock should fail")
	}
	if !strings.Contains(err.Error(), indexLockSubstr) {
		t.Fatalf("error should preserve git's verbatim stderr: %v", err)
	}
	// At least one retry tick elapsed: cap (80ms) — proof the loop
	// actually retried before giving up. Upper bound is loose; CI
	// jitter shouldn't cause flakes.
	if elapsed < 80*time.Millisecond {
		t.Fatalf("Run returned in %v, expected to wait out cap", elapsed)
	}
}

// TestOutput_RetriesIndexLock proves read-side primitives also retry
// when the index lock is contended — the split-cap design lives or
// dies on this. Output uses the shorter read cap; we shrink both to
// keep the test fast.
func TestOutput_RetriesIndexLock(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	withIndexLockTiming(t, 500*time.Millisecond, 10*time.Millisecond)

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.Remove(lock)
	}()

	// `status` reaches for the index, so it races the lock just like a
	// write would. Output is the primitive Status uses internally.
	if _, err := Output(dir, "status", "--porcelain"); err != nil {
		t.Fatalf("Output after lock cleared: %v", err)
	}
}

// TestProbe_ExitCodeAnswer covers Probe's contract: exit 0 → true,
// non-zero → false, output suppressed regardless.
func TestProbe_ExitCodeAnswer(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	// HEAD exists → rev-parse --verify --quiet succeeds.
	if !Probe(dir, "rev-parse", "--verify", "--quiet", "HEAD") {
		t.Fatalf("Probe HEAD should be true")
	}
	// A bogus ref does not.
	if Probe(dir, "rev-parse", "--verify", "--quiet", "refs/heads/does-not-exist") {
		t.Fatalf("Probe missing ref should be false")
	}
}

// TestHasRef wraps the same shape as TestProbe_ExitCodeAnswer but at
// the typed-wrapper layer — pins the convenience surface callers will
// actually use.
func TestHasRef(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	if !HasRef(dir, "HEAD") {
		t.Fatalf("HasRef HEAD should be true after init commit")
	}
	if HasRef(dir, "refs/heads/missing") {
		t.Fatalf("HasRef missing branch should be false")
	}
}

// TestUpstream_NoUpstreamReturnsEmpty confirms a fresh branch with no
// @{u} returns "" rather than an error — the contract sync.HasUpstream
// callers depend on.
func TestUpstream_NoUpstreamReturnsEmpty(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	got, err := Upstream(dir)
	if err != nil {
		t.Fatalf("Upstream on fresh branch: %v", err)
	}
	if got != "" {
		t.Fatalf("Upstream = %q, want \"\"", got)
	}
}

// TestUpstream_ReturnsConfiguredRef confirms a configured upstream
// round-trips. We point at the same repo via a bare clone so the test
// doesn't depend on the network.
func TestUpstream_ReturnsConfiguredRef(t *testing.T) {
	bare := gittest.InitBare(t)

	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")
	gittest.Run(t, dir, "remote", "add", "origin", bare)
	gittest.Run(t, dir, "push", "-u", "origin", "HEAD")

	got, err := Upstream(dir)
	if err != nil {
		t.Fatalf("Upstream: %v", err)
	}
	if !strings.HasPrefix(got, "origin/") {
		t.Fatalf("Upstream = %q, want something under origin/", got)
	}
}

// TestAheadOf_Counts confirms AheadOf returns the rev-list count when
// base and head both exist.
func TestAheadOf_Counts(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "base")
	gittest.Run(t, dir, "checkout", "-b", "feat")
	gittest.Commit(t, dir, "a")
	gittest.Commit(t, dir, "b")

	// gittest pins init.defaultBranch=main, so `main` is the trunk
	// regardless of host git version. AheadOf swallows missing-ref
	// errors (TestAheadOf_UnknownBase pins that), so the test can't
	// fall through to a default-branch detection — it has to name the
	// branch it knows exists.
	n, err := AheadOf(dir, "main", "feat")
	if err != nil {
		t.Fatalf("AheadOf: %v", err)
	}
	if n != 2 {
		t.Fatalf("AheadOf = %d, want 2", n)
	}
}

// TestAheadOf_UnknownBase confirms AheadOf swallows rev-list failures
// and returns (0, nil) — the contract CheckBranchHasCommits depends on.
func TestAheadOf_UnknownBase(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	n, err := AheadOf(dir, "refs/heads/does-not-exist", "HEAD")
	if err != nil {
		t.Fatalf("AheadOf with unknown base: %v", err)
	}
	if n != 0 {
		t.Fatalf("AheadOf with unknown base = %d, want 0", n)
	}
}

// TestLsRemoteDefault_BareRepo confirms LsRemoteDefault parses the
// symbolic HEAD out of `ls-remote --symref`, using a bare local repo
// as the URL so the test runs offline.
func TestLsRemoteDefault_BareRepo(t *testing.T) {
	src := gittest.Init(t)
	gittest.Commit(t, src, "init")

	// Determine src's default branch (could be `main` or `master`
	// depending on git defaults) and use it as the assertion target.
	want := gittest.Output(t, src, "rev-parse", "--abbrev-ref", "HEAD")

	bare := t.TempDir()
	gittest.Run(t, bare, "clone", "--bare", "-q", src, ".")

	got, err := LsRemoteDefault(bare)
	if err != nil {
		t.Fatalf("LsRemoteDefault: %v", err)
	}
	if got != want {
		t.Fatalf("LsRemoteDefault = %q, want %q", got, want)
	}
}

// TestHEAD_Sugar confirms HEAD returns the same SHA as RevParse("HEAD").
func TestHEAD_Sugar(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	want, err := RevParse(dir, "HEAD")
	if err != nil {
		t.Fatalf("RevParse HEAD: %v", err)
	}
	got, err := HEAD(dir)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if got != want {
		t.Fatalf("HEAD = %q, RevParse = %q", got, want)
	}
}

// TestStream_PassesThroughWriters confirms Stream's stdio reaches the
// writers we hand it (the property the interactive push/pull callers
// rely on).
func TestStream_PassesThroughWriters(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	var stdout, stderr bytes.Buffer
	if err := Stream(dir, &stdout, &stderr, "log", "-1", "--format=%s"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(stdout.String(), "init") {
		t.Fatalf("stdout = %q, want 'init' subject line", stdout.String())
	}
}

// TestHook_FiresOnRunAndStream confirms Hook is invoked once per
// underlying `exec.Command` attempt for both Run (capture path) and
// Stream (passthrough path) — the test seam value the design bundles
// in.
func TestHook_FiresOnRunAndStream(t *testing.T) {
	dir := gittest.Init(t)
	gittest.Commit(t, dir, "init")

	type call struct {
		args []string
		err  error
	}
	var calls []call
	prev := Hook
	Hook = func(_ string, args []string, _ time.Duration, err error) {
		calls = append(calls, call{args: append([]string(nil), args...), err: err})
	}
	t.Cleanup(func() { Hook = prev })

	if err := Run(dir, "commit", "--allow-empty", "-m", "after"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var buf bytes.Buffer
	if err := Stream(dir, &buf, &buf, "log", "-1", "--format=%H"); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("Hook fired %d times, want 2: %#v", len(calls), calls)
	}
	if calls[0].args[0] != "commit" {
		t.Fatalf("Hook[0] args[0] = %q, want commit", calls[0].args[0])
	}
	if calls[1].args[0] != "log" {
		t.Fatalf("Hook[1] args[0] = %q, want log", calls[1].args[0])
	}
	for i, c := range calls {
		if c.err != nil {
			t.Fatalf("Hook[%d] err = %v, want nil", i, c.err)
		}
	}
}

// withIndexLockTiming swaps in test-friendly retry bounds and restores
// the production values when the test ends. Both write and read caps
// move together: tests that exercise the retry loop care about the
// budget regardless of which primitive they call.
func withIndexLockTiming(t *testing.T, cap, step time.Duration) {
	t.Helper()
	pw, pr, ps := writeRetryCap, readRetryCap, indexLockRetryStep
	writeRetryCap = cap
	readRetryCap = cap
	indexLockRetryStep = step
	t.Cleanup(func() {
		writeRetryCap = pw
		readRetryCap = pr
		indexLockRetryStep = ps
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
