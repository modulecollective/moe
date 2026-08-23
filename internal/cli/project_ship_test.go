package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/project"
)

// TestProjectShipVerbReadsAndSets: same shape as `project mode` — no
// argument asks, an argument tells. The read matters more here than for
// mode: every unflagged ship in the bureaucracy now answers to this.
func TestProjectShipVerbReadsAndSets(t *testing.T) {
	root := spawnFixture(t)

	var out, errb bytes.Buffer
	if code := Run([]string{"project", "ship", "moe"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := out.String(); got != "moe: pr\n" {
		t.Errorf("read on a fresh project = %q, want pr", got)
	}

	out.Reset()
	if code := Run([]string{"project", "ship", "moe", "merge"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := project.ReadShip(root, "moe"); got != project.ShipMerge {
		t.Fatalf("ReadShip = %q, want merge", got)
	}

	out.Reset()
	if code := Run([]string{"project", "ship", "moe"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := out.String(); got != "moe: merge\n" {
		t.Errorf("read after the set = %q, want merge", got)
	}

	// Re-setting says so and mints nothing — the hub switch double-taps.
	out.Reset()
	if code := Run([]string{"project", "ship", "moe", "merge"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := out.String(); got != "moe: merge (unchanged)\n" {
		t.Errorf("re-set = %q, want the unchanged note", got)
	}
}

func TestProjectShipVerbRefusesJunk(t *testing.T) {
	root := spawnFixture(t)
	for _, args := range [][]string{
		{"project", "ship", "moe", "ff"},
		{"project", "ship", "moe", "PR"},
		{"project", "ship"},
		{"project", "ship", "moe", "merge", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code == 0 {
			t.Errorf("%v exited 0; stdout=%q", args, out.String())
		}
	}
	if got := project.ReadShip(root, "moe"); got != project.ShipPR {
		t.Errorf("a rejected route must not have been written: %q", got)
	}
}

// TestProjectListShowsShip: the index the operator scans to compare
// projects has to answer "which of these merges straight to main".
func TestProjectListShowsShip(t *testing.T) {
	root := spawnFixture(t)
	if err := project.SetShip(root, "moe", project.ShipMerge); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"project", "list"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasPrefix(line, "moe\tauto\tmerge\t") {
		t.Errorf("list row should carry id, mode, ship: %q", line)
	}
}
