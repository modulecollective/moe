package dash

import (
	"bytes"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/input"
)

// Clock reads in the box's local zone. Swap time.Local, not TZ: the
// process has already cached its zone.
func TestClockIsLocal(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("TEST", 2*3600)
	t.Cleanup(func() { time.Local = saved })
	now := time.Date(2026, 11, 26, 12, 7, 0, 0, time.UTC)
	if got := Clock(now); got != "Thu 26 Nov 14:07" {
		t.Fatalf("Clock = %q, want the local wall clock", got)
	}
}

func TestRenderPulseGolden(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 11, 26, 14, 7, 0, 0, time.UTC)
	last := &Row{Project: "_cabinet", Run: "pulse-2026-11-26-12", Workflow: PulseWorkflow, When: now.Add(-12 * time.Minute)}
	f := input.File{Notes: []input.Entry{
		{ID: 1, Text: "bump Go everywhere", DeliveredTo: "_cabinet/pulse-2026-11-26-11"},
		{ID: 2, Text: "prefer lightpool", DeliveredTo: "_cabinet/pulse-2026-11-26-12"},
		{ID: 3, Text: "do the lightpool fix next\n\nit blocks the release"},
	}}
	pending, read := SplitPulseNotes(f)
	for _, tc := range []struct {
		name string
		p    Pulse
		want string
	}{
		{
			name: "no serve, no notes",
			p:    Pulse{},
			want: "PULSE\n  serve not running\n\n",
		},
		{
			name: "armed with notes",
			p: Pulse{
				Status:  "serve armed · up 4m · rex · next 15m",
				Last:    last,
				Waiting: PulseWaiting(true, "15m", "", ""),
				Pending: pending,
				Read:    read,
			},
			want: "PULSE\n" +
				"  serve armed · up 4m · rex · next 15m · last pulse 12m ago (pulse-2026-11-26-12)\n" +
				"  ▒ #3  do the lightpool fix next  · waiting — next check in 15m\n" +
				"  ░ #2  prefer lightpool           · read by _cabinet/pulse-2026-11-26-12\n\n",
		},
		{
			name: "paused, dead serve",
			p: Pulse{
				Status:  "serve dead (pid 42) · stale 3h 0m",
				Waiting: PulseWaiting(false, "", "new automatic sweeps paused until 14:20", ""),
				Pending: pending,
			},
			want: "PULSE\n" +
				"  serve dead (pid 42) · stale 3h 0m\n" +
				"  ▒ #3  do the lightpool fix next  · waiting — new automatic sweeps paused until 14:20\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderPulse(&buf, now, tc.p)
			if got := buf.String(); got != tc.want {
				t.Fatalf("RenderPulse =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	if got := PulseWaiting(false, "", "", ""); got != "serve is not armed — `moe pulse new`" {
		t.Errorf("unarmed waiting = %q", got)
	}
}

func TestLastPulsePicksTheNewestPulseRow(t *testing.T) {
	now := time.Now()
	rows := []Row{
		{Run: "pulse-a", Workflow: PulseWorkflow, When: now.Add(-time.Hour)},
		{Run: "fix", Workflow: "sdlc", When: now},
		{Run: "pulse-b", Workflow: PulseWorkflow, When: now.Add(-time.Minute)},
	}
	if got := LastPulse(rows); got == nil || got.Run != "pulse-b" {
		t.Fatalf("LastPulse = %+v, want pulse-b", got)
	}
	if LastPulse(rows[1:2]) != nil {
		t.Fatal("LastPulse found a pulse on a board with none")
	}
}

// When has one-second resolution, so sweeps closing in the same second
// tie. Rows arrive in ReadDir byte order, which puts the older slug
// first in the first two cases.
func TestLastPulseBreaksSameSecondTiesBySlug(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		runs []string
		want string
	}{
		{[]string{"pulse-2026-11-26-5", "pulse-2026-11-26-6"}, "pulse-2026-11-26-6"},
		{[]string{"pulse-2026-11-26", "pulse-2026-11-26-2"}, "pulse-2026-11-26-2"},
		{[]string{"pulse-2026-11-26-10", "pulse-2026-11-26-9"}, "pulse-2026-11-26-10"},
		{[]string{"pulse-2026-11-26-18", "pulse-2026-11-27"}, "pulse-2026-11-27"},
	} {
		for _, reverse := range []bool{false, true} {
			var rows []Row
			for _, run := range tc.runs {
				rows = append(rows, Row{Run: run, Workflow: PulseWorkflow, When: now})
			}
			if reverse {
				rows[0], rows[1] = rows[1], rows[0]
			}
			if got := LastPulse(rows); got == nil || got.Run != tc.want {
				t.Errorf("LastPulse(%s, %s) = %+v, want %s", rows[0].Run, rows[1].Run, got, tc.want)
			}
		}
	}

	// Time still outranks slug.
	rows := []Row{
		{Run: "pulse-2026-11-26-5", Workflow: PulseWorkflow, When: now.Add(time.Second)},
		{Run: "pulse-2026-11-26-6", Workflow: PulseWorkflow, When: now},
	}
	for _, rs := range [][]Row{rows, {rows[1], rows[0]}} {
		if got := LastPulse(rs); got == nil || got.Run != "pulse-2026-11-26-5" {
			t.Errorf("LastPulse = %+v, want the strictly newer pulse-2026-11-26-5", got)
		}
	}
}
