package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// seedProject writes a minimal project.json under a git-backed root and
// returns the root. Enough for the mode reads and writes, which touch
// nothing else about a project.
func seedProject(t *testing.T, body string) string {
	t.Helper()
	gittest.SetupEnv(t)
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	dir := filepath.Join(root, "projects", "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "register alpha")
	return root
}

const bareProjectJSON = `{"id":"alpha","submodule":"projects/alpha/src",` +
	`"remote":"git@example.test:alpha.git","default_branch":"main"}`

// TestModeOfDefaultsToAuto is the whole migration story: a project.json
// written before modes existed keeps today's behaviour with no edit.
func TestModeOfDefaultsToAuto(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   *Metadata
		want Mode
	}{
		{"absent", &Metadata{ID: "alpha"}, ModeAuto},
		{"explicit auto", &Metadata{ID: "alpha", Mode: "auto"}, ModeAuto},
		{"safe", &Metadata{ID: "alpha", Mode: "safe"}, ModeSafe},
		{"paused", &Metadata{ID: "alpha", Mode: "paused"}, ModePaused},
		// A hand-edited typo cannot reach one of the restrictive modes:
		// freezing a project with nothing on any surface explaining why is
		// the worse failure, and the write path validates.
		{"junk", &Metadata{ID: "alpha", Mode: "snooze"}, ModeAuto},
		{"nil", nil, ModeAuto},
	} {
		if got := ModeOf(tc.md); got != tc.want {
			t.Errorf("%s: ModeOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseModeRejectsAnythingElse(t *testing.T) {
	for _, ok := range []string{"paused", "safe", "auto"} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q) = %v, want no error", ok, err)
		}
	}
	for _, bad := range []string{"", "AUTO", "Safe", "snooze", "off", "auto "} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) accepted an unknown mode", bad)
		}
	}
}

// TestSetModeWritesAndCommits: the mode is journal state, and the commit
// is what moves the project's tip — which is what wakes the heartbeat's
// moved leg on the next tick, so newly licensed work starts without
// anyone remembering to pulse.
func TestSetModeWritesAndCommits(t *testing.T) {
	root := seedProject(t, bareProjectJSON)

	if err := SetMode(root, "alpha", ModeSafe); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	mode, err := ReadMode(root, "alpha")
	if err != nil || mode != ModeSafe {
		t.Fatalf("ReadMode = %q, %v; want safe", mode, err)
	}
	subject := strings.TrimSpace(gittest.Output(t, root, "log", "-1", "--format=%s"))
	if subject != "Set project alpha mode: safe" {
		t.Errorf("commit subject = %q", subject)
	}
	// A mode flip is an operator act, so it carries no machine trailers —
	// which is what lets every journal consumer read it as one.
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, banned := range []string{"MoE-Consent:", "MoE-Spawned-By:"} {
		if strings.Contains(body, banned) {
			t.Errorf("commit carries %s; a mode flip is the operator's:\n%s", banned, body)
		}
	}
	if out := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("SetMode left the tree dirty:\n%s", out)
	}
}

// TestSetModeAutoDropsTheField: auto is the absent state, not a value.
// Writing it out would leave two spellings of the default for every
// surface that reads the file directly.
func TestSetModeAutoDropsTheField(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	if err := SetMode(root, "alpha", ModePaused); err != nil {
		t.Fatal(err)
	}
	if err := SetMode(root, "alpha", ModeAuto); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "projects", "alpha", "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mode") {
		t.Errorf("project.json still carries a mode key at auto:\n%s", raw)
	}
	if mode, _ := ReadMode(root, "alpha"); mode != ModeAuto {
		t.Errorf("ReadMode = %q, want auto", mode)
	}
}

// TestSetModeIsIdempotent: re-setting the mode a project already has
// writes nothing, so a double-tap on the hub switch doesn't mint an empty
// commit — and doesn't move the tip, which would trigger a sweep.
func TestSetModeIsIdempotent(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	before := gittest.HeadSHA(t, root)
	if err := SetMode(root, "alpha", ModeAuto); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if got := gittest.HeadSHA(t, root); got != before {
		t.Error("setting the mode a project already has minted a commit")
	}
}

func TestSetModeRejectsJunk(t *testing.T) {
	root := seedProject(t, bareProjectJSON)
	if err := SetMode(root, "alpha", Mode("snooze")); err == nil {
		t.Error("SetMode accepted an unknown mode")
	}
	if err := SetMode(root, "nope", ModeSafe); err == nil {
		t.Error("SetMode accepted an unregistered project")
	}
}
