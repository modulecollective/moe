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
// The line budget is the point. Status costs no line at all: it rides the
// banner's tail as one cluster, in the same slot for every state, so an
// operator who doesn't run serve sees the banner they always saw. A
// project only earns a line of its own when it is sweeping, cooling, or
// its last sweep died. --watch gets freshness for free — the 3s repaint
// re-reads one small file.

// serveState is one read of serve's state file, held so the frame can
// spend it twice: once on the banner's tail, once on the rows below.
//
// Warn-only: a state file that won't parse becomes one line and the rest
// of the dash renders. A dashboard that refused to draw because a monitor
// file was truncated would have the priorities backwards.
type serveState struct {
	snap serve.ActivitySnapshot
	ok   bool // a serve has written a state file here
	err  error
}

// readServeState reads the snapshot for one dash frame.
func readServeState(root string) serveState {
	snap, ok, err := serve.ReadActivitySnapshot(root)
	return serveState{snap: snap, ok: ok, err: err}
}

// bannerCluster is the serve status the banner carries in its tail:
// whether the process may pulse, how long it has been up, when the next
// tick lands, and how many projects are in trouble. Empty when no serve
// has ever written a state file here — which is what keeps the banner
// unchanged for an operator who doesn't run serve.
//
// A file whose pid is gone is a serve that crashed: clean shutdown
// removes the file, so its presence plus a dead pid is the whole signal.
// The stamp's age is roughly when. It rides the same slot as every other
// state — placement that moved by state would cost the reader more than
// the alarm buys.
func (s serveState) bannerCluster(now time.Time) string {
	if !s.ok {
		return ""
	}
	if !repolock.ProcessAlive(s.snap.Pid) {
		return fmt.Sprintf("serve dead (pid %d) · stale %s",
			s.snap.Pid, dash.HumanDuration(now.Sub(s.snap.WrittenAt)))
	}
	var next string
	if !s.snap.NextTick.IsZero() {
		next = dash.HumanDuration(max(s.snap.NextTick.Sub(now), 0))
	}
	return dash.ServeCluster(s.snap.Armed, dash.HumanDuration(now.Sub(s.snap.Started)),
		next, s.failing(now))
}

// failing counts the projects whose row would read "cooling" or
// "failed" — the same states the web panel colours, derived from the
// same predicate that earns the rows, so the banner's count and the
// lines under it can't disagree.
func (s serveState) failing(now time.Time) int {
	n := 0
	for _, p := range s.snap.Projects {
		switch state, _ := serveProjectState(now, p); state {
		case "cooling", "failed":
			n++
		}
	}
	return n
}

// renderLines writes what serve earns below the banner: the parse
// warning if the file was unreadable, then a row per noteworthy project.
// A healthy quiet board writes nothing — its status is already in the
// banner's tail.
func (s serveState) renderLines(w io.Writer, now time.Time) {
	if s.err != nil {
		// The error already wears the "serve:" prefix (it names the file).
		cliout.Printf(w, "%v\n", s.err)
		return
	}
	if !s.ok || !repolock.ProcessAlive(s.snap.Pid) {
		// A dead serve's project records are as stale as its stamp; the
		// banner's "dead" is the whole message.
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range s.snap.Projects {
		state, detail := serveProjectState(now, p)
		if state == "" {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.Project, state, detail)
	}
	tw.Flush()
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
