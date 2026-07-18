package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// chainEdge stamps a chain edge the way `chain edit` does: an empty
// commit carrying just the MoE-Chained-To trailer, which
// BuildJournalIndex replays into the effective chain state.
func chainEdge(t *testing.T, root, parent, child string) {
	t.Helper()
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: "+parent+" "+child+"\n")
}

// TestChainStateBlockRendersUnits pins the block's shape and its two
// filters. The orphan drop is the load-bearing one: an unchained
// in-progress run carries no order information the disk scan doesn't
// already give the agent, so listing it is pure noise.
func TestChainStateBlockRendersUnits(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "queue-batch", queueWorkflow, run.StatusInProgress, now, nil)
	seedRun(t, root, "moe", "fix-red-ci", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Fix red CI on main\n\nbody\n"})
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "moe/queue-batch", "moe/fix-red-ci")

	got := chainStateBlock(root, "moe")
	want := "- `queue-batch` (queue) — queue-batch → `fix-red-ci` (sdlc) — Fix red CI on main"
	if !strings.Contains(got, want) {
		t.Errorf("block missing unit line %q:\n%s", want, got)
	}
	if strings.Contains(got, "lone-run") {
		t.Errorf("block lists the orphan `lone-run`, which carries no order information:\n%s", got)
	}
}

// TestChainStateBlockCrossProject: chains may span projects. A unit
// that touches this project belongs in the block, and its foreign
// members must render qualified so the agent doesn't read `other`'s run
// as one of its own. A chain wholly inside another project is not this
// sweep's business.
func TestChainStateBlockCrossProject(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "mine", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "other", "theirs", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "other", "far-head", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "other", "far-tail", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "moe/mine", "other/theirs")
	chainEdge(t, root, "other/far-head", "other/far-tail")

	got := chainStateBlock(root, "moe")
	if !strings.Contains(got, "`mine` (sdlc)") {
		t.Errorf("block missing this project's member bare:\n%s", got)
	}
	if !strings.Contains(got, "`other/theirs` (sdlc)") {
		t.Errorf("block missing the foreign member qualified:\n%s", got)
	}
	if strings.Contains(got, "far-head") || strings.Contains(got, "far-tail") {
		t.Errorf("block lists a chain wholly inside another project:\n%s", got)
	}
}

// TestChainStateBlockEmpty: nothing sequenced means no block at all,
// consistent with all three siblings in pulseKickoffWithContext. An
// absent block reads as "nothing is sequenced"; an empty one with a
// header reads as a bug.
func TestChainStateBlockEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, time.Now().Local(), nil)

	if got := chainStateBlock(root, "moe"); got != "" {
		t.Errorf("chainStateBlock with nothing chained = %q, want \"\"", got)
	}
}
