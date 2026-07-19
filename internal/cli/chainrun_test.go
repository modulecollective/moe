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

// TestSpawnMintsParkedRunsUnderAChain: a gate carrying two spawn
// entries mints one parked sdlc run each, opens a chain placeholder,
// and chains chain → fix1 → fix2 in the proposed order.
func TestSpawnMintsParkedRunsUnderAChain(t *testing.T) {
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
	chainRuns := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(chainRuns) != 1 {
		t.Fatalf("chain runs %v, want exactly 1 fresh chain head; stderr=%s", chainRuns, errb.String())
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

	// The chain runs head → first proposal → second proposal.
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	chainKey := "moe/" + chainRuns[0]
	first := idx.ChainedChild[chainKey]
	if !strings.HasPrefix(first, "moe/fix-ci-red-main") {
		t.Fatalf("chain head chains to %q, want the first proposal", first)
	}
	second := idx.ChainedChild[first]
	if !strings.HasPrefix(second, "moe/fix-doc-drift") {
		t.Fatalf("%s chains to %q, want the second proposal", first, second)
	}
	if tail := idx.ChainedChild[second]; tail != "" {
		t.Errorf("last proposal chains to %q, want nothing", tail)
	}

	// The head carries the spawner too, so the whole batch reads as the
	// survey's lineage rather than the fix runs alone.
	headMD, err := run.Load(root, "moe", chainRuns[0])
	if err != nil {
		t.Fatal(err)
	}
	if headMD.SpawnedBy != "moe/pulse-2026-07-18" {
		t.Errorf("chain head spawned_by=%q, want the pulse that proposed the batch", headMD.SpawnedBy)
	}

	// The canvas names the spawning pulse and nothing else. Membership
	// used to be appended here, one line per run, frozen at mint time —
	// it is the edges above that the operator reads before kicking now,
	// rendered live. Provenance is the one fact those edges don't carry.
	canvas, err := os.ReadFile(filepath.Join(root, run.ContentPath("moe", chainRuns[0], chainDoc)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canvas), "pulse-2026-07-18") {
		t.Errorf("chain canvas missing its provenance line:\n%s", canvas)
	}
	for _, unwanted := range []string{"fix-ci-red-main", "fix-doc-drift"} {
		if strings.Contains(string(canvas), unwanted) {
			t.Errorf("chain canvas should not list member %q — that's the drift the live section ends:\n%s", unwanted, canvas)
		}
	}
}

// TestSpawnNeverAppendsToAnExistingChain is the safety property the
// rename bought: the pulse mints a fresh head per batch and has no
// append path at all. Two consecutive pulses produce two independent
// chain units — never an edge stamped behind a head the operator may
// already be riding, and never a machine proposal dumped into a curated
// topic chain.
func TestSpawnNeverAppendsToAnExistingChain(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, io.Discard, io.Discard)
	maybeSpawnFixRuns(root, "moe", "pulse-two", []pulseSpawn{
		{Slug: "fix-three", Title: "Three"},
		{Slug: "fix-four", Title: "Four"},
	}, io.Discard, io.Discard)

	chainRuns := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(chainRuns) != 2 {
		t.Fatalf("chain runs %v, want 2 — one fresh head per batch", chainRuns)
	}

	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	// Neither head hangs off anything, and neither batch's tail chains
	// onward: the two units are disjoint.
	for _, id := range chainRuns {
		key := "moe/" + id
		for parent, child := range idx.ChainedChild {
			if child == key {
				t.Errorf("chain head %s is chained under %s — the pulse must never append", key, parent)
			}
		}
	}
	for parent, child := range idx.ChainedChild {
		if (strings.HasPrefix(parent, "moe/fix-two") || strings.HasPrefix(parent, "moe/fix-four")) && child != "" {
			t.Errorf("batch tail %s chains onward to %s — the pulse must never append", parent, child)
		}
	}
}

