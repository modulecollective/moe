package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// foreignHeldWorkspaceFixture opens tele/fix-it bound to tele/dev but
// never attached, then attaches a second run, tele/other, to the same
// workspace — so fix-it is bound to a tree another run holds.
func foreignHeldWorkspaceFixture(t *testing.T) (root, wp string) {
	t.Helper()
	root = stageWorkspaceFixture(t, "--workspace", "tele/dev")
	other := &run.Metadata{Project: "tele", ID: "other", Workflow: "sdlc", Workspace: "dev"}
	wp, err := attachTestRunWorkspace(t, root, other, branchPrefix+"other")
	if err != nil {
		t.Fatal(err)
	}
	return root, wp
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gittest.Output(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
}

func claimRun(t *testing.T, root, name string) string {
	t.Helper()
	c, err := workspace.ReadClaim(root, "tele", name)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		return ""
	}
	return c.Run
}

// TestReleaseRunWorkspaceLeavesForeignClaim: a run bound to a workspace
// another run holds releases nothing — the holder keeps its claim and
// its tree stays on its branch.
func TestReleaseRunWorkspaceLeavesForeignClaim(t *testing.T) {
	root, wp := foreignHeldWorkspaceFixture(t)

	if err := releaseTestRunWorkspace(t, root, loadRun(t, root, "tele", "fix-it")); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := claimRun(t, root, "dev"); got != "tele/other" {
		t.Errorf("claim = %q, want tele/other", got)
	}
	if got := currentBranch(t, wp); got != branchPrefix+"other" {
		t.Errorf("workspace branch = %q, want %s", got, branchPrefix+"other")
	}
}

// TestSDLCCloseLeavesForeignClaim: the same through the verb — the
// close succeeds and the other run still holds the workspace.
func TestSDLCCloseLeavesForeignClaim(t *testing.T) {
	root, wp := foreignHeldWorkspaceFixture(t)

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "tele/fix-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := loadRun(t, root, "tele", "fix-it").Status; got != run.StatusClosed {
		t.Errorf("status = %q, want closed", got)
	}
	if got := claimRun(t, root, "dev"); got != "tele/other" {
		t.Errorf("claim = %q, want tele/other", got)
	}
	if got := currentBranch(t, wp); got != branchPrefix+"other" {
		t.Errorf("workspace branch = %q, want %s", got, branchPrefix+"other")
	}
}

// TestReleaseRunWorkspaceLeavesUnclaimedTree: only the holder moves the
// tree; an unclaimed workspace stays where it was parked.
func TestReleaseRunWorkspaceLeavesUnclaimedTree(t *testing.T) {
	root, wp := foreignHeldWorkspaceFixture(t)
	if err := workspace.Release(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}

	if err := releaseTestRunWorkspace(t, root, loadRun(t, root, "tele", "fix-it")); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := currentBranch(t, wp); got != branchPrefix+"other" {
		t.Errorf("workspace branch = %q, want %s", got, branchPrefix+"other")
	}
}

// TestReleaseRunWorkspaceMalformedClaim: a claim that can't be read is
// an error, not "not mine" — and it isn't deleted blind.
func TestReleaseRunWorkspaceMalformedClaim(t *testing.T) {
	root := stageWorkspaceFixture(t, "--workspace", "tele/dev")
	plantClaim(t, root, "tele", "dev", "tele/fix-it")
	claimPath := filepath.Join(workspace.Path(root, "tele", "dev"), ".moe", "claim.json")
	if err := os.WriteFile(claimPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := releaseTestRunWorkspace(t, root, loadRun(t, root, "tele", "fix-it")); err == nil {
		t.Errorf("release of a malformed claim returned nil")
	}
	if _, err := os.Stat(claimPath); err != nil {
		t.Errorf("claim.json gone after a failed release: %v", err)
	}
}
