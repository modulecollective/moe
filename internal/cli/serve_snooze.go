package cli

import (
	"io"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/serve"
)

// The CLI half of the heartbeat's brake: `moe serve snooze [dur]` and
// `moe serve wake`.
//
// Both write the snooze file and nothing else. That is the whole
// transport — a running serve reads it at its next tick, and the CLI
// never needs an HTTP client or a way to find the listener. It also
// means both work with serve down: snoozing before bed and starting
// serve afterwards holds the clock exactly the same way, and a serve
// restart mid-snooze doesn't silently resume spending.

// runServeSnooze prints the current hold with no argument and sets one
// with a duration. The no-argument read is the reason this isn't
// write-only: `moe serve snooze` is the question as well as the command,
// and it answers even when no serve is running to ask.
func runServeSnooze(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		moePrintln(stderr, "usage: moe serve snooze [duration]")
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	now := time.Now()

	if len(args) == 0 {
		until, snoozed, err := serve.ReadSnooze(root, now)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if !snoozed {
			moePrintln(stdout, "heartbeat: not snoozed")
			return 0
		}
		moePrintf(stdout, "heartbeat: snoozed until %s (%s from now)\n",
			serve.SnoozeClock(until), dash.HumanDuration(until.Sub(now)))
		return 0
	}

	d, err := time.ParseDuration(args[0])
	if err != nil {
		moePrintf(stderr, "moe serve snooze: %v\n", err)
		return 2
	}
	// Zero and negative are almost certainly a typo, and the charitable
	// reading of each ("wake now"? "snooze forever"?) points in opposite
	// directions. `moe serve wake` is the unambiguous spelling of one and
	// nothing spells the other.
	if d <= 0 {
		moePrintln(stderr, "moe serve snooze: duration must be positive; `moe serve wake` resumes now")
		return 2
	}
	until := now.Add(d)
	if err := serve.WriteSnooze(root, until); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "heartbeat: snoozed until %s — sweeps resume in %s\n",
		serve.SnoozeClock(until), dash.HumanDuration(d))
	return 0
}

// runServeWake releases the hold. Silent about whether there was one to
// release: the operator asked for a heartbeat that sweeps, and that is
// what they have either way.
func runServeWake(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe serve wake")
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := serve.ClearSnooze(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintln(stdout, "heartbeat: awake")
	return 0
}
