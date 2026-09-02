package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// writePulseGateCanvas plants a survey canvas carrying a `## Gate` fence
// with the given spawn entries — the on-disk artifact the provenance
// walk reads a recorded reason back out of.
func writePulseGateCanvas(t *testing.T, root, projectID, pulseSlug, gateJSON string) {
	t.Helper()
	canvas := filepath.Join(root, run.ContentPath(projectID, pulseSlug, pulseDoc))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Pulse\n\n## Gate\n\n```json\n" + gateJSON + "\n```\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Land it: the spawn path refuses to mint against a dirty tree, and a
	// real survey's canvas is committed by its own work turn anyway.
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "work: update "+pulseDoc)
}

// TestRunProvenanceNamesTheSpawnerAndItsReason: the headline case. A
// pulse-spawned run's page must say who opened it, mark it as the
// machine's doing, and repeat the reason the survey recorded — the whole
// point being that none of this should need a journal-archaeology
// session.
func TestRunProvenanceNamesTheSpawnerAndItsReason(t *testing.T) {
	root := spawnFixture(t)
	// The pulse is a real run an operator opened — the top of the chain
	// the page is meant to draw.
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-2026-07-20", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}
	writePulseGateCanvas(t, root, "moe", "pulse-2026-07-20",
		`{"status":"ok","loose":[{"slug":"fix-ci-red-main","title":"Fix CI","why":"TestX failing since abc123"}]}`)
	spawnAndHead(t, root, "moe", "pulse-2026-07-20", "batch", []pulseRunSpec{
		{Slug: "fix-ci-red-main", Title: "Fix CI", Why: "TestX failing since abc123"},
	}, os.Stderr)

	var child string
	for _, id := range runsWithWorkflow(t, root, "moe", "sdlc") {
		if strings.HasPrefix(id, "fix-ci-red-main") {
			child = id
		}
	}
	if child == "" {
		t.Fatal("no spawned run to walk")
	}

	hops, err := runProvenance(root, "moe", child)
	if err != nil {
		t.Fatal(err)
	}
	// Root first: the operator who opened the pulse, the pulse opening,
	// then the spawn that landed on this run.
	if len(hops) != 3 {
		t.Fatalf("hops = %+v, want 3 (root actor, pulse, this run)", hops)
	}
	if hops[0].Subject != "operator" || hops[0].Verb != "" {
		t.Errorf("root hop = %+v, want the bare actor \"operator\"", hops[0])
	}
	if hops[1].Verb != "opened" || hops[1].Object != "moe/pulse-2026-07-20" {
		t.Errorf("hop 1 = %q %q, want \"opened\" moe/pulse-2026-07-20", hops[1].Verb, hops[1].Object)
	}
	if hops[1].Subject != "" {
		t.Errorf("hop 1 Subject = %q, want empty — the arrow carries it from the line above", hops[1].Subject)
	}
	// The pulse is on disk, so its name is a link: the page it points at
	// exists and is the next thing a reader wants.
	if hops[1].ObjectURL != "/run/moe/pulse-2026-07-20" {
		t.Errorf("hop 1 ObjectURL = %q, want the spawner's run page", hops[1].ObjectURL)
	}
	last := hops[2]
	if last.Verb != "spawned" || last.Object != "this run" || last.ObjectURL != "" {
		t.Errorf("last hop = %+v, want \"spawned\" an unlinked \"this run\"", last)
	}
	if !last.Agent {
		t.Error("a spawned run's opening hop must be marked agent")
	}
	// The fixture never enters withRideMode, so the recorded consent is
	// the bare "none" — a machine turn with no ride in flight. Present,
	// not absent: that distinction is the trailer's whole job.
	if last.Consent != "none" {
		t.Errorf("spawn hop Consent = %q, want \"none\"", last.Consent)
	}
	if last.Why != "TestX failing since abc123" {
		t.Errorf("spawn hop Why = %q, want the gate's recorded reason", last.Why)
	}
}

