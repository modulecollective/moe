package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// kickFixture stands up a bureaucracy with one project, chdir'd, with
// the cascade's two agent-facing seams stubbed. Everything `moe chain
// kick` drives below the CLI boundary — stage sessions and the ship —
// is captured rather than executed.
func kickFixture(t *testing.T) (root string, stages *[]openSdlcStageInvocation, pushes *[]pushFromCascadeInvocation) {
	t.Helper()
	root = spawnFixture(t)
	t.Chdir(root)
	t.Setenv("NO_COLOR", "1")
	return root, stubOpenSdlcStage(t, nil), stubPushFromCascade(t, 0, nil)
}

// chainEdgeCommit stamps a live chain edge the way `moe chain edit`
// does: an empty commit carrying just the MoE-Chained-To trailer.
func chainEdgeCommit(t *testing.T, root, parent, child string) {
	t.Helper()
	if err := git.Run(root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: "+parent+" "+child+"\n"); err != nil {
		t.Fatal(err)
	}
}

func kickStages(invs []openSdlcStageInvocation) []string {
	out := make([]string, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inv.runID+":"+inv.stage)
	}
	return out
}

// TestChainKickChainRunHeadClosesAndRides is the shape the pulse's
// batches take: a stageless head, two parked fixes behind it. The head
// is trivially done, so kick closes it and the ride carries into the
// children — today's queue-kick behaviour, now falling out of the
// uniform path rather than being its own verb.
func TestChainKickChainRunHeadClosesAndRides(t *testing.T) {
	root, stages, pushes := kickFixture(t)

	spawnAndHead(t, root, "moe", "pulse-one", "batch", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, os.Stderr)

	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain runs %v, want 1", heads)
	}
	head := heads[0]

	var out, errb bytes.Buffer
	if code := runChainKick([]string{"moe/" + head}, &out, &errb); code != 0 {
		t.Fatalf("chain kick exit=%d stderr=%q", code, errb.String())
	}

	// The head closed rather than lingering on the dash's ACTIVE list.
	md, err := run.Load(root, "moe", head)
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Errorf("head status=%s, want closed — a chain run's whole lifecycle is reaching done", md.Status)
	}

	// Both children rode the full ladder and shipped.
	got := kickStages(*stages)
	want := []string{
		"fix-one:design", "fix-one:code", "fix-one:review", "fix-one:test",
		"fix-two:design", "fix-two:code", "fix-two:review", "fix-two:test",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dispatched %v, want %v\nstdout=%q", got, want, out.String())
	}
	if len(*pushes) != 2 {
		t.Errorf("pushes=%d, want 2 (one per chained fix)", len(*pushes))
	}
}

