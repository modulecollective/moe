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
	if hops[1].Verb != "spawned" || hops[1].Object != "this run" {
		t.Errorf("hop 1 = %+v, want \"spawned\" \"this run\"", hops[1])
	}
	for _, h := range hops {
		if h.Verb == "opened by operator" || h.Subject == "operator" {
			t.Errorf("hops = %+v, must not invent an operator for a pruned origin", hops)
		}
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

// TestRunProvenanceOperatorOpenedRun: the terminal claim. Only machine
// verbs have ever written spawned_by, so its absence is the one thing
// the walk may state positively about a run's origin.
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

	members, _, err := chainMembers(root, "moe", head, time.Now().UTC())
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
