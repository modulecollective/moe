package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// spawnFixture stands up a bureaucracy with one registered project and
// returns its root, ready for maybeSpawnFixRuns.
func spawnFixture(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	return root
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

// TestSpawnMintsParkedRunsUnderAQueue is the core of Part 2: a gate
// carrying spawn entries mints one parked sdlc run each, opens a queue
// placeholder, and chains queue → fix1 → fix2 in the proposed order.
func TestSpawnMintsParkedRunsUnderAQueue(t *testing.T) {
	root := spawnFixture(t)

	var errb bytes.Buffer
	maybeSpawnFixRuns(root, "moe", "pulse-2026-07-18", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI on main", Why: "TestX failing since abc123", Design: "# Fix red CI\n\nTestX asserts a stale path.\n"},
		{Slug: "fix-doc-drift", Title: "Fix doc drift", Why: "README names a removed flag"},
	}, io.Discard, &errb)

	sdlcRuns := runsWithWorkflow(t, root, "moe", "sdlc")
	if len(sdlcRuns) != 2 {
		t.Fatalf("sdlc runs %v, want 2; stderr=%s", sdlcRuns, errb.String())
	}
	queueRuns := runsWithWorkflow(t, root, "moe", queueWorkflow)
	if len(queueRuns) != 1 {
		t.Fatalf("queue runs %v, want exactly 1; stderr=%s", queueRuns, errb.String())
	}

	// Every spawned run parks: opened, never advanced.
	for _, id := range sdlcRuns {
		md, err := run.Load(root, "moe", id)
		if err != nil {
			t.Fatal(err)
		}
		if md.Status != run.StatusInProgress {
			t.Errorf("%s status=%s, want in_progress (parked)", id, md.Status)
		}
		if md.SpawnedBy != "moe/pulse-2026-07-18" {
			t.Errorf("%s spawned_by=%q, want the pulse that proposed it", id, md.SpawnedBy)
		}
	}

	// The design seed reaches the canvas, so the design stage starts
	// from the survey's findings rather than an empty page.
	var fixRun string
	for _, id := range sdlcRuns {
		if strings.HasPrefix(id, "fix-ci-red-main") {
			fixRun = id
		}
	}
	if fixRun == "" {
		t.Fatalf("no run derived from slug fix-ci-red-main in %v", sdlcRuns)
	}
	seed, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", fixRun, "design")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seed), "TestX asserts a stale path") {
		t.Errorf("design canvas did not carry the proposed seed:\n%s", seed)
	}

	// The chain runs queue → first proposal → second proposal.
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	queueKey := "moe/" + queueRuns[0]
	first := idx.ChainedChild[queueKey]
	if !strings.HasPrefix(first, "moe/fix-ci-red-main") {
		t.Fatalf("queue chains to %q, want the first proposal", first)
	}
	second := idx.ChainedChild[first]
	if !strings.HasPrefix(second, "moe/fix-doc-drift") {
		t.Fatalf("%s chains to %q, want the second proposal", first, second)
	}
	if tail := idx.ChainedChild[second]; tail != "" {
		t.Errorf("last proposal chains to %q, want nothing", tail)
	}
}

// TestSpawnAppendsToTheLiveQueue: a later pulse appends to the existing
// queue's tail rather than minting a second queue for the project.
func TestSpawnAppendsToTheLiveQueue(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
	}, io.Discard, io.Discard)
	maybeSpawnFixRuns(root, "moe", "pulse-two", []pulseSpawn{
		{Slug: "fix-two", Title: "Two"},
	}, io.Discard, io.Discard)

	queueRuns := runsWithWorkflow(t, root, "moe", queueWorkflow)
	if len(queueRuns) != 1 {
		t.Fatalf("queue runs %v, want exactly 1 live queue per project", queueRuns)
	}

	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	first := idx.ChainedChild["moe/"+queueRuns[0]]
	if !strings.HasPrefix(first, "moe/fix-one") {
		t.Fatalf("queue chains to %q, want fix-one", first)
	}
	if second := idx.ChainedChild[first]; !strings.HasPrefix(second, "moe/fix-two") {
		t.Fatalf("fix-one chains to %q, want the later pulse's fix-two appended at the tail", second)
	}

	// Both entries are on the queue canvas — that's what the operator
	// reads before kicking.
	canvas, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", queueRuns[0], queueDoc)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fix-one", "fix-two", "pulse-one", "pulse-two"} {
		if !strings.Contains(string(canvas), want) {
			t.Errorf("queue canvas missing %q:\n%s", want, canvas)
		}
	}
}

// TestSpawnSkipsSlugsAlreadyInProgress is the one mechanical guard the
// harness owns: a finding that survives two pulses must not queue the
// same fix twice.
func TestSpawnSkipsSlugsAlreadyInProgress(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI"},
	}, io.Discard, io.Discard)

	var errb bytes.Buffer
	maybeSpawnFixRuns(root, "moe", "pulse-two", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI (again)"},
	}, io.Discard, &errb)

	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != 1 {
		t.Fatalf("sdlc runs %v, want 1 — the second proposal should dedupe", got)
	}
	if !strings.Contains(errb.String(), "already has an in-progress run") {
		t.Errorf("stderr=%q, want the dedupe skip named", errb.String())
	}
}