// TestSpawnOfOneParksStandalone: a single proposal needs no placeholder.
// `moe chain kick` handles a chain of one, so minting a head for it
// would be a run to review and close for nothing.
func TestSpawnOfOneParksStandalone(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-one", Title: "One"},
	}, io.Discard, io.Discard)

	if got := runsWithWorkflow(t, root, "moe", chainWorkflow); len(got) != 0 {
		t.Fatalf("chain runs %v, want none — a batch of one parks standalone", got)
	}
	sdlc := runsWithWorkflow(t, root, "moe", "sdlc")
	if len(sdlc) != 1 {
		t.Fatalf("sdlc runs %v, want the lone fix", sdlc)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for parent, child := range idx.ChainedChild {
		if child == "moe/"+sdlc[0] {
			t.Errorf("lone fix is chained under %s, want it standalone", parent)
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
	if !strings.Contains(errb.String(), "already has a live run") {
		t.Errorf("stderr=%q, want the dedupe skip named", errb.String())
	}
}

// TestSpawnSkipsSlugsAlreadyPushed: a fix pushed with `--pr` is waiting
// on a human to merge, so whatever it fixes is still broken on the
// default branch. Re-proposing it is the duplicate the guard exists to
// stop — the run leaving StatusInProgress must not drop it from the set.
func TestSpawnSkipsSlugsAlreadyPushed(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI"},
	}, io.Discard, io.Discard)
	setRunStatus(t, root, "moe", "fix-ci-red-main", run.StatusPushed)

	var errb bytes.Buffer
	maybeSpawnFixRuns(root, "moe", "pulse-two", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI (again)"},
	}, io.Discard, &errb)

	if got := sdlcRuns(t, root, "moe"); len(got) != 1 {
		t.Fatalf("sdlc runs %v, want 1 — a pushed run still dedupes", got)
	}
	if !strings.Contains(errb.String(), "already has a live run") {
		t.Errorf("stderr=%q, want the dedupe skip named", errb.String())
	}
}

// TestSpawnRespawnsAfterMerge pins the other half of the decision: once
// the fix has merged, a still-red check means the fix didn't take or
// something new broke. That's a new run, not a duplicate — so widening
// the live set further has to argue with this test.
func TestSpawnRespawnsAfterMerge(t *testing.T) {
	root := spawnFixture(t)

	maybeSpawnFixRuns(root, "moe", "pulse-one", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI"},
	}, io.Discard, io.Discard)
	setRunStatus(t, root, "moe", "fix-ci-red-main", run.StatusMerged)

	maybeSpawnFixRuns(root, "moe", "pulse-two", []pulseSpawn{
		{Slug: "fix-ci-red-main", Title: "Fix red CI (again)"},
	}, io.Discard, io.Discard)

	got := sdlcRuns(t, root, "moe")
	if len(got) != 2 {
		t.Fatalf("sdlc runs %v, want 2 — a merged run should not dedupe", got)
	}
}

// setRunStatus rewrites a run's status in place, standing in for the
// lifecycle transitions (`moe sdlc push`, `moe run close`) the spawn
// guard has to read correctly.
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
	// runopen.Open refuses a dirty tree, so land the transition the way
	// the real lifecycle commands do before the next pulse runs.
	if err := run.StageAndCommit(root, "test: set "+id+" to "+status, run.Dir(projectID, id)); err != nil {
		t.Fatal(err)
	}
}

// sdlcRuns lists the project's sdlc runs at any status — runsWithWorkflow
// filters to in-progress, which hides exactly the runs these tests move.
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
// carries no spawn list at all, and must not mint a chain head for a
// project that has nothing queued.
func TestSpawnWithNoEntriesTouchesNothing(t *testing.T) {
	root := spawnFixture(t)
	maybeSpawnFixRuns(root, "moe", "pulse-one", nil, io.Discard, io.Discard)
	if got := runsWithWorkflow(t, root, "moe", chainWorkflow); len(got) != 0 {
		t.Fatalf("chain runs %v, want none — an empty spawn list opens nothing", got)
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

// TestChainNewRequiresASlug: operator-minted chains are the topical
// ones, so they get named. A bare project is a usage error, not a
// silently-dated `moe/chain`.
func TestChainNewRequiresASlug(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)

	var errb bytes.Buffer
	if code := runChainNew([]string{"moe"}, io.Discard, &errb); code != 2 {
		t.Fatalf("chain new exit=%d, want 2 (usage) for a bare project", code)
	}
	if !strings.Contains(errb.String(), "<project>/<run>") {
		t.Errorf("stderr=%q, want the qualified-argument shape named", errb.String())
	}
	if got := runsWithWorkflow(t, root, "moe", chainWorkflow); len(got) != 0 {
		t.Fatalf("chain runs %v, want none — a refused mint opens nothing", got)
	}
}

// TestChainNewMintsAndCoexists: several live chains per project is the
// point of minting by hand (one per topic), and a re-used slug dates
// rather than colliding — the IDBase rule the pulse's own mint uses.
func TestChainNewMintsAndCoexists(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)

	var out bytes.Buffer
	for _, arg := range []string{"moe/perf-cleanups", "moe/doc-sweep", "moe/perf-cleanups"} {
		if code := runChainNew([]string{arg}, &out, os.Stderr); code != 0 {
			t.Fatalf("chain new %s exit=%d", arg, code)
		}
	}

	got := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(got) != 3 {
		t.Fatalf("chain runs %v, want 3 live chains coexisting", got)
	}
	var perf int
	for _, id := range got {
		if strings.HasPrefix(id, "perf-cleanups") {
			perf++
		}
	}
	if perf != 2 {
		t.Errorf("perf-cleanups runs = %d in %v, want 2 (the repeat dates rather than colliding)", perf, got)
	}
	// The mint prints the two next steps; a head nobody knows how to use
	// is a head nobody uses.
	for _, want := range []string{"moe chain edit", "moe chain kick"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout=%q, want %q in the next-steps hint", out.String(), want)
		}
	}
}

