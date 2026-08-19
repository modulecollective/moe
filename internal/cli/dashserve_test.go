package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
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

// serveCluster is what the banner's tail carries for this root.
func serveCluster(t *testing.T, root string, now time.Time) string {
	t.Helper()
	return readServeState(root).bannerCluster(now)
}

// serveLines is what serve earns *below* the banner.
func serveLines(t *testing.T, root string, now time.Time) string {
	t.Helper()
	var buf bytes.Buffer
	readServeState(root).renderLines(&buf, now)
	return buf.String()
}

// TestDashSaysNothingWithoutAServe is the line budget's floor: an operator
// who doesn't run serve sees the dash they saw before this change —
// nothing below the banner and nothing in its tail.
func TestDashSaysNothingWithoutAServe(t *testing.T) {
	root, now := t.TempDir(), time.Now()
	if got := serveCluster(t, root, now); got != "" {
		t.Errorf("banner tail = %q with no serve running, want nothing at all", got)
	}
	if got := serveLines(t, root, now); got != "" {
		t.Errorf("dash printed %q with no serve running, want nothing at all", got)
	}
}

// TestDashBannerCarriesAnArmedServe: the operator's three facts — may it
// pulse, how long has it been up, when does the next tick land — ride the
// banner's tail instead of a line of their own.
//
// The exact string is pinned because the web header renders the identical
// cluster (internal/serve's TestActivityPanelClusterMatchesTheCLIBanner
// holds the other half). Both sides route through dash.ServeCluster, and
// these two goldens are what say so.
func TestDashBannerCarriesAnArmedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true,
		Started:  now.Add(-3*24*time.Hour - 2*time.Hour),
		NextTick: now.Add(12 * time.Minute),
	})

	if got, want := serveCluster(t, root, now), "serve armed · up 3d 2h · next 12m"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
	if got := serveLines(t, root, now); got != "" {
		t.Errorf("a healthy quiet board spent %q below the banner, want nothing", got)
	}
}

// TestDashBannerOmitsAnUnscheduledSweep: an armed serve between ticks has
// no countdown to promise, so the fact is dropped rather than faked.
func TestDashBannerOmitsAnUnscheduledSweep(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true, Started: now.Add(-4 * time.Minute),
	})
	if got, want := serveCluster(t, root, now), "serve armed · up 4m"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
}

// TestDashBannerCarriesAnUnarmedServe: a serve that is up and will never
// pulse is the confusion this exists to fix, on this surface too.
func TestDashBannerCarriesAnUnarmedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{Pid: os.Getpid(), Started: now.Add(-time.Hour)})

	got := serveCluster(t, root, now)
	if want := "serve browse-only · up 1h 0m"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
	if strings.Contains(got, "next") {
		t.Errorf("banner tail = %q promised a sweep an unarmed serve never runs", got)
	}
}

// TestDashBannerCountsFailingProjects: the earned fourth fact. It
// summarises what the rows below detail, so a glance at the banner alone
// still says "something is wrong down there" — and stays silent when
// nothing is.
func TestDashBannerCountsFailingProjects(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true, Started: now.Add(-4 * time.Minute),
		NextTick: now.Add(15 * time.Minute),
		Projects: []serve.ActivityProject{
			// A live sweep is not a failure, even mid-cool-off: the row it
			// renders says "sweeping", and the count follows the rows.
			{Project: "sweeper", Sweep: true, CoolTicks: 1, SweepStarted: now.Add(-time.Minute)},
			{Project: "cooler", CoolTicks: 2, SweptAt: now.Add(-40 * time.Minute)},
			{Project: "faller", SweptAt: now.Add(-5 * time.Minute)},
			{Project: "quiet", Decision: "a sweep already surveyed the current tip"},
		},
	})
	if got, want := serveCluster(t, root, now), "serve armed · up 4m · next 15m · 2 failing"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
}