// TestRunProvenanceDeadEndChainStartsMidStory: a spawner whose own
// origin nobody recorded gets no invented root actor. The chain names
// the oldest run it can still stand behind and starts there — the
// honesty rule again, this time by saying less.
func TestRunProvenanceDeadEndChainStartsMidStory(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-2026-07-20", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}
	spawnAndHead(t, root, "moe", "pulse-2026-07-20", "batch", []pulseRunSpec{
		{Slug: "orphan-chain", Title: "Fix"},
	}, io.Discard)

	var child string
	for _, id := range runsWithWorkflow(t, root, "moe", "sdlc") {
		if strings.HasPrefix(id, "orphan-chain") {
			child = id
		}
	}
	if child == "" {
		t.Fatal("no spawned run to walk")
	}
	// Prune the spawner: its run.json is how the walk would learn that
	// an operator opened it, and nothing else records that.
	if err := os.RemoveAll(filepath.Join(root, run.Dir("moe", "pulse-2026-07-20"))); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", child)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %+v, want 2 (the named spawner, then this run)", hops)
	}
	if hops[0].Subject != "moe/pulse-2026-07-20" || hops[0].Verb != "" {
		t.Errorf("root hop = %+v, want the spawner named with no origin claim", hops[0])
	}
	// Naming the pruned spawner is honest; linking it is not — the root
	// line is the first thing a reader clicks, and that page is gone.
	if hops[0].SubjectURL != "" {
		t.Errorf("root hop SubjectURL = %q, want no link to a pruned run", hops[0].SubjectURL)
	}
	if hops[1].Verb != "spawned" || hops[1].Object != "this run" {
		t.Errorf("hop 1 = %+v, want \"spawned\" \"this run\"", hops[1])
	}
	for _, h := range hops {
		if h.Verb == "opened by operator" || h.Subject == "operator" {
			t.Errorf("hops = %+v, must not invent an operator for a pruned origin", hops)
		}
	}
}

// TestRunProvenanceUnlinksAPrunedMidChainRun: a prune in the middle of
// the chain doesn't stop the walk — the journal still carries the spawn
// record — so the pruned run renders as an object line. Naming it is
// fine; linking it would put the same 404 one click deeper than the root
// case. Its neighbours keep their links.
func TestRunProvenanceUnlinksAPrunedMidChainRun(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-2026-07-20", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}
	spawnAndHead(t, root, "moe", "pulse-2026-07-20", "batch", []pulseRunSpec{
		{Slug: "mid-chain", Title: "Fix"},
	}, io.Discard)

	var mid string
	for _, id := range runsWithWorkflow(t, root, "moe", "sdlc") {
		if strings.HasPrefix(id, "mid-chain") {
			mid = id
		}
	}
	if mid == "" {
		t.Fatal("no spawned run to hang a child off")
	}
	// A run the middle one spawned in turn. The trailer is what the
	// journal index reads, so it rides the open commit alongside the
	// metadata field — the pairing every machine-spawn path writes.
	if _, err := run.New(root, "moe", run.Options{
		ID: "leaf", Workflow: "sdlc",
		SpawnedBy: "moe/" + mid,
		Trailers:  trailers.Block{SpawnedBy: "moe/" + mid},
	}); err != nil {
		t.Fatal(err)
	}
	// Prune the middle run only. Its MoE-Spawned-By trailer survives in
	// the journal, so the walk passes straight through it up to the pulse.
	if err := os.RemoveAll(filepath.Join(root, run.Dir("moe", mid))); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "leaf")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 4 {
		t.Fatalf("hops = %+v, want 4 (operator, pulse, pruned run, this run)", hops)
	}
	if hops[1].Object != "moe/pulse-2026-07-20" || hops[1].ObjectURL != "/run/moe/pulse-2026-07-20" {
		t.Errorf("hop 1 = %+v, want the on-disk pulse still linked", hops[1])
	}
	if hops[2].Object != "moe/"+mid {
		t.Fatalf("hop 2 = %+v, want the pruned run named", hops[2])
	}
	if hops[2].ObjectURL != "" {
		t.Errorf("hop 2 ObjectURL = %q, want no link to a pruned run", hops[2].ObjectURL)
	}
}