// TestChainKickRegularHeadCascadesThenRides: kick is uniform. A head
// that is an ordinary run with work left cascades to its own ship
// first, then rides its children — a programmatic `!!!`.
func TestChainKickRegularHeadCascadesThenRides(t *testing.T) {
	root, stages, pushes := kickFixture(t)

	if _, err := run.New(root, "moe", run.Options{ID: "head-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{ID: "child-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/head-run", "moe/child-run")

	var out, errb bytes.Buffer
	if code := runChainKick([]string{"moe/head-run"}, &out, &errb); code != 0 {
		t.Fatalf("chain kick exit=%d stderr=%q", code, errb.String())
	}

	got := kickStages(*stages)
	want := []string{
		"head-run:design", "head-run:code", "head-run:review", "head-run:test",
		"child-run:design", "child-run:code", "child-run:review", "child-run:test",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dispatched %v, want %v", got, want)
	}
	if len(*pushes) != 2 {
		t.Errorf("pushes=%d, want 2 (head ships, then the child does)", len(*pushes))
	}
}

// TestChainKickChildlessRunIsAChainOfOne: pruning a chain down to its
// last run must not make that run unkickable. Kick cascades it to its
// ship and rides nothing — the verb-form twin of `--ship`.
func TestChainKickChildlessRunIsAChainOfOne(t *testing.T) {
	root, stages, pushes := kickFixture(t)

	if _, err := run.New(root, "moe", run.Options{ID: "lone-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/lone-run"}, io.Discard, &errb); code != 0 {
		t.Fatalf("chain kick exit=%d stderr=%q", code, errb.String())
	}
	if got := len(*stages); got != 4 {
		t.Errorf("dispatched %d stages (%v), want the lone run's four", got, kickStages(*stages))
	}
	if len(*pushes) != 1 {
		t.Errorf("pushes=%d, want 1 — it ships and rides nothing", len(*pushes))
	}
}

// TestChainKickRegularHeadWithNothingPendingRidesButStaysOpen: the
// standing chain-ride decision is never to auto-close someone's parked
// run. Only a chain run — whose whole lifecycle is reaching done —
// closes on kick.
func TestChainKickRegularHeadWithNothingPendingRidesButStaysOpen(t *testing.T) {
	root, stages, _ := kickFixture(t)

	if _, err := run.New(root, "moe", run.Options{ID: "head-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{ID: "child-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/head-run", "moe/child-run")
	// Flip the head to pushed: Next() short-circuits to done, so there
	// is no stage left for the cascade to drive.
	setRunStatus(t, root, "moe", "head-run", run.StatusPushed)

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/head-run"}, io.Discard, &errb); code != 0 {
		t.Fatalf("chain kick exit=%d stderr=%q", code, errb.String())
	}

	md, err := run.Load(root, "moe", "head-run")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusPushed {
		t.Errorf("head status=%s, want it left alone at pushed", md.Status)
	}
	for _, inv := range *stages {
		if inv.runID != "child-run" {
			t.Errorf("dispatched against the head %+v, want only the child ridden", inv)
		}
	}
	if len(*stages) == 0 {
		t.Error("no stages dispatched — the child should still have ridden")
	}
}

// TestChainKickRefusesANonHead: with several chains per project the
// kick always names its target, so naming a link rather than the head
// is an operator error worth correcting by name.
func TestChainKickRefusesANonHead(t *testing.T) {
	root, stages, _ := kickFixture(t)

	if _, err := run.New(root, "moe", run.Options{ID: "head-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{ID: "child-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/head-run", "moe/child-run")

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/child-run"}, io.Discard, &errb); code == 0 {
		t.Fatal("chain kick rode a run that is chained under another")
	}
	if !strings.Contains(errb.String(), "chained under moe/head-run") {
		t.Errorf("stderr=%q, want the head named so the operator can retarget", errb.String())
	}
	if len(*stages) != 0 {
		t.Errorf("a refused kick dispatched %v, want nothing", kickStages(*stages))
	}
}

// TestChainKickRefusesUnchainableWorkflows: kick's admissible set is
// the chain editor's — operatorCascades ∪ chain. A machine-paced pulse
// run is not something the operator chains, so it is not something kick
// drives either.
func TestChainKickRefusesUnchainableWorkflows(t *testing.T) {
	root, stages, _ := kickFixture(t)

	if _, err := run.New(root, "moe", run.Options{ID: "pulse-run", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/pulse-run"}, io.Discard, &errb); code == 0 {
		t.Fatal("chain kick accepted a pulse run")
	}
	if !strings.Contains(errb.String(), "not chainable") {
		t.Errorf("stderr=%q, want the refusal named", errb.String())
	}
	if len(*stages) != 0 {
		t.Errorf("a refused kick dispatched %v, want nothing", kickStages(*stages))
	}
}

// TestChainKickRequiresAQualifiedRun: no omitted-run convenience —
// with several chains per project, a bare project name is ambiguous.
func TestChainKickRequiresAQualifiedRun(t *testing.T) {
	root, _, _ := kickFixture(t)
	_ = root

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe"}, io.Discard, &errb); code != 2 {
		t.Fatalf("chain kick exit=%d, want 2 (usage) for a bare project", code)
	}
}

// TestChainKickStalledRideExitsNonZero is the reason this seam exists:
// kick is the programmatic entry point (cron, scripts), so a chain that
// stalls partway must reach the shell as a non-zero exit rather than as
// a stderr line nobody parses. The head still closes — its own work was
// trivially done, and leaving it open to punish a child's failure would
// park a dead run on the dash.
func TestChainKickStalledRideExitsNonZero(t *testing.T) {
	root, _, _ := kickFixture(t)
	// Re-stub: the first child's design stage fails.
	stages := stubOpenSdlcStage(t, map[string]int{"design": 1})
	pushes := stubPushFromCascade(t, 0, nil)

	spawnAndHead(t, root, "moe", "pulse-one", "batch", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, os.Stderr)

	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain runs %v, want 1", heads)
	}
	head := heads[0]

	var out, errb bytes.Buffer
	if code := runChainKick([]string{"moe/" + head}, &out, &errb); code != 1 {
		t.Fatalf("chain kick exit=%d, want 1 (the stalled child's code)\nstdout=%q\nstderr=%q", code, out.String(), errb.String())
	}

	// The head still closed — the ride's failure is not its failure.
	md, err := run.Load(root, "moe", head)
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Errorf("head status=%s, want closed — a stalled ride must not leave the head parked", md.Status)
	}

	// The walk stopped at the failing stage: fix-one's later stages never
	// dispatched, and fix-two was never reached.
	got := kickStages(*stages)
	want := []string{"fix-one:design"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dispatched %v, want %v\nstdout=%q", got, want, out.String())
	}
	if len(*pushes) != 0 {
		t.Errorf("pushes=%d, want 0 — nothing reached a ship", len(*pushes))
	}
	// The exit code says "something stalled"; stderr says which run.
	if !strings.Contains(errb.String(), "chain ride into moe/fix-one exited 1") {
		t.Errorf("expected stalled-ride stderr line naming the child, got:\n%s", errb.String())
	}
}