// TestDashShowsACrashedServe: clean shutdown removes the file, so a file
// naming a dead pid is a serve that died — which used to be indis-
// tinguishable from never having run one. It rides the same banner slot
// as every other state, and its stale project records stay unprinted.
func TestDashShowsACrashedServe(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	pid := deadPid(t)
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: pid, Armed: true, Started: now.Add(-time.Hour), WrittenAt: now.Add(-3 * time.Hour),
		Projects: []serve.ActivityProject{{Project: "faller", SweptAt: now.Add(-4 * time.Hour)}},
	})

	got := serveCluster(t, root, now)
	if want := fmt.Sprintf("serve dead (pid %d) · stale 3h 0m", pid); got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
	if strings.Contains(got, "armed") {
		t.Errorf("banner tail = %q, want a dead serve to stop claiming it will pulse", got)
	}
	if lines := serveLines(t, root, now); lines != "" {
		t.Errorf("a dead serve printed %q from its stale records, want nothing", lines)
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
// backwards. The warning keeps its own line — it names a file, and that's
// a warning rather than status.
func TestDashSurvivesATruncatedStateFile(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	if err := os.MkdirAll(root+"/.moe", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serve.ActivityPath(root), []byte("{half"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := serveLines(t, root, now)
	if !strings.HasPrefix(got, "serve:") || strings.Count(got, "\n") != 1 {
		t.Errorf("serve lines = %q, want one warning line", got)
	}
	if tail := serveCluster(t, root, now); tail != "" {
		t.Errorf("banner tail = %q for an unreadable state file, want nothing", tail)
	}
}

// TestDashBannerCarriesTheModeCounts: modes are read off project.json
// rather than out of serve.json, so `moe project mode` shows up in `moe
// dash` on the next frame instead of the next tick — twenty minutes of
// the dash disagreeing with the command just typed.
func TestDashBannerCarriesTheModeCounts(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	// The snapshot is deliberately the *pre-flip* one: serve only rewrites
	// it on its own events, so this is exactly the state a dash frame finds
	// seconds after the verb ran.
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true,
		Started:  now.Add(-4 * time.Minute),
		NextTick: now.Add(12 * time.Minute),
	})
	writeProjectMetadata(t, root, &project.Metadata{
		ID: "alpha", Mode: "paused", Remote: "x", DefaultBranch: "main"})
	writeProjectMetadata(t, root, &project.Metadata{
		ID: "beta", Mode: "safe", Remote: "x", DefaultBranch: "main"})
	writeProjectMetadata(t, root, &project.Metadata{
		ID: "gamma", Remote: "x", DefaultBranch: "main"})

	want := "serve armed · up 4m · next 12m · 1 paused · 1 safe"
	if got := serveCluster(t, root, now); got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
}

// TestDashBannerSpendsNothingOnAnAllAutoBoard: the common case has to
// read exactly as it did before modes existed.
func TestDashBannerSpendsNothingOnAnAllAutoBoard(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: os.Getpid(), Armed: true,
		Started:  now.Add(-4 * time.Minute),
		NextTick: now.Add(12 * time.Minute),
	})
	writeProjectMetadata(t, root, &project.Metadata{
		ID: "alpha", Remote: "x", DefaultBranch: "main"})

	if got, want := serveCluster(t, root, now), "serve armed · up 4m · next 12m"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
}

// TestDashWontCallAServeDeadItCannotSee is the incident this three-valued
// liveness exists for: an agent read the dash from a Bash sandbox, whose
// pid namespace does not contain the host serve's pid, and relayed
// "serve dead (pid 23970)" to an operator whose own terminal was showing
// "serve armed". The probe is unanswerable across the boundary, so the
// banner says so — and the rows stay, because they are the only view of
// serve that reader has.
func TestDashWontCallAServeDeadItCannotSee(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeServeState(t, root, serve.ActivitySnapshot{
		// A namespace that is certainly not ours, and a pid that is
		// certainly gone from ours — exactly what the sandbox sees.
		Pid: deadPid(t), PidNS: "pid:[1]", Armed: true,
		Started: now.Add(-2 * time.Hour), WrittenAt: now.Add(-3 * time.Minute),
		NextTick: now.Add(14 * time.Minute),
		Projects: []serve.ActivityProject{
			{Project: "sweeper", Decision: "the journal moved", Sweep: true,
				SweepStarted: now.Add(-time.Minute)},
		},
	})

	got := serveCluster(t, root, now)
	if want := "serve unknown (sandbox) · written 3m ago"; got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
	// Neither claim is the reader's to make from here.
	for _, forbidden := range []string{"armed", "dead"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("banner tail = %q, want no %q claim from a reader that can't see the pid", got, forbidden)
		}
	}
	if lines := serveLines(t, root, now); !strings.Contains(lines, "sweeper  sweeping") {
		t.Errorf("serve lines = %q, want the sweep rows an unknown serve still earns", lines)
	}
}

// TestDashStillCallsADeadServeDeadInItsOwnNamespace: the guard is a
// namespace comparison, not a blanket retreat from the probe. A reader
// looking at a serve that wrote from the same namespace it is reading
// in has real evidence, and a crashed serve must still read as crashed.
func TestDashStillCallsADeadServeDeadInItsOwnNamespace(t *testing.T) {
	self := repolock.PidNamespace()
	if self == "" {
		t.Skip("no pid namespace handle on this platform")
	}
	root := t.TempDir()
	now := time.Now()
	pid := deadPid(t)
	writeServeState(t, root, serve.ActivitySnapshot{
		Pid: pid, PidNS: self, Armed: true,
		Started: now.Add(-time.Hour), WrittenAt: now.Add(-3 * time.Hour),
	})

	got := serveCluster(t, root, now)
	if want := fmt.Sprintf("serve dead (pid %d) · stale 3h 0m", pid); got != want {
		t.Errorf("banner tail = %q, want %q", got, want)
	}
}
