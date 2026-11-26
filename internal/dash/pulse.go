package dash

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/input"
)

// PulseWorkflow is the survey's workflow id, as BuildRows records it on
// a row.
const PulseWorkflow = "pulse"

// Clock is the board's top-line timestamp in the box's local zone:
// "Thu 26 Nov 14:07". Both dashes' first line is the logo and this.
func Clock(now time.Time) string {
	return now.Local().Format("Mon 2 Jan 15:04")
}

// Pulse is what the PULSE section shows: the survey's state, and what
// the operator has told it. The caller builds Status (the serve cluster,
// which only it can read) and Detail (any serve trouble rows it earns,
// already rendered); the rest is read off the board and the pulse-notes
// record.
type Pulse struct {
	// Status is the serve cluster line; empty renders "serve not running".
	Status string
	// Detail is pre-rendered lines that qualify Status, each indented.
	Detail string
	// Last is the newest pulse run on the board, if any.
	Last *Row
	// Waiting says what a pending note waits on (see PulseWaiting).
	Waiting string
	// Pending are the notes no sweep has read, oldest first.
	Pending []input.Entry
	// Read are notes a sweep has read, newest first.
	Read []input.Entry
}

// LastPulse returns the newest pulse run among rows. Rows carry each
// run's last journal activity, so this is the sweep the operator most
// recently heard from.
func LastPulse(rows []Row) *Row {
	var last *Row
	for i := range rows {
		r := &rows[i]
		if r.Workflow != PulseWorkflow {
			continue
		}
		if last == nil || r.When.After(last.When) {
			last = r
		}
	}
	return last
}

// SplitPulseNotes partitions the pulse record: pending oldest first,
// read newest first.
func SplitPulseNotes(f input.File) (pending, read []input.Entry) {
	for i := len(f.Notes) - 1; i >= 0; i-- {
		if f.Notes[i].Delivered() {
			read = append(read, f.Notes[i])
		}
	}
	return f.Pending(), read
}

// PulseWaiting spells what a pending note is waiting on: the next sweep
// when one is coming, otherwise why none is. pause is the global pause
// summary when a pause is active, empty otherwise; next is the countdown
// to the next tick, empty when none is scheduled.
func PulseWaiting(armed bool, next, pause string) string {
	switch {
	case pause != "":
		return pause
	case !armed:
		return "serve is not armed — `moe pulse new`"
	case next != "":
		return "next sweep in " + next
	default:
		return "next sweep"
	}
}

// RenderPulse writes the PULSE section the terminal boards open with:
// the status line, then pending notes by first line, then the last read
// one with the run that read it.
func RenderPulse(w io.Writer, now time.Time, p Pulse) {
	cliout.Printf(w, "PULSE\n")
	status := p.Status
	if status == "" {
		status = "serve not running"
	}
	if p.Last != nil {
		status += fmt.Sprintf(" · last pulse %s (%s)", HumanAgo(now, p.Last.When), p.Last.Run)
	}
	fmt.Fprintf(w, "  %s\n", status)
	fmt.Fprint(w, p.Detail)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range p.Pending {
		fmt.Fprintf(tw, "  ▒ #%d\t%s\t· waiting — %s\n", e.ID, e.FirstLine(), p.Waiting)
	}
	if len(p.Read) > 0 {
		e := p.Read[0]
		fmt.Fprintf(tw, "  ░ #%d\t%s\t· read by %s\n", e.ID, e.FirstLine(), e.DeliveredTo)
	}
	tw.Flush()
	fmt.Fprintln(w)
}