// TestRunProvenanceDegradesWithNoGateCanvas: the spawner's canvas is the
// only place a spawn reason lives, and it is a file an operator can edit
// or a prune can remove. A missing or unparseable gate must cost the hop
// its reason and nothing else — no error, no dropped hop, no page 500.
func TestRunProvenanceDegradesWithNoGateCanvas(t *testing.T) {
	root := spawnFixture(t)
	// No writePulseGateCanvas call: the spawner has no canvas at all.
	spawnAndHead(t, root, "moe", "pulse-2026-07-20", "batch", []pulseRunSpec{
		{Slug: "fix-orphaned", Title: "Fix"},
	}, io.Discard)

	var child string
	for _, id := range runsWithWorkflow(t, root, "moe", "sdlc") {
		if strings.HasPrefix(id, "fix-orphaned") {
			child = id
		}
	}
	if child == "" {
		t.Fatal("no spawned run to walk")
	}

	hops, err := runProvenance(root, "moe", child)
	if err != nil {
		t.Fatalf("a missing gate canvas must not fail the walk: %v", err)
	}
	if len(hops) == 0 {
		t.Fatal("the spawn hop must survive a missing gate")
	}
	spawn := hops[len(hops)-1]
	if spawn.Verb != "spawned" || spawn.Object != "this run" || !spawn.Agent {
		t.Errorf("hop = %+v, want the spawn still landed on this run and marked agent", spawn)
	}
	if spawn.Why != "" {
		t.Errorf("hop Why = %q, want empty — the reason is unrecoverable", spawn.Why)
	}
}