// TestChainNewRefusesANonCanonicalSlug: the slug is operator-typed, so
// it fails loud rather than being silently slugified into something the
// operator didn't ask for.
func TestChainNewRefusesANonCanonicalSlug(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)

	var errb bytes.Buffer
	if code := runChainNew([]string{"moe/Perf-Cleanups"}, io.Discard, &errb); code == 0 {
		t.Fatal("chain new accepted a non-canonical slug")
	}
	if !strings.Contains(errb.String(), "perf-cleanups") {
		t.Errorf("stderr=%q, want the canonical form suggested", errb.String())
	}
}

// TestChainEditOffersMintedHeads: the operator prunes and reorders a
// batch before kicking it, which means a chain head — hand-minted or
// pulse-minted — has to show up in the editor alongside the runs it
// chains to. Offering it is mandatory, not cosmetic: chain edit clears
// the edges of any run it didn't show.
func TestChainEditOffersMintedHeads(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	if code := runChainNew([]string{"moe/perf-cleanups"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("chain new failed")
	}

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
	chainRuns := runsWithWorkflow(t, root, "moe", chainWorkflow)
	want := "moe/" + chainRuns[0]
	if !slicesContains(offered, want) {
		t.Fatalf("chain edit offered %v, want the chain head %s among them", offered, want)
	}
}

// TestChainCanvasResolvesWithoutAStage: the chain workflow registers no
// stages on purpose (that is what makes a chain run trivially done), so
// its canvas hangs off RegisterDoc instead. Both the serve run page and
// `moe <wf> cat` route through resolveCanvasPath — if this regresses,
// the operator's purpose note becomes unreachable.
func TestChainCanvasResolvesWithoutAStage(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	if code := runChainNew([]string{"moe/perf-cleanups"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("chain new failed")
	}
	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)

	wf, err := LookupWorkflow(chainWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Stages()) != 0 {
		t.Errorf("chain stages = %v, want none — the empty ladder is what makes a chain run trivially done", wf.Stages())
	}
	if got := wf.Docs(); len(got) != 1 || got[0] != chainDoc {
		t.Fatalf("chain docs = %v, want just %q", got, chainDoc)
	}

	path, err := resolveCanvasPath(root, chainWorkflow, "moe", heads[0], chainDoc)
	if err != nil {
		t.Fatalf("resolve chain canvas: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chain canvas: %v", err)
	}
	if !strings.Contains(string(body), "# Chain") {
		t.Errorf("chain canvas skeleton missing its heading:\n%s", body)
	}
	// The seeded note is a heading plus an HTML comment and nothing else:
	// an untouched note renders as a bare heading, not as boilerplate the
	// operator has to recognise as unwritten. And no membership list —
	// that is the drift the live members section exists to end.
	if strings.Contains(string(body), "## Chained") {
		t.Errorf("chain canvas should carry no membership section:\n%s", body)
	}
	open, close := strings.Index(string(body), "<!--"), strings.Index(string(body), "-->")
	if open < 0 || close < open {
		t.Fatalf("chain canvas skeleton missing its HTML-comment hint:\n%s", body)
	}
	rendered := string(body)[:open] + string(body)[close+len("-->"):]
	if got := strings.TrimSpace(rendered); got != "# Chain" {
		t.Errorf("unseeded note should render as just a heading, got %q from:\n%s", got, body)
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
