package serve

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func testActivity(t *testing.T, now time.Time, armed bool) *activity {
	return newActivity(t.TempDir(), 4242, "127.0.0.1:4242", armed, now)
}

// TestActivityRecordsEveryVerdictNotJustTheSweeps is the whole point of
// widening the Due seam: the gate's stand-down reasons used to go to
// stderr and die there, which is why four heartbeat bugs had to be
// diagnosed by inferring invisible cursor state.
func TestActivityRecordsEveryVerdictNotJustTheSweeps(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{
		{Project: "moe", Sweep: true, Reason: "the journal moved"},
		{Project: "bureaucracy", Reason: "a sweep already surveyed the current tip"},
	})

	snap := a.snapshot(now)
	if len(snap.Projects) != 2 {
		t.Fatalf("snapshot has %d projects, want both the swept and the quiet one", len(snap.Projects))
	}
	// Sorted by id, so bureaucracy leads.
	if got := snap.Projects[0]; got.Project != "bureaucracy" || got.Sweep ||
		got.Decision != "a sweep already surveyed the current tip" {
		t.Errorf("quiet project = %+v, want the stand-down reason recorded", got)
	}
	if got := snap.Projects[1]; got.Project != "moe" || !got.Sweep || got.Decision != "the journal moved" {
		t.Errorf("swept project = %+v, want the sweep reason recorded", got)
	}
}

// TestActivityCoolOffOverridesTheGatesReason: the gate wanted the project
// swept and the ticker's backoff held it. Leaving the gate's reason
// standing would render "sweeping — the journal moved" for a project
// doing nothing of the kind.
func TestActivityCoolOffOverridesTheGatesReason(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{{Project: "moe", Sweep: true, Reason: "the journal moved"}})
	a.recordSkip("moe", "cooling off after 2 failure(s)", 2, 1)

	p := a.snapshot(now).Projects[0]
	if p.Sweep {
		t.Error("a project held by the cool-off must not read as swept")
	}
	if !strings.Contains(p.Decision, "cooling off") {
		t.Errorf("decision = %q, want the cool-off named", p.Decision)
	}
	if p.Fails != 2 || p.CoolTicks != 1 {
		t.Errorf("fails/cool = %d/%d, want 2/1", p.Fails, p.CoolTicks)
	}
	if !p.Cooling() {
		t.Error("Cooling() should report the outstanding cool-off")
	}
}

// TestActivitySweepLifecycle walks one sweep: start marks the project
// live, exit clears that and records the outcome.
func TestActivitySweepLifecycle(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordSweepStart("moe", now)
	if p := a.snapshot(now).Projects[0]; !p.Sweeping() {
		t.Fatalf("project = %+v, want a live sweep", p)
	}
	a.recordSweepEnd("moe", now.Add(time.Minute), false /*failed*/, 1, 1)

	p := a.snapshot(now).Projects[0]
	if p.Sweeping() {
		t.Error("a finished sweep must stop reading as live")
	}
	if !p.Failed() {
		t.Error("Failed() should report the dead sweep")
	}
}

// TestActivityFailedNeedsAFinishedSweep: SweepClean is false on a
// zero-valued record, so a project that has never swept must not read as
// one whose sweep died.
func TestActivityFailedNeedsAFinishedSweep(t *testing.T) {
	var p ActivityProject
	if p.Failed() {
		t.Error("a project with no finished sweep reads as failed")
	}
}

// TestActivityRingIsBounded: the record is a monitor, not a log. It has
// to survive a serve left running for weeks without growing.
func TestActivityRingIsBounded(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	for i := range activityRing * 3 {
		a.recordChildSpawn("alpha/run", now.Add(time.Duration(i)*time.Second))
	}
	if got := len(a.events); got != activityRing {
		t.Errorf("ring holds %d events, want it capped at %d", got, activityRing)
	}
}