// TestSpawnDedupeIsNotPrefixGreedy: `fix-ci` and `fix-ci-red-main` are
// different proposals. Only a date-shaped remainder means "the harness
// already dated this base" — a bare prefix match would silently drop
// every proposal that happens to extend a live slug.
func TestSpawnDedupeIsNotPrefixGreedy(t *testing.T) {
	if !slugBaseMatches([]string{"fix-ci-2026-07-18"}, "fix-ci") {
		t.Error("a dated form of the base should dedupe")
	}
	if !slugBaseMatches([]string{"fix-ci-2026-07-18-2"}, "fix-ci") {
		t.Error("a same-day repeat of the base should dedupe")
	}
	if !slugBaseMatches([]string{"fix-ci"}, "fix-ci") {
		t.Error("the bare base should dedupe")
	}
	if slugBaseMatches([]string{"fix-ci-red-main-2026-07-18"}, "fix-ci") {
		t.Error("a longer, different slug must not dedupe against a shorter base")
	}
}

// TestSpawnSkipsUnusableSlugs: a malformed slug is skipped with a
// warning, and the rest of the batch still lands. Warn-only is the
// pulse's posture everywhere else too.
func TestSpawnSkipsUnusableSlugs(t *testing.T) {
	root := spawnFixture(t)

	var errb bytes.Buffer
	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "Not A Slug", Title: "Bad"},
		{Slug: "", Title: "Also bad"},
		{Slug: "fix-good", Title: "Good"},
	}, io.Discard, &errb)

	got := runsWithWorkflow(t, root, "moe", "sdlc")
	if len(got) != 1 || !strings.HasPrefix(got[0], "fix-good") {
		t.Fatalf("sdlc runs %v, want only the well-formed proposal", got)
	}
	if strings.Count(errb.String(), "unusable slug") != 2 {
		t.Errorf("stderr=%q, want both malformed entries warned", errb.String())
	}
}

// TestSpawnWithNoEntriesTouchesNothing: the overwhelmingly common gate
// carries no spawn list at all, and must not mint a queue for a project
// that has nothing queued.
func TestSpawnWithNoEntriesTouchesNothing(t *testing.T) {
	root := spawnFixture(t)
	maybeSpawnFixRuns(root, "moe", "pulse-one", nil, io.Discard, io.Discard)
	if got := runsWithWorkflow(t, root, "moe", queueWorkflow); len(got) != 0 {
		t.Fatalf("queue runs %v, want none — an empty spawn list opens nothing", got)
	}
}

// TestPulseGateParsesSpawnList pins the wire shape the stage fragment
// teaches, end to end through the canvas reader.
func TestPulseGateParsesSpawnList(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedGitHubProject(t, root, "moe")

	canvas := filepath.Join(root, run.ContentPath("moe", "pulse-x", pulseDoc))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Pulse\n\n## Gate\n\n```json\n" +
		`{"status":"ok","reflect":{"due":false},"spawn":[{"slug":"fix-ci-red-main","title":"Fix red CI on main","why":"TestX failing","design":"# seed\n"}]}` +
		"\n```\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	gate, ok := readPulseGate(root, "moe", "pulse-x")
	if !ok {
		t.Fatal("gate did not parse")
	}
	if len(gate.Spawn) != 1 {
		t.Fatalf("spawn entries %+v, want 1", gate.Spawn)
	}
	if gate.Spawn[0].Slug != "fix-ci-red-main" || gate.Spawn[0].Title != "Fix red CI on main" {
		t.Errorf("spawn entry = %+v, want the proposed slug and title", gate.Spawn[0])
	}
}

// TestQueueKickWithNoLiveQueueRefuses: kicking a project with nothing
// queued is an operator error worth naming, not a silent success.
func TestQueueKickWithNoLiveQueueRefuses(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)

	var errb bytes.Buffer
	if code := runQueueKick([]string{"moe"}, io.Discard, &errb); code == 0 {
		t.Fatal("queue kick exited 0 with no live queue")
	}
	if !strings.Contains(errb.String(), "no live queue run") {
		t.Errorf("stderr=%q, want the empty-queue refusal named", errb.String())
	}
}

// TestChainEditOffersQueueHeads: the operator prunes and reorders a
// proposed batch before kicking it, which means the queue head has to
// show up in the editor alongside the sdlc runs it chains to.
func TestChainEditOffersQueueHeads(t *testing.T) {
	root := spawnFixture(t)
	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
	}, io.Discard, io.Discard)

	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var offered []string
	for _, block := range activeChainItems(mds, idx, byKey) {
		for _, it := range block {
			offered = append(offered, it.Key)
		}
	}
	queueRuns := runsWithWorkflow(t, root, "moe", queueWorkflow)
	wantQueue := "moe/" + queueRuns[0]
	if !slicesContains(offered, wantQueue) {
		t.Fatalf("chain edit offered %v, want the queue head %s among them", offered, wantQueue)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
