package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// residueFixture plants the rule-5 shape the hook exists for: an
// unregistered session directory whose only survivors sit under
// projects/<p>/runs/<r>/… in a directory the host can't write — what a
// container writing as root through a bind mount leaves behind. The
// project exists in the bureaucracy so its hooks are eligible.
func residueFixture(t *testing.T, root, projectID string) (dir, locked string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission checks")
	}
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, ".moe", "worktrees", "00000000-0000-4000-8000-0000000000aa")
	locked = filepath.Join(dir, "projects", projectID, "runs", "r1", "documents", "test", "results")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, ".last-run.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockTree(dir) })
	return dir, locked
}

// unlockTree restores owner write so TempDir cleanup can remove what a
// failing case left behind.
func unlockTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func writeCleanupHook(t *testing.T, root, projectID, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "hooks", sessionCleanupDirRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestSessionGCCleanupHookMakesResidueRemovable is the production
// path: host removal fails on the locked results directory, the
// residue project's hook repairs permissions, and gc's retry removes
// the whole directory. The hook sees the directory as cwd and
// MOE_SESSION_DIR; a project with no residue in the directory is not
// asked.
func TestSessionGCCleanupHookMakesResidueRemovable(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, _ := residueFixture(t, root, "telomere")
	writeCleanupHook(t, root, "telomere", "10-fix.sh",
		`{ echo "pwd=$(pwd)"; echo "project=$MOE_PROJECT"; echo "dir=$MOE_SESSION_DIR"; echo "root=$MOE_BUREAUCRACY"; } > "$MOE_BUREAUCRACY/hook.log"
chmod -R u+w "$MOE_SESSION_DIR"
echo fixed
`)
	if err := os.MkdirAll(filepath.Join(root, "projects", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCleanupHook(t, root, "alpha", "10-spy.sh", `touch "$MOE_BUREAUCRACY/alpha-ran"`+"\n")

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "removed worktree dir "+filepath.Base(dir)) {
		t.Errorf("stdout missing removal:\n%s", stdout)
	}
	if !strings.Contains(stderr, "fixed") || !strings.Contains(stderr, "10-fix.sh") {
		t.Errorf("hook output should reach stderr:\n%s", stderr)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("residue still present: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pwd=" + dir, "project=telomere", "dir=" + dir, "root=" + root} {
		if !strings.Contains(string(log), want+"\n") {
			t.Errorf("hook env missing %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "alpha-ran")); !os.IsNotExist(err) {
		t.Errorf("a project with no residue in the directory ran its hook")
	}
}

// TestSessionGCCleanupWithoutHookReportsPath pins the no-hook outcome:
// the original permission error, the directory left whole, exit 1,
// and a reason naming the hook event that would claim it.
func TestSessionGCCleanupWithoutHookReportsPath(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, locked := residueFixture(t, root, "telomere")

	code, _, stderr := runSessionGCInRoot(t, root)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr)
	}
	for _, want := range []string{dir, "permission denied", "no project session-cleanup.d hook claims it"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(locked, ".last-run.json")); err != nil {
		t.Errorf("locked residue should survive: %v", err)
	}
}

// TestSessionGCCleanupHookFailureLeavesResidue: a failing script is
// reported with its path and the directory is not retried.
func TestSessionGCCleanupHookFailureLeavesResidue(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, locked := residueFixture(t, root, "telomere")
	writeCleanupHook(t, root, "telomere", "10-fail.sh", "echo boom >&2\nexit 3\n")
	writeCleanupHook(t, root, "telomere", "20-later.sh", `touch "$MOE_BUREAUCRACY/later-ran"`+"\n")

	code, _, stderr := runSessionGCInRoot(t, root)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr)
	}
	for _, want := range []string{dir, "session-cleanup hook", "10-fail.sh", "exit status 3", "boom"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(locked, ".last-run.json")); err != nil {
		t.Errorf("residue should survive a failed hook: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "later-ran")); !os.IsNotExist(err) {
		t.Errorf("a failed script should stop that project's later scripts")
	}
}

// TestSessionGCCleanupHookThatDoesNotHelp: the hook exits 0 but the
// directory still refuses removal; gc says so rather than claiming it.
func TestSessionGCCleanupHookThatDoesNotHelp(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, _ := residueFixture(t, root, "telomere")
	writeCleanupHook(t, root, "telomere", "10-noop.sh", "exit 0\n")

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "after session-cleanup.d hooks") || !strings.Contains(stderr, dir) {
		t.Errorf("stderr should report the retry failure:\n%s", stderr)
	}
	if strings.Contains(stdout, "removed") {
		t.Errorf("nothing was removed:\n%s", stdout)
	}
}

// TestSessionGCCleanupHookTimeout: a hung script — and the child it
// spawned — is killed at the deadline, so the heartbeat tick cannot
// wedge on it.
func TestSessionGCCleanupHookTimeout(t *testing.T) {
	old := sessionCleanupTimeout
	sessionCleanupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sessionCleanupTimeout = old })
	root := newSessionTestRoot(t)
	residueFixture(t, root, "telomere")
	// The backgrounded sleep stands in for a docker client the script
	// started: WaitDelay alone would let Run return while it lives on.
	writeCleanupHook(t, root, "telomere", "10-hang.sh", "sleep 30 &\necho $! > \"$MOE_BUREAUCRACY/child.pid\"\nsleep 30\n")

	start := time.Now()
	code, _, stderr := runSessionGCInRoot(t, root)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("gc took %s; the deadline did not reap the script", elapsed)
	}
	if code != 1 || !strings.Contains(stderr, "timed out after") {
		t.Errorf("exit=%d, want 1 with a timeout reason:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// The killed child is reparented and reaped by init; give that a
	// moment rather than reading a zombie as alive.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("script's child %d outlived the deadline", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSessionGCCleanupRechecksRegistration: a directory that git came
// to know about while the hook ran is left alone, even though the
// hook made it removable.
func TestSessionGCCleanupRechecksRegistration(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, _ := residueFixture(t, root, "telomere")
	// Register the directory the way `git worktree add` records it.
	writeCleanupHook(t, root, "telomere", "10-register.sh", `chmod -R u+w "$MOE_SESSION_DIR"
admin="$MOE_BUREAUCRACY/.git/worktrees/late"
mkdir -p "$admin"
echo "$MOE_SESSION_DIR/.git" > "$admin/gitdir"
echo ../.. > "$admin/commondir"
git -C "$MOE_BUREAUCRACY" rev-parse HEAD > "$admin/HEAD"
echo "gitdir: $admin" > "$MOE_SESSION_DIR/.git"
`)

	code, _, stderr := runSessionGCInRoot(t, root)
	if code != 1 || !strings.Contains(stderr, "registered with git during cleanup") {
		t.Fatalf("exit=%d, want 1 with a registration refusal:\n%s", code, stderr)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("registered directory should survive: %v", err)
	}
}

// TestHeartbeatSessionGCRunsCleanupHook: the tick shares the pass, so
// it reaches the hook too, with the hook's output in the serve log.
func TestHeartbeatSessionGCRunsCleanupHook(t *testing.T) {
	root := newSessionTestRoot(t)
	dir, _ := residueFixture(t, root, "telomere")
	writeCleanupHook(t, root, "telomere", "10-fix.sh", "chmod -R u+w \"$MOE_SESSION_DIR\"\necho tick-fixed\n")

	var log bytes.Buffer
	newHeartbeatGate(root).gc(&log)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("residue still present: %v\nlog:\n%s", err, log.String())
	}
	for _, want := range []string{"tick-fixed", "heartbeat: session gc removed worktree dir " + filepath.Base(dir)} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log.String())
		}
	}
}
