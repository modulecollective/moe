package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/serve"
)

// The CLI dash's window onto a running serve.
//
// `moe dash` is stateless compute-on-read from the journal and has no
// channel to a resident serve, so a heartbeat that is about to pulse — or
// one that died three hours ago — is invisible from the terminal. This
// reads the snapshot serve keeps at <root>/.moe/serve.json and spends at
// most a handful of lines on it.
//
// The line budget is the point. No file means no lines at all, which is
// what keeps the dash byte-identical for an operator who doesn't run
// serve; a quiet board gets one status line; a project only earns a line
// of its own when it is sweeping, cooling, or its last sweep died.
// --watch gets freshness for free — the 3s repaint re-reads one small
// file.

// renderServeLines writes the serve block, or nothing at all when no
// serve has ever written a state file here.
//
// Warn-only: a state file that won't parse becomes one line and the rest
// of the dash renders. A dashboard that refused to draw because a monitor
// file was truncated would have the priorities backwards.
func renderServeLines(w io.Writer, now time.Time, root string) {
	snap, ok, err := serve.ReadActivitySnapshot(root)
	if err != nil {
		// The error already wears the "serve:" prefix (it names the file).
		cliout.Printf(w, "%v\n", err)
		return
	}
	if !ok {
		return
	}

	// A file whose pid is gone is a serve that crashed: clean shutdown
	// removes the file, so its presence plus a dead pid is the whole
	// signal. The stamp's age is roughly when.
	if !repolock.ProcessAlive(snap.Pid) {
		cliout.Printf(w, "serve: dead (pid %d) — stale since %s\n",
			snap.Pid, dash.HumanAgo(now, snap.WrittenAt))
		return
	}

	cliout.Printf(w, "serve: %s\n", serveStatusLine(now, snap))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range snap.Projects {
		state, detail := serveProjectState(now, p)
		if state == "" {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.Project, state, detail)
	}
	tw.Flush()
}

// serveStatusLine is the one line an operator always gets: whether the
// process may pulse, how long it has been up, and when the next tick
// lands.
func serveStatusLine(now time.Time, snap serve.ActivitySnapshot) string {
	if !snap.Armed {
		return fmt.Sprintf("browse-only (unarmed) · up %s", dash.HumanDuration(now.Sub(snap.Started)))
	}
	line := fmt.Sprintf("armed · up %s", dash.HumanDuration(now.Sub(snap.Started)))
	if !snap.NextTick.IsZero() {
		line += " · next sweep in " + dash.HumanDuration(max(snap.NextTick.Sub(now), 0))
	}
	return line
}

// serveProjectState decides whether a project has earned a line and what
// it says. An empty state means it hasn't: a quiet project is the normal
// case and spending a line on it every repaint would drown the two or
// three that mean something.
//
// Precedence matches the web panel's — a live sweep, then a cool-off
// holding the project back, then a sweep that died.
func serveProjectState(now time.Time, p serve.ActivityProject) (state, detail string) {
	switch {
	case p.Sweeping():
		return "sweeping", fmt.Sprintf("(%s) — %s", dash.HumanDuration(now.Sub(p.SweepStarted)), p.Decision)
	case p.Cooling():
		return "cooling", fmt.Sprintf("(%s left) — last sweep failed %s",
			dash.Plural(p.CoolTicks, "tick"), dash.HumanAgo(now, p.SweptAt))
	case p.Failed():
		return "failed", fmt.Sprintf("(%s) — %s", dash.HumanAgo(now, p.SweptAt), p.Decision)
	}
	return "", ""
}