// TestActivityNextTickIsExactBeforeTheFirstTick: the ticker is
// fixed-cadence and starts with the listener, so "next sweep in 12m" is
// answerable from the moment serve is up — "armed, nothing swept yet" is
// a different answer from "no serve at all".
func TestActivityNextTickIsExactBeforeTheFirstTick(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	if got := a.snapshot(now).NextTick; !got.Equal(now.Add(heartbeatInterval)) {
		t.Errorf("next tick = %v, want the process start plus one interval", got)
	}

	tick := now.Add(5 * time.Minute)
	a.recordTick(tick, nil)
	if got := a.snapshot(tick).NextTick; !got.Equal(tick.Add(heartbeatInterval)) {
		t.Errorf("next tick = %v, want the last tick plus one interval", got)
	}
}

// TestActivityUnarmedServeHasNoNextTick: an unarmed serve never ticks, so
// promising a sweep would be a lie. The strip still renders — "it's up but
// will never pulse" is exactly the confusion being fixed.
func TestActivityUnarmedServeHasNoNextTick(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, false)
	if got := a.snapshot(now).NextTick; !got.IsZero() {
		t.Errorf("next tick = %v on an unarmed serve, want none", got)
	}
	if vm := a.panel(now); vm.Armed || vm.NextSweep != "" {
		t.Errorf("panel = %+v, want browse-only with no countdown", vm)
	}
}

// TestActivityStateFileRoundTrips is the CLI transport: serve writes,
// `moe dash` reads, and a clean shutdown takes the file with it — which
// is what makes a file left behind mean "crashed".
func TestActivityStateFileRoundTrips(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	if _, ok, err := ReadActivitySnapshot(root); ok || err != nil {
		t.Fatalf("read with no file = (%v, %v), want (false, nil) — a dash with no serve must stay as it was", ok, err)
	}

	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{{Project: "moe", Sweep: true, Reason: "the journal moved"}})
	if err := writeSnapshot(root, a.snapshot(now)); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}

	got, ok, err := ReadActivitySnapshot(root)
	if err != nil || !ok {
		t.Fatalf("read after write = (%v, %v), want a snapshot", ok, err)
	}
	if got.Pid != 4242 || !got.Armed || len(got.Projects) != 1 || got.Projects[0].Decision != "the journal moved" {
		t.Errorf("snapshot = %+v, want the record it was written from", got)
	}
	if got.NextTick.IsZero() || got.WrittenAt.IsZero() {
		t.Errorf("snapshot = %+v, want the tick clock and the write stamp to survive JSON", got)
	}

	if err := removeSnapshot(root); err != nil {
		t.Fatalf("removeSnapshot: %v", err)
	}
	if _, ok, _ := ReadActivitySnapshot(root); ok {
		t.Error("the state file survived a clean shutdown")
	}
	// Idempotent: shutdown can race a hand-removed file.
	if err := removeSnapshot(root); err != nil {
		t.Errorf("second removeSnapshot: %v", err)
	}
}

// TestActivityStateFileStaysOutOfGit: `.moe/` carries a `*` gitignore, and
// a running serve rewriting a file every few minutes must not dirty the
// operator's tree.
func TestActivityStateFileStaysOutOfGit(t *testing.T) {
	root := t.TempDir()
	if err := writeSnapshot(root, testActivity(t, time.Now(), true).snapshot(time.Now())); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(root + "/.moe/.gitignore")
	if err != nil {
		t.Fatalf("no gitignore beside the state file: %v", err)
	}
	if strings.TrimSpace(string(body)) != "*" {
		t.Errorf(".moe/.gitignore = %q, want the catch-all", string(body))
	}
}

// TestActivityStateFileRejectsGarbage: a truncated write is worth a word
// on the dash rather than a silently missing panel.
func TestActivityStateFileRejectsGarbage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/.moe", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ActivityPath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadActivitySnapshot(root); err == nil {
		t.Error("an unparseable state file read clean")
	}
}

