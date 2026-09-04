package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/sync"
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
// its last sweep died, and the drain to origin only when it is being
// refused. --watch gets freshness for free — the 3s repaint re-reads one
// small file and one rev-list.

// serveLiveness is what this reader can honestly say about the process
// behind a state file. Three-valued because the pid probe is not always
// answerable — see probeLiveness.
type serveLiveness int

const (
	serveAlive serveLiveness = iota
	serveDead
	serveUnknown
)

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
	// live is the pid probe's verdict, taken once: the banner and the
	// rows below it must not disagree about whether the process is
	// still there.
	live serveLiveness
	// modes counts the projects the operator has capped, read straight
	// from project.json rather than out of the snapshot. Serve only
	// rewrites serve.json on its own events, so waiting for a mode flip to
	// appear there would leave `moe project mode` invisible in `moe dash`
	// until the next tick — twenty minutes of the dash disagreeing with
	// the command the operator just ran.
	modes dash.ModeCounts
	// ahead is how many commits local main holds that origin doesn't,
	// read from git at render rather than out of the snapshot. The
	// snapshot says what serve's pusher did; this says whether anything
	// is waiting *now*, which is the half a crashed serve, a manual
	// `moe sync` or a burst of fresh commits would otherwise get wrong.
	ahead int
}

// readServeState reads the snapshot for one dash frame.
func readServeState(root string) serveState {
	snap, ok, err := serve.ReadActivitySnapshot(root)
	st := serveState{snap: snap, ok: ok, err: err, live: serveDead}
	if ok {
		st.live = probeLiveness(snap)
	}
	// Warn-only, like the snapshot read above it: an unreadable projects/
	// costs the banner its mode counts, not the frame.
	st.modes = projectModeCounts(root)
	// Same terms — an unanswerable ahead-count costs the origin item, not
	// the frame. A `rev-list --count` against local refs, beside the git
	// the dash already runs to gather.
	st.ahead, _ = sync.Unpushed(root)
	return st
}

// projectModeCounts tallies the projects the operator has capped. Zero
// on an all-auto board, which is what keeps the banner's tail unchanged
// for an operator who has never set a mode.
func projectModeCounts(root string) dash.ModeCounts {
	mds, _, err := project.List(root)
	if err != nil {
		return dash.ModeCounts{}
	}
	var counts dash.ModeCounts
	for _, md := range mds {
		switch project.ModeOf(md) {
		case project.ModePaused:
			counts.Paused++
		case project.ModeSafe:
			counts.Safe++
		}
	}
	return counts
}

// probeLiveness decides what the pid probe is worth here.
//
// ProcessAlive only sees pids in the caller's own pid namespace, and an
// agent harness's Bash sandbox gets its own — so a serve running fine
// on the host reads as gone from inside one, and the dash used to
// render "serve dead" over a serve the operator's terminal showed as
// armed. Serve records the namespace it wrote from; when it isn't ours,
// the probe is unanswerable and the honest answer is unknown.
//
// Dead means provably dead. An empty PidNS — a serve older than the
// field, or a writer where the handle can't be read — leaves the probe
// authoritative, which is what keeps this back-compatible. A reader
// that can't establish its own namespace can prove nothing either, so
// it says unknown too.
func probeLiveness(snap serve.ActivitySnapshot) serveLiveness {
	if !repolock.SamePidNS(snap.PidNS) {
		return serveUnknown
	}
	if repolock.ProcessAlive(snap.Pid) {
		return serveAlive
	}
	return serveDead
}

// bannerCluster is everything the banner's tail carries: the serve
// status, then the origin item. Empty when there is nothing to say at
// all — which is what keeps the banner unchanged for an operator who
// doesn't run serve and has nothing waiting to push.
//
// The two halves are independent on purpose. `N unpushed` is read from
// git and true whether or not anything is resident, so a box with no
// serve still shows a journal that hasn't left it; a serve with a quiet
// drain still shows its uptime and next tick.
func (s serveState) bannerCluster(now time.Time) string {
	cluster, item := s.serveCluster(now), s.pushItem(now)
	if cluster == "" || item == "" {
		return cluster + item
	}
	return cluster + " · " + item
}

