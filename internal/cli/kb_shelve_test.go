package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// commitMarker stages and commits the bureaucracy marker file so the
// shelve tests can pass requireCleanTree. markBureaucracy only writes
// the file; leaving it uncommitted is fine for dash (no clean-tree
// gate) but shelve refuses on a dirty tree, by design.
func commitMarker(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "add", "bureaucracy.conf")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add marker: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", root, "commit", "-m", "add bureaucracy marker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit marker: %v\n%s", err, out)
	}
}

// TestShelveRefusesBeforeSummarize guards the precondition: shelve
// must refuse if no `MoE-Document: summarize` work-turn commit exists
// for the run yet. This is the "summarize was never actually written"
// case — shelving there would copy an empty or placeholder file into
// the knowledge shelf. The refusal keeps the bad state out.
//
// The test stops before the claude subprocess would be invoked: the
// precondition check is the very first gate after metadata load, so
// the call to claude is never reached on this path. That's what
// makes it a meaningful unit test despite the rest of shelve
// requiring a real Claude Code install.
func TestShelveRefusesBeforeSummarize(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	commitMarker(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "dns-basics", "kb", "in_progress")
	// Intentionally no summarize commit.

	var out, errb bytes.Buffer
	code := Run([]string{"workflow", "kb", "shelve", "tele", "dns-basics"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit when summarize is missing, stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "summarize has not been written") {
		t.Fatalf("stderr missing precondition message, got: %q", errb.String())
	}
}

// TestShelveRefusesNonKBRun keeps operators from mis-typing a workflow
// and silently writing kb-shaped output onto a non-kb run.
func TestShelveRefusesNonKBRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	commitMarker(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", "in_progress")

	var out, errb bytes.Buffer
	code := Run([]string{"workflow", "kb", "shelve", "tele", "fix-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit for non-kb run, stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "not kb") {
		t.Fatalf("stderr missing workflow mismatch message, got: %q", errb.String())
	}
}