// TestActivityPanelPrecedence: what the operator needs first wins the
// line. A live sweep beats a cool-off beats a dead last sweep beats a
// hold; quiet earns no line at all.
func TestActivityPanelPrecedence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		setup func(a *activity)
		// held stages a gate hold ahead of setup, so the case proves the
		// state under test outranks it rather than merely coexisting.
		held bool
		want string
	}{
		{
			name: "sweeping wins over a cool-off",
			setup: func(a *activity) {
				a.recordSweepEnd("moe", now.Add(-time.Hour), false, 1, 1)
				a.recordSweepStart("moe", now.Add(-3*time.Minute))
			},
			want: "sweeping",
		},
		{
			name:  "cooling wins over a dead last sweep",
			setup: func(a *activity) { a.recordSweepEnd("moe", now.Add(-time.Hour), false, 1, 2) },
			want:  "cooling",
		},
		{
			name:  "a dead sweep with no cool-off left",
			setup: func(a *activity) { a.recordSweepEnd("moe", now.Add(-time.Hour), false, 1, 0) },
			want:  "failed",
		},
		{
			// A stand-down that names something outside the machine is the
			// one quiet worth a row: an operator can act on it.
			name: "a held sweep earns its row",
			setup: func(a *activity) {
				a.recordTick(now, []HeartbeatDecision{
					{Project: "moe", Held: true, Reason: "somebody is already inside the project"},
				})
			},
			want: "held",
		},
		{
			name:  "a dead sweep outranks a hold",
			setup: func(a *activity) { a.recordSweepEnd("moe", now.Add(-time.Hour), false, 1, 0) },
			held:  true,
			want:  "failed",
		},
		{
			name:  "a clean sweep is quiet",
			setup: func(a *activity) { a.recordSweepEnd("moe", now.Add(-time.Hour), true, 0, 0) },
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testActivity(t, now.Add(-2*time.Hour), true)
			if tc.held {
				a.recordTick(now, []HeartbeatDecision{
					{Project: "moe", Held: true, Reason: "somebody is already inside the project"},
				})
			}
			tc.setup(a)
			vm := a.panel(now)
			// An empty want is the collapse: quiet spends no row, only a
			// tally mark.
			if tc.want == "" {
				if len(vm.Projects) != 0 || vm.QuietCount != 1 {
					t.Errorf("panel projects = %+v (quiet %d), want the quiet one collapsed",
						vm.Projects, vm.QuietCount)
				}
				return
			}
			if len(vm.Projects) != 1 || vm.Projects[0].State != tc.want {
				t.Errorf("panel projects = %+v, want state %q", vm.Projects, tc.want)
			}
		})
	}
}

// TestActivityPanelIsBoardWide: /serve is the whole heartbeat's page —
// every project it has a verdict worth reporting for, every child it
// spawned, and a tick's news on one line.
func TestActivityPanelIsBoardWide(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{
		{Project: "moe", Sweep: true, Reason: "the journal moved"},
		{Project: "bureaucracy", Held: true, Reason: "somebody is already inside the project"},
	})
	a.recordSweepStart("moe", now)
	a.recordChildSpawn(heartbeatChildPrefix+"moe", now)
	a.recordChildSpawn("bureaucracy/other", now)

	vm := a.panel(now)
	if len(vm.Projects) != 2 {
		t.Errorf("panel projects = %+v, want both", vm.Projects)
	}
	if len(vm.Events) != 3 {
		t.Errorf("panel events = %+v, want the tick and both spawns", vm.Events)
	}
	tick := vm.Events[len(vm.Events)-1].Text
	if !strings.Contains(tick, "moe sweeping — the journal moved") ||
		!strings.Contains(tick, "bureaucracy held — somebody is already inside the project") {
		t.Errorf("tick text = %q, want both verdicts that are news", tick)
	}
}

// TestSweepStartKeepsTheRunItsChildAlreadyNamed: recordSweepStart runs
// after the spawn, so a child that writes its emit file and exits in
// microseconds can land recordSweepRun first. Clearing the row's run
// here would erase that link permanently — takeSweepRun consumed the
// emit file, so the panel's mid-flight read finds nothing and the row
// stays Failed with no run. The clear lives before the spawn instead.
func TestSweepStartKeepsTheRunItsChildAlreadyNamed(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordSweepRun("moe", "pulse-2026-04-01")
	a.recordSweepStart("moe", now)

	if got := a.panel(now).Projects[0].Run; got != "pulse-2026-04-01" {
		t.Errorf("row Run=%q, want the run its own child already named", got)
	}
}

