package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestSDLCDesignWrongProjectFailsFast: a typo on the project (or run)
// should fail with "run not found" before any per-turn worktree gets
// materialised. Without the pre-flight in runDesign, the failure
// surfaced as a raw filesystem read error from inside the worktree —
// uninformative and harder to recover from.
func TestSDLCDesignWrongProjectFailsFast(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "design", "wrongproj", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on wrong-project typo, stdout=%q", out.String())
	}
	msg := errb.String()
	if !strings.Contains(msg, "run not found") {
		t.Fatalf("expected run-not-found error, got: %q", msg)
	}
	if !strings.Contains(msg, "wrongproj ghost") {
		t.Fatalf("expected error to name wrongproj ghost, got: %q", msg)
	}
}

// TestSDLCCodeWrongProjectSaysRunNotFound: on `sdlc code` with a typo,
// the operator must see "run not found" and not "design canvas
// missing" — the latter sends them off to run a design stage that's
// also going to fail. The pre-flight beats requireDesignCanvas to the
// punch.
func TestSDLCCodeWrongProjectSaysRunNotFound(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "wrongproj", "ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on wrong-project typo, stdout=%q", out.String())
	}
	msg := errb.String()
	if !strings.Contains(msg, "run not found") {
		t.Fatalf("expected run-not-found error, got: %q", msg)
	}
	if strings.Contains(msg, "design canvas missing") {
		t.Fatalf("typo should not surface as design-canvas-missing, got: %q", msg)
	}
}