// serveCluster is the serve half: whether the process may pulse, how
// long it has been up, when the next tick lands, and how many projects
// are in trouble. Empty when no serve has ever written a state file
// here.
//
// A file whose pid is gone is a serve that crashed: clean shutdown
// removes the file, so its presence plus a dead pid is the whole signal.
// The stamp's age is roughly when. It rides the same slot as every other
// state — placement that moved by state would cost the reader more than
// the alarm buys.
//
// The unknown line names the reader's position rather than the serve's
// state, because the reader's position is the whole limitation, and it
// carries neither "armed" nor "dead": agents pattern-match this
// headline and relay it, which is how a sandboxed one came to tell its
// operator their serve was down. The write stamp rides along as the one
// signal that survives the namespace boundary intact.
func (s serveState) serveCluster(now time.Time) string {
	if !s.ok {
		return ""
	}
	switch s.live {
	case serveDead:
		return fmt.Sprintf("serve dead (pid %d) · stale %s",
			s.snap.Pid, dash.HumanDuration(now.Sub(s.snap.WrittenAt)))
	case serveUnknown:
		return "serve unknown (sandbox) · written " + dash.HumanAgo(now, s.snap.WrittenAt)
	}
	var next string
	if !s.snap.NextTick.IsZero() {
		next = dash.HumanDuration(max(s.snap.NextTick.Sub(now), 0))
	}
	return dash.ServeCluster(s.snap.Armed, dash.HumanDuration(now.Sub(s.snap.Started)),
		next, s.modes, s.failing(now))
}

// pushItem is the origin half: has the journal reached GitHub.
//
// Only a serve that could plausibly still be pushing vouches for the
// recorded fields. A provably dead one — and a root with no state file
// at all — gets the ahead-count alone, because "pushed 2m ago" from a
// process that has since crashed is the one reading that would send an
// operator away happy while their journal sits on the box. Unknown
// vouches: git answers from a sandbox, the process is probably fine, and
// the headline already says the reader can't check.
func (s serveState) pushItem(now time.Time) string {
	if !s.ok || s.live == serveDead {
		return dash.PushItem(now, false, s.ahead, time.Time{}, time.Time{})
	}
	return dash.PushItem(now, true, s.ahead, s.snap.LastPush, s.snap.PushFailingSince)
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
	if s.live == serveDead {
		// A dead serve's project records are as stale as its stamp; the
		// banner's "dead" is the whole message. Only provable death
		// buys this silence — under unknown the rows are the sandboxed
		// reader's only view of what serve is sweeping, and the
		// headline already flags what it can't vouch for.
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Same rule as the projects below: trouble earns a row, health
	// doesn't. A healthy drain is already in the banner's tail.
	if state, detail := s.pushLine(now); state != "" {
		fmt.Fprintf(tw, "  origin\t%s\t%s\n", state, detail)
	}
	for _, p := range s.snap.Projects {
		state, detail := serveProjectState(now, p)
		if state == "" {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.Project, state, detail)
	}
	tw.Flush()
}

// pushLine decides whether the drain has earned a row of its own and
// what it says. It has, and only has, when serve is being refused *and*
// commits are actually waiting: that is the state the banner's item can
// only name, and the error text is the thing the operator needs — a
// rejected non-fast-forward and a dead network want different responses.
//
// An empty state is the normal case, and the one a root with no state
// file falls into for free — its zero snapshot is failing since never.
// Callers have already dropped the dead-serve frame, whose whole record
// is stale.
//
// The error goes last, after the parenthetical, so the tabwriter's
// columns are set by the short fixed parts rather than by however long
// git's complaint happens to be. The web row is composed the same way.
func (s serveState) pushLine(now time.Time) (state, detail string) {
	if s.snap.PushFailingSince.IsZero() || s.ahead == 0 {
		return "", ""
	}
	detail = dash.HumanDuration(now.Sub(s.snap.PushFailingSince)) +
		" · " + dash.Plural(s.ahead, "commit")
	if !s.snap.PushRetryAt.IsZero() {
		detail += " · retry in " + dash.HumanDuration(max(s.snap.PushRetryAt.Sub(now), 0))
	}
	return "push failing", "(" + detail + ") — " + s.snap.PushError
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