// TestActivityPanelCollapsesTheQuiet is the wall this page exists to
// stop: fourteen projects with nothing to do produced fourteen rows all
// restating "nothing to do", and a tick paragraph joining all fourteen
// verdicts. One tally mark carries the same fact.
func TestActivityPanelCollapsesTheQuiet(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{
		{Project: "moe", Sweep: true, Reason: "the journal moved"},
		{Project: "alpha", Reason: "a sweep already surveyed the current tip"},
		{Project: "beta", Reason: "the journal hasn't moved and nothing settled is parked"},
	})
	a.recordSweepStart("moe", now)

	vm := a.panel(now)
	if len(vm.Projects) != 1 || vm.Projects[0].Project != "moe" {
		t.Errorf("panel projects = %+v, want only the sweeping one", vm.Projects)
	}
	if vm.QuietCount != 2 {
		t.Errorf("quiet count = %d, want both quiet projects tallied", vm.QuietCount)
	}
	if got, want := vm.Events[0].Text, "moe sweeping — the journal moved · 2 quiet"; got != want {
		t.Errorf("tick text = %q, want %q", got, want)
	}
	for _, gone := range []string{"a sweep already surveyed the current tip", "alpha", "beta"} {
		if strings.Contains(vm.Events[0].Text, gone) {
			t.Errorf("tick text = %q, still spells out %q", vm.Events[0].Text, gone)
		}
	}
}

// TestActivityPanelDropsAnAllQuietTick: a tick where nothing happened is
// not an event. The ring still records it — it is the CLI snapshot's
// source and a debugging trail — but the page says "the heartbeat is
// alive" once, on the status line, instead of once per tick.
func TestActivityPanelDropsAnAllQuietTick(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now.Add(-time.Hour), true)
	a.recordTick(now.Add(-24*time.Minute), []HeartbeatDecision{
		{Project: "moe", Reason: "a sweep already surveyed the current tip"},
	})
	a.recordTick(now.Add(-4*time.Minute), []HeartbeatDecision{
		{Project: "moe", Reason: "a sweep already surveyed the current tip"},
	})

	vm := a.panel(now)
	if len(vm.Events) != 0 {
		t.Errorf("panel events = %+v, want an empty ring — nothing happened", vm.Events)
	}
	if len(a.events) != 2 {
		t.Errorf("record holds %d events, want both ticks still recorded", len(a.events))
	}
	if vm.LastTick != "4m ago" {
		t.Errorf("last tick = %q, want the liveness those entries were carrying", vm.LastTick)
	}
}

// TestActivityPanelHasNoLastTickBeforeTheFirstOne: a serve that just
// started has honestly never looked, and "last tick never" would be a
// worse answer than saying nothing.
func TestActivityPanelHasNoLastTickBeforeTheFirstOne(t *testing.T) {
	now := time.Now()
	if got := testActivity(t, now, true).panel(now).LastTick; got != "" {
		t.Errorf("last tick = %q on a serve that has never ticked, want nothing", got)
	}
}

// TestActivityPanelIsNewestFirst: the question the list answers is "what
// just happened".
func TestActivityPanelIsNewestFirst(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordChildSpawn("alpha/first", now)
	a.recordChildSpawn("alpha/second", now.Add(time.Minute))

	vm := a.panel(now)
	if len(vm.Events) != 2 || vm.Events[0].Subject != "alpha/second" {
		t.Errorf("events = %+v, want the newest first", vm.Events)
	}
}

// TestActivityExitCarriesItsTail is what turns "sweep failed, exit 1"
// into "sweep failed: credit limit reached" — the difference between a
// glance and an ssh session. Only on a failure: a clean exit's output is
// noise.
func TestActivityExitCarriesItsTail(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordChildExit(heartbeatChildPrefix+"moe", now, errors.New("exit status 1"), "credit limit reached", "" /*run*/)
	a.recordChildExit("alpha/fine", now, nil, "all good", "" /*run*/)

	vm := a.panel(now)
	var failed, clean serveEventVM
	for _, ev := range vm.Events {
		if ev.Failed {
			failed = ev
		} else {
			clean = ev
		}
	}
	if failed.Tail != "credit limit reached" {
		t.Errorf("failed exit tail = %q, want the output snippet", failed.Tail)
	}
	if !strings.Contains(failed.Text, "exit status 1") {
		t.Errorf("failed exit text = %q, want the exit error", failed.Text)
	}
	if clean.Tail != "" {
		t.Errorf("clean exit tail = %q, want nothing — only a death needs explaining", clean.Tail)
	}
}

