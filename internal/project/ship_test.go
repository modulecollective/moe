package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestShipOfDefaultsToPR: absent means pr, and so does anything a hand
// edit could leave behind. The fail-safe direction is the reversible
// route — a PR can be closed unmerged, a merge has landed.
func TestShipOfDefaultsToPR(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   *Metadata
		want ShipMode
	}{
		{"absent", &Metadata{ID: "alpha"}, ShipPR},
		{"explicit pr", &Metadata{ID: "alpha", Ship: "pr"}, ShipPR},
		{"merge", &Metadata{ID: "alpha", Ship: "merge"}, ShipMerge},
		{"junk", &Metadata{ID: "alpha", Ship: "yolo"}, ShipPR},
		{"nil", nil, ShipPR},
	} {
		if got := ShipOf(tc.md); got != tc.want {
			t.Errorf("%s: ShipOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseShipRejectsAnythingElse(t *testing.T) {
	for _, ok := range []string{"pr", "merge"} {
		if _, err := ParseShip(ok); err != nil {
			t.Errorf("ParseShip(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "PR", "ff", "rebase", "auto"} {
		if _, err := ParseShip(bad); err == nil {
			t.Errorf("ParseShip(%q) accepted", bad)
		}
	}
}

func TestSetShipWritesAndCommits(t *testing.T) {
	root := seedProject(t, bareProjectJSON)

	if err := SetShip(root, "alpha", ShipMerge); err != nil {
		t.Fatalf("SetShip: %v", err)
	}
	if got := ReadShip(root, "alpha"); got != ShipMerge {
		t.Fatalf("ReadShip = %q, want merge", got)
	}
	subject := strings.TrimSpace(gittest.Output(t, root, "log", "-1", "--format=%s"))
	if subject != "Set project alpha ship: merge" {
		t.Errorf("commit subject = %q", subject)
	}
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, banned := range []string{"MoE-Consent:", "MoE-Spawned-By:"} {
		if strings.Contains(body, banned) {
			t.Errorf("commit carries %s; a ship flip is the operator's:\n%s", banned, body)
		}
	}
	if out := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("SetShip left the tree dirty:\n%s", out)
	}
}

// TestSetShipPRDropsTheField: pr is the absent state, not a value —
// same anti-drift rule as mode's auto.
func TestSetShipPRDropsTheField(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	if err := SetShip(root, "alpha", ShipMerge); err != nil {
		t.Fatal(err)
	}
	if err := SetShip(root, "alpha", ShipPR); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "projects", "alpha", "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ship") {
		t.Errorf("project.json still carries a ship key at pr:\n%s", raw)
	}
	if got := ReadShip(root, "alpha"); got != ShipPR {
		t.Errorf("ReadShip = %q, want pr", got)
	}
}

// TestSetShipIsIdempotent: re-setting the route a project already has
// mints no commit, so a double-tap on the hub switch doesn't move the
// tip and wake a sweep.
func TestSetShipIsIdempotent(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	before := gittest.HeadSHA(t, root)
	if err := SetShip(root, "alpha", ShipPR); err != nil {
		t.Fatalf("SetShip: %v", err)
	}
	if got := gittest.HeadSHA(t, root); got != before {
		t.Error("setting the route a project already has minted a commit")
	}
}

func TestSetShipRejectsJunk(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	if err := SetShip(root, "alpha", ShipMode("yolo")); err == nil {
		t.Error("SetShip accepted an unknown route")
	}
	if err := SetShip(root, "nope", ShipMerge); err == nil {
		t.Error("SetShip accepted an unregistered project")
	}
}

// TestShipAndModeAreOrthogonal: setting one leaves the other alone.
// They answer different questions — what the clock may start, and how a
// finished run lands — and project.json holds both.
func TestShipAndModeAreOrthogonal(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	if err := SetMode(root, "alpha", ModeSafe); err != nil {
		t.Fatal(err)
	}
	if err := SetShip(root, "alpha", ShipMerge); err != nil {
		t.Fatal(err)
	}
	md, err := Load(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if ModeOf(md) != ModeSafe {
		t.Errorf("ModeOf = %q after SetShip, want safe", ModeOf(md))
	}
	if ShipOf(md) != ShipMerge {
		t.Errorf("ShipOf = %q, want merge", ShipOf(md))
	}
}
