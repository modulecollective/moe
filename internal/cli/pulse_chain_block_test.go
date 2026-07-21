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

	seedRun(t, root, "moe", "chain-batch", chainWorkflow, run.StatusInProgress, now, nil)
	seedRun(t, root, "moe", "fix-red-ci", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Fix red CI on main\n\nbody\n"})
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "moe/chain-batch", "moe/fix-red-ci")

	got := chainStateBlock(mustPulseScan(t, root), "moe")
	want := "- `chain-batch` (chain) — chain-batch → `fix-red-ci` (sdlc) — Fix red CI on main"
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

	got := chainStateBlock(mustPulseScan(t, root), "moe")
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

	if got := chainStateBlock(mustPulseScan(t, root), "moe"); got != "" {
		t.Errorf("chainStateBlock with nothing chained = %q, want \"\"", got)
	}
}

// TestChainStateBlockKeepsTailOfShippedChain reproduces the 2026-07-20
// incident: a two-item chain whose head merged. The block used to drop
// the unit (one active member), the sweep saw no order at all, and it
// narrated the tail — the literal next thing to run — as a run that had
// been deliberately un-threaded. The settled head must render as the
// leading term.
func TestChainStateBlockKeepsTailOfShippedChain(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "shipped-head", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "moe", "queued-tail", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Prefer reflect at end of chain\n\nbody\n"})
	chainEdge(t, root, "moe/shipped-head", "moe/queued-tail")

	got := chainStateBlock(mustPulseScan(t, root), "moe")
	want := "- `shipped-head` (sdlc, merged) → `queued-tail` (sdlc) — Prefer reflect at end of chain"
	if !strings.Contains(got, want) {
		t.Errorf("block missing the settled-head line %q:\n%s", want, got)
	}
	if !strings.Contains(got, "already executing") {
		t.Errorf("block renders a settled head but never tells the agent how to read it:\n%s", got)
	}
}

// TestChainStateBlockOrphanStillDropped: the settled-parent exception is
// narrow. A one-member unit with no chain edge at all is still an orphan
// and still carries no order information worth spending context on.
func TestChainStateBlockOrphanStillDropped(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "shipped-head", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "moe", "queued-tail", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "moe/shipped-head", "moe/queued-tail")

	got := chainStateBlock(mustPulseScan(t, root), "moe")
	if strings.Contains(got, "lone-run") {
		t.Errorf("block lists the orphan `lone-run`:\n%s", got)
	}
}

// TestChainStateBlockSettledParentCrossProject: a settled head in
// another project must render qualified, same rule as active foreign
// members — an agent must never read `other`'s run as one of its own.
func TestChainStateBlockSettledParentCrossProject(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "other", "their-head", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "moe", "my-tail", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "other/their-head", "moe/my-tail")

	got := chainStateBlock(mustPulseScan(t, root), "moe")
	if !strings.Contains(got, "`other/their-head` (sdlc, merged) →") {
		t.Errorf("settled foreign head not rendered qualified:\n%s", got)
	}
}

// TestChainStateBlockSettledParentOnlyForeignTail: a settled head in
// this project whose tail lives entirely elsewhere is not this sweep's
// business — the touches rule is unchanged by the new exception.
func TestChainStateBlockSettledParentOnlyForeignTail(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()

	seedRun(t, root, "moe", "my-head", "sdlc", run.StatusMerged, now, nil)
	seedRun(t, root, "other", "their-tail", "sdlc", run.StatusInProgress, now, nil)
	chainEdge(t, root, "moe/my-head", "other/their-tail")

	if got := chainStateBlock(mustPulseScan(t, root), "moe"); got != "" {
		t.Errorf("block rendered a unit with no active member in this project:\n%s", got)
	}
}
