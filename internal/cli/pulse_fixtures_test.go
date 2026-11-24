package cli

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// spawnFixture stands up a bureaucracy with one registered project and
// returns its root, ready for the gate minter.
func spawnFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	return root
}

// kickFixture is spawnFixture, chdir'd, with the cascade's two
// agent-facing seams captured rather than executed.
func kickFixture(t *testing.T) (root string, stages *[]openSdlcStageInvocation, pushes *[]pushFromCascadeInvocation) {
	t.Helper()
	root = spawnFixture(t)
	t.Chdir(root)
	t.Setenv("NO_COLOR", "1")
	return root, stubOpenSdlcStage(t, nil), stubPushFromCascade(t, 0, nil)
}

func kickStages(invs []openSdlcStageInvocation) []string {
	out := make([]string, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inv.runID+":"+inv.stage)
	}
	return out
}

// mintSpecs mints specs in one project and returns slug → minted id.
// Specs the minter skipped are absent.
func mintSpecs(root, projectID, pulseSlug string, specs []pulseRunSpec, stdout, stderr io.Writer) map[string]string {
	m := &pulseMinter{root: root, projectID: projectID, spawnedBy: projectID + "/" + pulseSlug}
	out := map[string]string{}
	for _, s := range specs {
		if id := m.mint(s, stdout, stderr); id != "" {
			out[strings.TrimSpace(s.Slug)] = id
		}
	}
	return out
}

// groomFixture mints parked sdlc runs through the real spawn step and
// returns the slug → id map.
func groomFixture(t *testing.T, root string, slugs ...string) map[string]string {
	t.Helper()
	spawns := make([]pulseRunSpec, 0, len(slugs))
	for _, s := range slugs {
		spawns = append(spawns, pulseRunSpec{Slug: s, Title: s})
	}
	minted := mintSpecs(root, "moe", "pulse-groom", spawns, io.Discard, os.Stderr)
	if len(minted) != len(slugs) {
		t.Fatalf("minted %v, want all of %v", minted, slugs)
	}
	return minted
}

// runsWithWorkflow lists the project's in-progress runs for a workflow.
func runsWithWorkflow(t *testing.T, root, projectID, workflow string) []string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, md := range mds {
		if md.Project == projectID && md.Workflow == workflow && md.Status == run.StatusInProgress {
			out = append(out, md.ID)
		}
	}
	return out
}

// sdlcRuns lists the project's sdlc runs at any status.
func sdlcRuns(t *testing.T, root, projectID string) []string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, md := range mds {
		if md.Project == projectID && md.Workflow == "sdlc" {
			out = append(out, md.ID)
		}
	}
	return out
}

// setRunStatus rewrites a run's status and commits it, standing in for a
// lifecycle transition.
func setRunStatus(t *testing.T, root, projectID, id, status string) {
	t.Helper()
	md, err := run.Load(root, projectID, id)
	if err != nil {
		t.Fatal(err)
	}
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := run.StageAndCommit(root, "test: set "+id+" to "+status, run.Dir(projectID, id)); err != nil {
		t.Fatal(err)
	}
}

func slicesContains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// feedStdin pipes a single answer line into os.Stdin for a prompt test,
// restoring the original on cleanup.
func feedStdin(t *testing.T, answer string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, err := io.WriteString(w, answer+"\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
}