// TestActivityTailIsNotInTheStateFile: the file is re-read on every 3s
// watch repaint, so 8KB of PTY bytes per child has no business in it.
func TestActivityTailIsNotInTheStateFile(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordChildExit(heartbeatChildPrefix+"moe", now, errors.New("boom"), "SECRET-TAIL-MARKER", "" /*run*/)

	root := t.TempDir()
	if err := writeSnapshot(root, a.snapshot(now)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(ActivityPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET-TAIL-MARKER") {
		t.Error("the child output tail reached the state file")
	}
}

// TestCleanTailStripsTerminalControl: the tail is rendered as text on a
// web page. An agent's full-screen repaint is mostly cursor moves,
// colour, and padding, and none of it says what went wrong.
func TestCleanTailStripsTerminalControl(t *testing.T) {
	raw := "\x1b[2J\x1b[H\x1b[31mError:\x1b[0m credit limit reached\r\n" +
		"\n\n\x1b]0;title\x07  at moe pulse\r\n"
	got := cleanTail(raw)
	if got != "Error: credit limit reached\n  at moe pulse" {
		t.Errorf("cleanTail = %q", got)
	}
}

// TestCleanTailKeepsOnlyTheNewestLines: the last lines are the ones that
// say what it died of, and a page has to stay a page.
func TestCleanTailKeepsOnlyTheNewestLines(t *testing.T) {
	var b strings.Builder
	for i := range tailRenderLines * 2 {
		b.WriteString("line ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteByte('\n')
	}
	got := cleanTail(b.String())
	if n := len(strings.Split(got, "\n")); n != tailRenderLines {
		t.Errorf("cleanTail kept %d lines, want %d", n, tailRenderLines)
	}
}

// TestActivityPanelClusterMatchesTheCLIBanner is the cross-surface pin.
// The web header and the CLI banner are contractually the same line —
// both route through dash.ServeCluster, and this golden plus internal/
// cli's TestDashBannerCarriesAnArmedServe are what say so. Change one
// and the other fails.
func TestActivityPanelClusterMatchesTheCLIBanner(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now.Add(-3*24*time.Hour-2*time.Hour), true)
	// A tick 8m back puts the next one 12m out on the 20m cadence.
	a.recordTick(now.Add(-8*time.Minute), nil)
	if got, want := a.panel(now).Cluster, "serve armed · up 3d 2h · next 12m"; got != want {
		t.Errorf("panel cluster = %q, want %q", got, want)
	}
}

// TestActivityPanelClusterCountsFailingProjects: the earned fourth fact.
// It is what keeps "a sweep is failing" on a board glance now that the
// panel itself lives on /serve.
func TestActivityPanelClusterCountsFailingProjects(t *testing.T) {
	now := time.Now()
	a := testActivity(t, now, true)
	a.recordTick(now, []HeartbeatDecision{
		{Project: "alpha", Sweep: true, Reason: "the journal moved"},
		{Project: "beta", Sweep: true, Reason: "the journal moved"},
		{Project: "gamma", Reason: "a sweep already surveyed the current tip"},
	})
	a.recordSweepEnd("alpha", now, false /*failed*/, 1, 1)
	a.recordSweepEnd("beta", now, true /*clean*/, 0, 0)

	if got := a.panel(now).Cluster; !strings.HasSuffix(got, " · 1 failing") {
		t.Errorf("panel cluster = %q, want a failing count of 1", got)
	}
	a.recordSweepEnd("alpha", now, true /*clean*/, 0, 0)
	if got := a.panel(now).Cluster; strings.Contains(got, "failing") {
		t.Errorf("panel cluster = %q, want nothing spent on failures at zero", got)
	}
}
