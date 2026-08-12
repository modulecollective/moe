package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/serve"
)

// writeServeState plants a state file the way a running serve would.
func writeServeState(t *testing.T, root string, snap serve.ActivitySnapshot) {
	t.Helper()
	if err := os.MkdirAll(root+"/.moe", 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serve.ActivityPath(root), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPid returns a pid that has certainly exited, so the stale-serve leg
// can be driven without guessing at an unused number.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func serveLines(t *testing.T, root string, now time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	renderServeLines(&buf, now, root)
	return buf.String()
}

// TestDashSaysNothingWithoutAServe is the line budget's floor: an operator
// who doesn't run serve sees the dash they saw before this change.
func TestDashSaysNothingWithoutAServe(t *testing.T) {
	if got := serveLines(t, t.TempDir(), time.Now()); got != "" {
		t.Errorf("dash printed %q with no serve running, want nothing at all", got)
	}
}

// TestDashShowsAnArmedServe: one line for the process — may it pulse, how
// long has it been up, when does the next tick land.
func TestDashShowsAnArmedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true,
		Started:  now.Add(-3*24*time.Hour - 2*time.Hour),
		NextTick: now.Add(12 * time.Minute),
	})

	got := serveLines(t, root, now)
	for _, want := range []string{"serve:", "armed", "up 3d 2h", "next sweep in 12m"} {
		if !strings.Contains(got, want) {
			t.Errorf("serve line %q is missing %q", got, want)
		}
	}
}

// TestDashShowsAnUnarmedServe: a serve that is up and will never pulse is
// the confusion the panel exists to fix, on this surface too.
func TestDashShowsAnUnarmedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{Pid: os.Getpid(), Started: now.Add(-time.Hour)})

	got := serveLines(t, root, now)
	if !strings.Contains(got, "browse-only (unarmed)") {
		t.Errorf("serve line = %q, want the unarmed spelling", got)
	}
	if strings.Contains(got, "next sweep") {
		t.Errorf("serve line = %q promised a sweep an unarmed serve never runs", got)
	}
}

// TestDashShowsACrashedServe: clean shutdown removes the file, so a file
// naming a dead pid is a serve that died — which used to be indis-
// tinguishable from never having run one.
func TestDashShowsACrashedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	pid := deadPid(t)
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: pid, Armed: true, Started: now.Add(-time.Hour), WrittenAt: now.Add(-90 * time.Minute),
	})

	got := serveLines(t, root, now)
	for _, want := range []string{"dead", strconv.Itoa(pid), "stale since 1h ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("serve line %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "armed") {
		t.Errorf("serve line = %q, want a dead serve to stop claiming it will pulse", got)
	}
}

// TestDashSpendsProjectLinesOnlyOnNoteworthyProjects is the rest of the
// budget: a quiet board stays quiet. A project earns a line when it is
// sweeping, cooling, or its last sweep died — and not otherwise.
func TestDashSpendsProjectLinesOnlyOnNoteworthyProjects(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true, Started: now.Add(-time.Hour), NextTick: now.Add(time.Minute),
		Projects: []serve.ActivityProject{
			{Project: "sweeper", Decision: "the journal moved", Sweep: true,
				SweepStarted: now.Add(-3 * time.Minute)},
			{Project: "cooler", Fails: 2, CoolTicks: 2, SweptAt: now.Add(-40 * time.Minute)},
			{Project: "faller", Decision: "the journal moved", SweptAt: now.Add(-5 * time.Minute)},
			{Project: "quiet", Decision: "a sweep already surveyed the current tip"},
			{Project: "cleanly", Decision: "the journal moved",
				SweptAt: now.Add(-time.Hour), SweepClean: true},
		},
	})

	got := serveLines(t, root, now)
	for _, want := range []string{
		"sweeper  sweeping",
		"cooler   cooling   (2 ticks left)",
		"faller   failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("serve lines are missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"quiet", "cleanly"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a project with nothing to report (%s) spent a line:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "last sweep failed 40m ago") {
		t.Errorf("the cooling line should date the failure that earned it:\n%s", got)
	}
}

// TestDashSurvivesATruncatedStateFile: a dashboard that refused to draw
// because a monitor file was half-written would have the priorities
// backwards.
func TestDashSurvivesATruncatedStateFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/.moe", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serve.ActivityPath(root), []byte("{half"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := serveLines(t, root, time.Now())
	if !strings.HasPrefix(got, "serve:") || strings.Count(got, "\n") != 1 {
		t.Errorf("serve lines = %q, want one warning line", got)
	}
}