// TestRunProvenanceOperatorOpenedRun: the terminal claim. No operator
// verb writes any of the marks the arms above it read, so an open commit
// carrying none of them is the one origin the walk may state positively.
func TestRunProvenanceOperatorOpenedRun(t *testing.T) {
	root := spawnFixture(t)
	md, err := run.New(root, "moe", run.Options{ID: "hand-opened", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 {
		t.Fatalf("hops = %+v, want exactly 1", hops)
	}
	if hops[0].Verb != "opened by operator" {
		t.Errorf("hop Verb = %q, want \"opened by operator\"", hops[0].Verb)
	}
	if hops[0].Agent || hops[0].Consent != "" {
		t.Errorf("hop = %+v, want no agent marker and no consent claim", hops[0])
	}
}

// TestChainMembersCarryEdgeAttribution: a groomed batch's members each
// know the machine placed them there. This is the second half of the
// "what did the agent add" question — a run can be operator-opened and
// still be sequenced by a pulse.
func TestChainMembersCarryEdgeAttribution(t *testing.T) {
	root := spawnFixture(t)
	head, _, _ := chainBatch(t, root)

	members, err := chainMembers(root, "moe", head, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %+v, want 2", members)
	}
	for i, m := range members {
		if !m.EdgeAgent {
			t.Errorf("member %d (%s) EdgeAgent = false, want true — a groom placed it", i, m.Run)
		}
		if m.EdgeConsent != "none" {
			t.Errorf("member %d (%s) EdgeConsent = %q, want \"none\"", i, m.Run, m.EdgeConsent)
		}
	}
}

// harvestedIdea opens an idea run the way a followups harvest does: the
// MoE-From-Run trailer on the open commit names the run whose scratch
// file the entry came out of, and MoE-Consent records the ride the close
// that harvested it was walking under.
func harvestedIdea(t *testing.T, root, projectID, ideaID, sourceRun, consent string) {
	t.Helper()
	if _, err := run.New(root, projectID, run.Options{
		ID: ideaID, Workflow: "idea",
		Trailers: trailers.Block{FromRun: sourceRun, Consent: consent},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRunProvenanceNamesTheRunACapturedIdeaCameFrom: the operator's
// reported shape. A followup an agent filed in one run, harvested into
// an idea by that run's close, used to render as "opened by operator" —
// the one fact the reader wanted was on the open commit and dropped at
// the index.
func TestRunProvenanceNamesTheRunACapturedIdeaCameFrom(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "design-only-spawn", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	harvestedIdea(t, root, "moe", "tag-on-ideas", "moe/design-only-spawn", "none")

	hops, err := runProvenance(root, "moe", "tag-on-ideas")
	if err != nil {
		t.Fatal(err)
	}
	// The walk doesn't stop at the harvesting run: how *that* run opened
	// is the rest of the same story, so an operator-opened source puts the
	// operator back at the top where they belong.
	if len(hops) != 3 {
		t.Fatalf("hops = %+v, want 3 (operator, the source run, the capture)", hops)
	}
	if hops[0].Subject != "operator" {
		t.Errorf("root hop = %+v, want the operator who opened the source run", hops[0])
	}
	if hops[1].Verb != "opened" || hops[1].Object != "moe/design-only-spawn" {
		t.Errorf("hop 1 = %q %q, want \"opened\" moe/design-only-spawn", hops[1].Verb, hops[1].Object)
	}
	capture := hops[2]
	if capture.Verb != "captured" || capture.Object != "this run" {
		t.Errorf("capture hop = %+v, want \"captured\" \"this run\"", capture)
	}
	if !capture.Agent {
		t.Error("a harvested idea's opening hop must be marked agent — a followup entry is an agent's writing")
	}
	// "none" is a real answer, not a missing one: a bang cascade did the
	// close, with no ride licensed. Distinct from no chip at all, which
	// is what an operator closing the run by hand leaves.
	if capture.Consent != "none" {
		t.Errorf("capture hop Consent = %q, want \"none\"", capture.Consent)
	}
}

// TestRunProvenanceCaptureNamesAPrunedSourceWithoutALink: the honesty
// rule on the new edge. A pruned harvesting run is still worth naming —
// it is the whole answer to "where did this come from" — but its page is
// gone, so the name carries no link.
func TestRunProvenanceCaptureNamesAPrunedSourceWithoutALink(t *testing.T) {
	root := spawnFixture(t)
	harvestedIdea(t, root, "moe", "tag-on-ideas", "moe/long-gone", "none")

	hops, err := runProvenance(root, "moe", "tag-on-ideas")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %+v, want 2 (the named source, then this run)", hops)
	}
	if hops[0].Subject != "moe/long-gone" {
		t.Errorf("root hop = %+v, want the source run named", hops[0])
	}
	if hops[0].SubjectURL != "" {
		t.Errorf("root hop SubjectURL = %q, want no link to a pruned run", hops[0].SubjectURL)
	}
	if hops[1].Verb != "captured" || !hops[1].Agent || hops[1].Consent != "none" {
		t.Errorf("capture hop = %+v, want \"captured\" marked agent at \"none\"", hops[1])
	}
}

// TestRunProvenanceWalksSpawnThenCaptureThenPromote: the long shape the
// design draws. A pulse spawns a run, an agent in that run files a
// followup, the close harvests it into an idea, a later sweep promotes
// the idea. Every hop of that is on disk, and the page tells it as one
// story rather than four disconnected facts.
func TestRunProvenanceWalksSpawnThenCaptureThenPromote(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-2026-09-01", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{
		ID: "design-only-spawn", Workflow: "sdlc",
		SpawnedBy: "moe/pulse-2026-09-01",
		Trailers:  trailers.Block{SpawnedBy: "moe/pulse-2026-09-01", Consent: "dynamic"},
	}); err != nil {
		t.Fatal(err)
	}
	harvestedIdea(t, root, "moe", "tag-on-ideas", "moe/design-only-spawn", "none")
	// The destination run, and the idea's own status bump that records
	// the promotion. Both carry the sweep's consent, as a real kick's do.
	if _, err := run.New(root, "moe", run.Options{
		ID: "tag-on-ideas-run", Workflow: "sdlc",
		Trailers: trailers.Block{Idea: "tag-on-ideas", Consent: "dynamic"},
	}); err != nil {
		t.Fatal(err)
	}
	journalCommit(t, root, "moe", "Promote idea moe/tag-on-ideas → moe/tag-on-ideas-run",
		"MoE-Run: tag-on-ideas\nMoE-Project: moe\nMoE-Promoted-To: moe/tag-on-ideas-run\nMoE-Consent: dynamic")

	hops, err := runProvenance(root, "moe", "tag-on-ideas-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 5 {
		t.Fatalf("hops = %+v, want 5 (operator, pulse, spawn, capture, promote)", hops)
	}
	want := []struct{ verb, object string }{
		{"", ""},
		{"opened", "moe/pulse-2026-09-01"},
		{"spawned", "moe/design-only-spawn"},
		{"captured", "moe/tag-on-ideas"},
		{"promoted to", "this run"},
	}
	for i, w := range want {
		if hops[i].Verb != w.verb || hops[i].Object != w.object {
			t.Errorf("hop %d = %q %q, want %q %q", i, hops[i].Verb, hops[i].Object, w.verb, w.object)
		}
	}
	if hops[0].Subject != "operator" {
		t.Errorf("root hop = %+v, want the operator who opened the pulse", hops[0])
	}
	promote := hops[4]
	if !promote.Agent || promote.Consent != "dynamic" {
		t.Errorf("promote hop = %+v, want the sweep's mark and its dynamic ride", promote)
	}
}

// TestRunProvenanceHandPromotedIdeaKeepsItsOperator: the cost of walking
// through a promote, and the check that it is only a cost. An idea the
// operator typed and promoted by hand gains a line — but no agent badge
// and no consent word, because nothing machine-authored is on either
// commit.
func TestRunProvenanceHandPromotedIdeaKeepsItsOperator(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "an-idea", Workflow: "idea"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{ID: "an-idea-run", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	journalCommit(t, root, "moe", "Promote idea moe/an-idea → moe/an-idea-run",
		"MoE-Run: an-idea\nMoE-Project: moe\nMoE-Promoted-To: moe/an-idea-run")

	hops, err := runProvenance(root, "moe", "an-idea-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 3 {
		t.Fatalf("hops = %+v, want 3 (operator, the idea, the promote)", hops)
	}
	if hops[0].Subject != "operator" || hops[1].Object != "moe/an-idea" {
		t.Errorf("hops = %+v, want the operator who typed the idea named above it", hops[:2])
	}
	if hops[2].Verb != "promoted to" || hops[2].Agent || hops[2].Consent != "" {
		t.Errorf("promote hop = %+v, want no agent marker and no consent claim", hops[2])
	}
}

// TestRunProvenanceDynamicPulseSurveyOpensAsAMachineWalk: a survey a
// dynamic sweep minted records no spawner — it *is* the sweep's first
// act — so consent standing alone is the whole mark, and it used to read
// as the operator's own work.
func TestRunProvenanceDynamicPulseSurveyOpensAsAMachineWalk(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{
		ID: "pulse-swept", Workflow: "pulse",
		Trailers: trailers.Block{Consent: "dynamic"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-by-hand", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "pulse-swept")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 {
		t.Fatalf("hops = %+v, want the one-line story", hops)
	}
	// The heartbeat flag isn't on the commit, so the page can name the
	// walk but not the clock behind it — the same phrase, for the same
	// reason, that the ship hop uses.
	if hops[0].Verb != "opened by a machine walk" || !hops[0].Agent || hops[0].Consent != "dynamic" {
		t.Errorf("hop = %+v, want \"opened by a machine walk\" marked agent at \"dynamic\"", hops[0])
	}

	// Same run shape, no consent trailer: `moe pulse` typed at a
	// terminal, and still the operator's.
	byHand, err := runProvenance(root, "moe", "pulse-by-hand")
	if err != nil {
		t.Fatal(err)
	}
	if len(byHand) != 1 || byHand[0].Verb != "opened by operator" {
		t.Errorf("hops = %+v, want the unstamped survey still \"opened by operator\"", byHand)
	}
}

// writeChoreDefinition plants a chore.json so the chore key the journal
// carries has a page to link to.
func writeChoreDefinition(t *testing.T, root, projectID, name string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "chores", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chore.json"), []byte(`{"cadence":"7d"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "register chore "+projectID+"/"+name)
}

// TestRunProvenanceChoreRunNamesItsChore: a run the heartbeat opened for
// a due chore. The cause is the chore, which has its own page, and the
// consent on the open commit is what separates a clock-fired run from
// `moe chore open` typed by hand.
func TestRunProvenanceChoreRunNamesItsChore(t *testing.T) {
	root := spawnFixture(t)
	writeChoreDefinition(t, root, "moe", "readme-refresh")
	// The run's own slug is the chore's name, which is the collision the
	// walk's chore-liveness answer has to stay out of the run map for.
	if _, err := run.New(root, "moe", run.Options{
		ID: "readme-refresh", Workflow: "sdlc",
		Trailers: trailers.Block{Chore: "moe/readme-refresh", Consent: "dynamic"},
	}); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "readme-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %+v, want 2 (the chore, then this run)", hops)
	}
	if hops[0].Subject != "moe/readme-refresh" {
		t.Errorf("root hop = %+v, want the chore named", hops[0])
	}
	// The chore's page, not the run's: a chore key never loads as a run.
	if hops[0].SubjectURL != "/chore/moe/readme-refresh" {
		t.Errorf("root hop SubjectURL = %q, want the chore page", hops[0].SubjectURL)
	}
	if hops[1].Verb != "opened" || hops[1].Object != "this run" || !hops[1].Agent || hops[1].Consent != "dynamic" {
		t.Errorf("chore hop = %+v, want \"opened\" \"this run\" marked agent at \"dynamic\"", hops[1])
	}
}

// TestRunProvenanceHandOpenedChoreRunNamesTheChoreUnbadged: `moe chore
// open` typed at a terminal. The chore is still what the run is for, so
// it is still named; nothing machine-authored caused it, so no badge.
func TestRunProvenanceHandOpenedChoreRunNamesTheChoreUnbadged(t *testing.T) {
	root := spawnFixture(t)
	writeChoreDefinition(t, root, "moe", "readme-refresh")
	if _, err := run.New(root, "moe", run.Options{
		ID: "readme-refresh-2", Workflow: "sdlc",
		Trailers: trailers.Block{Chore: "moe/readme-refresh"},
	}); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "readme-refresh-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %+v, want 2 (the chore, then this run)", hops)
	}
	if hops[0].Subject != "moe/readme-refresh" || hops[0].SubjectURL != "/chore/moe/readme-refresh" {
		t.Errorf("root hop = %+v, want the chore named and linked", hops[0])
	}
	if hops[1].Agent || hops[1].Consent != "" {
		t.Errorf("chore hop = %+v, want no agent marker and no consent claim", hops[1])
	}
}

