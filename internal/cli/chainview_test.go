package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// chainBatch mints a head plus two parked fix runs chained behind it,
// returning (head, first, second).
func chainBatch(t *testing.T, root string) (string, string, string) {
	t.Helper()
	spawnAndHead(t, root, "moe", "pulse-2026-07-19", "batch", []pulseRunSpec{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, os.Stderr)

	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain heads = %v, want 1", heads)
	}
	var first, second string
	for _, id := range runsWithWorkflow(t, root, "moe", "sdlc") {
		switch {
		case strings.HasPrefix(id, "fix-one"):
			first = id
		case strings.HasPrefix(id, "fix-two"):
			second = id
		}
	}
	if first == "" || second == "" {
		t.Fatalf("could not identify batch members: first=%q second=%q", first, second)
	}
	return heads[0], first, second
}

// TestChainMembersWalksTheBatch: the head page's members list is the
// chain in order, head→tail — the same runs `moe chain kick` would
// ride. This is the live truth that replaced the canvas's frozen
// membership lines.
func TestChainMembersWalksTheBatch(t *testing.T) {
	root := spawnFixture(t)
	head, first, second := chainBatch(t, root)

	members, err := chainMembers(root, "moe", head, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %+v, want 2", members)
	}
	if members[0].Run != first || members[1].Run != second {
		t.Errorf("members = [%s %s], want [%s %s] in chain order",
			members[0].Run, members[1].Run, first, second)
	}
	if members[0].Note == "" {
		t.Error("members should carry the dash's own note vocabulary")
	}
}

// TestChainMembersStopsAtATerminalMember: the walk stops exactly where
// maybeRideChain stops. A shipped or closed member ends the ride, so it
// ends the list too — the page must show the batch that will actually
// walk, not every edge ever stamped. The runs behind it keep their
// edges; `moe chain edit` is what re-strings them.
func TestChainMembersStopsAtATerminalMember(t *testing.T) {
	root := spawnFixture(t)
	head, first, _ := chainBatch(t, root)

	md, err := run.Load(root, "moe", first)
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusClosed
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	members, err := chainMembers(root, "moe", head, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Errorf("members = %+v, want none — the ride stops at the closed member", members)
	}
}