// TestRunProvenanceRetiredChoreIsNamedWithoutALink: the honesty rule
// again. A chore whose definition has since been deleted 404s, so the
// key is named and not linked — the same trade the walk makes for a
// pruned run.
func TestRunProvenanceRetiredChoreIsNamedWithoutALink(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{
		ID: "retired-chore-run", Workflow: "sdlc",
		Trailers: trailers.Block{Chore: "moe/long-retired", Consent: "dynamic"},
	}); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "retired-chore-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("hops = %+v, want 2 (the chore, then this run)", hops)
	}
	if hops[0].Subject != "moe/long-retired" {
		t.Errorf("root hop = %+v, want the retired chore still named", hops[0])
	}
	if hops[0].SubjectURL != "" {
		t.Errorf("root hop SubjectURL = %q, want no link to a chore page that 404s", hops[0].SubjectURL)
	}
}

// TestRunProvenanceLinksALegacyBareSpawner: 21 runs minted before
// writers qualified `spawned_by` at the source carry a bare slug in
// both run.json and their open commit's trailer. The journal index
// qualifies the trailer with the run's own project; the walk has to
// take that answer rather than the raw field, or the spawn root is a
// key it can't load and so a name with no link.
func TestRunProvenanceLinksALegacyBareSpawner(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-2026-07-17", Workflow: "pulse"}); err != nil {
		t.Fatal(err)
	}
	// Bare in the field and in the trailer: exactly what the pre-July-18
	// pulse mint wrote.
	if _, err := run.New(root, "moe", run.Options{
		ID:        "legacy-bare-spawn",
		Workflow:  "sdlc",
		SpawnedBy: "pulse-2026-07-17",
		Trailers:  trailers.Block{SpawnedBy: "pulse-2026-07-17"},
	}); err != nil {
		t.Fatal(err)
	}

	hops, err := runProvenance(root, "moe", "legacy-bare-spawn")
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 3 {
		t.Fatalf("hops = %+v, want 3 (root actor, spawner, this run)", hops)
	}
	if hops[0].Subject != "operator" {
		t.Errorf("root hop = %+v, want the operator who opened the spawner", hops[0])
	}
	if hops[1].Object != "moe/pulse-2026-07-17" {
		t.Errorf("hop 1 Object = %q, want the spawner qualified with its project", hops[1].Object)
	}
	// The point of the fix: a bare key loads nothing, so the spawner
	// would be named without a link even though its page is right there.
	if hops[1].ObjectURL != "/run/moe/pulse-2026-07-17" {
		t.Errorf("hop 1 ObjectURL = %q, want the spawner's run page", hops[1].ObjectURL)
	}
	if !hops[2].Agent {
		t.Error("the spawn hop must still be marked agent")
	}
}
