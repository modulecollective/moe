package serve

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
)

// The activity record: what an armed serve is about to do, what it just
// did, and why.
//
// Every signal the operator wants from a resident serve is produced
// today and then thrown away. Gate reasons go to stderr, child PTY
// output goes on the floor, and no last-tick or next-tick instant exists
// anywhere — the ticker just fires. That is right for cost and wrong for
// feedback: "is a pulse coming, is one running now, what happened last
// tick" is unanswerable without watching the terminal serve was started
// from.
//
// This is the one model both surfaces read. The web dash renders it from
// memory on page load; `moe dash` reads the JSON snapshot serve keeps at
// <root>/.moe/serve.json (see writeSnapshot).
//
// Runtime state, not history. It resets on restart, exactly like the
// gate's own cursors — a fresh serve has honestly never looked, and
// Started is what says so. Nothing here writes to the journal: the
// durable record of a pulse is its run, as it already was.

// activityRing is how many events the record keeps. Fifty spans a day of
// ticks at the baked cadence — far enough back to reach past the last
// thing that went wrong, short enough to render on a phone in one page.
const activityRing = 50

// tailRenderLines caps how much of a child's output tail reaches the
// page. The tail is a post-mortem snippet — the vendor error and the
// frame around it — not a console, so the last forty non-blank lines is
// the whole ambition.
const tailRenderLines = 40

// activityEvent is one line of the ring: newest-last in storage,
// rendered newest-first.
type activityEvent struct {
	At   time.Time
	Kind string // "tick" | "spawn" | "exit"
	// Subject is the child id a spawn/exit happened to ("heartbeat:moe",
	// "alpha/fix-it"). Empty for a tick, which is board-wide.
	Subject string
	Detail  string
	Failed  bool
	// Run is the pulse run a heartbeat sweep minted, read out of the
	// child's emit file at exit. Empty for every other child — a run
	// child's subject already names its run — and empty for a sweep that
	// died before minting one.
	Run string
	// Tail is the child's last PTY bytes, ANSI-stripped. Exit events
	// only, and web-only: it never reaches the state file.
	Tail string
	// Decisions is the whole per-project verdict set for a tick — the
	// gate trace. Tick events only.
	Decisions []HeartbeatDecision
}

// activityProject is the ticker's per-project record.
type activityProject struct {
	decision string
	sweep    bool
	// held is the last tick's HeartbeatDecision.Held: a stand-down worth
	// a row on /serve rather than a tally mark. Runtime-only, like the
	// rest of this struct — it never reaches the state file, because the
	// CLI dash's earned rows stay sweeping/cooling/failed.
	held bool
	// runID is the pulse run the current-or-last sweep minted, learned
	// from the child's emit file — lazily while the sweep is live, and at
	// its exit. Empty until one is known, and cleared at each sweep start:
	// the previous sweep's run is not this one's.
	runID     string
	started   time.Time // current sweep's start; zero when none is running
	sweptAt   time.Time
	clean     bool
	fails     int
	coolTicks int
}

// activity is the mutex-guarded record itself, owned by Server.
type activity struct {
	mu sync.Mutex

	// root is the bureaucracy the record belongs to. Held because a live
	// sweep's run is on disk, not in the record: panel() reads the emit
	// file for a project whose sweep is still running (see sweepRunPath).
	root    string
	pid     int
	addr    string
	armed   bool
	started time.Time
	// interval is the tick cadence, captured once at construction rather
	// than read on each snapshot. Snapshots run on the child-reader and
	// sweep-watcher goroutines, and heartbeatInterval is a var only so
	// tests can shorten it — reading it from there would be a write those
	// goroutines race. The cadence is baked in production, so holding a
	// copy costs nothing.
	interval time.Duration

	lastTick time.Time
	projects map[string]*activityProject
	events   []activityEvent
}

func newActivity(root string, pid int, addr string, armed bool, now time.Time) *activity {
	return &activity{
		root:     root,
		pid:      pid,
		addr:     addr,
		armed:    armed,
		started:  now,
		interval: heartbeatInterval,
		projects: map[string]*activityProject{},
	}
}

// project returns the record for projectID, minting it on first sight.
// Callers hold a.mu.
func (a *activity) project(projectID string) *activityProject {
	p := a.projects[projectID]
	if p == nil {
		p = &activityProject{}
		a.projects[projectID] = p
	}
	return p
}

func (a *activity) push(ev activityEvent) {
	a.events = append(a.events, ev)
	if len(a.events) > activityRing {
		a.events = a.events[len(a.events)-activityRing:]
	}
}

// recordTick folds one heartbeat pass into the record: the tick instant
// (which is also what dates the next one) and every project's verdict.
func (a *activity) recordTick(at time.Time, decisions []HeartbeatDecision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastTick = at
	for _, d := range decisions {
		p := a.project(d.Project)
		p.decision = d.Reason
		p.sweep, p.held = d.Sweep, d.Held
	}
	a.push(activityEvent{At: at, Kind: "tick", Decisions: slices.Clone(decisions)})
}

// recordSkip overrides a project's verdict with the ticker's own — the
// cool-off, which the gate knows nothing about. Without it a project the
// gate wanted swept and the ticker held would render as sweeping.
//
// held marks a hold that deserves its own row rather than collapsing
// into the quiet tally. The cool-off passes false: it earns its row
// through coolTicks, and setting held too would double-claim it.
func (a *activity) recordSkip(projectID, reason string, held bool, fails, coolTicks int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.project(projectID)
	p.decision = reason
	p.sweep, p.held = false, held
	p.fails, p.coolTicks = fails, coolTicks
}

// recordSweepStart stamps a live sweep's start on the row. It leaves
// runID alone on purpose: the caller runs this after the spawn, and a
// fast child can already have named its run through recordSweepRun by
// then. Dropping the last sweep's run is the caller's job, alongside its
// emit file, before the spawn — see heartbeatTick.
func (a *activity) recordSweepStart(projectID string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.project(projectID).started = at
}

// recordSweepRun folds in the run a sweep minted, once serve has read it
// out of the child's emit file.
func (a *activity) recordSweepRun(projectID, runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.project(projectID).runID = runID
}

func (a *activity) recordSweepEnd(projectID string, at time.Time, clean bool, fails, coolTicks int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.project(projectID)
	p.started = time.Time{}
	p.sweptAt, p.clean = at, clean
	p.fails, p.coolTicks = fails, coolTicks
}

// recordChildSpawn and recordChildExit are the registry's two hooks, so
// every PTY child serve parents — a phone-launched run, a chore, a
// heartbeat sweep — lands in the ring on the same terms. The child id is
// the subject and spells its own kind: "heartbeat:<project>" can't be
// mistaken for a run's "<project>/<slug>".
func (a *activity) recordChildSpawn(id string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.push(activityEvent{At: at, Kind: "spawn", Subject: id, Detail: "started"})
}

func (a *activity) recordChildExit(id string, at time.Time, exitErr error, tail, runID string) {
	detail := "exited cleanly"
	if exitErr != nil {
		detail = "exited: " + exitErr.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.push(activityEvent{
		At: at, Kind: "exit", Subject: id,
		Detail: detail, Failed: exitErr != nil, Tail: tail, Run: runID,
	})
}

// nextTick is when the ticker fires next: the last tick plus the baked
// interval, or — before the first one — the process start plus it. The
// cadence is fixed, so neither is a guess. Zero for an unarmed serve,
// which never ticks at all. Callers hold a.mu.
func (a *activity) nextTick() time.Time {
	if !a.armed {
		return time.Time{}
	}
	if a.lastTick.IsZero() {
		return a.started.Add(a.interval)
	}
	return a.lastTick.Add(a.interval)
}

// ActivitySnapshot is the JSON serve keeps at <root>/.moe/serve.json:
// the activity record minus the per-child output tails.
//
// The file is the CLI dash's whole transport. `moe dash` would need a
// record like this to *discover* a running serve before it could probe
// one over HTTP, so the record might as well carry the payload: one
// writer, one reader, no HTTP client in the CLI, no new API surface, and
// it still reads when serve is wedged enough to accept no connections.
// It can lag reality by a beat, which at one event every few minutes is
// nothing. The web panel does not read it — serve renders from memory.
type ActivitySnapshot struct {
	Pid int `json:"pid"`
	// PidNS is the pid namespace Pid is a number in, as Linux's
	// "pid:[<inode>]" handle; empty where it can't be read. A reader in
	// a different namespace — an agent harness's Bash sandbox, which
	// gets its own — cannot see this pid at all, so its liveness probe
	// would report a healthy serve as crashed. This is what lets that
	// reader tell "gone" from "not visible from here".
	PidNS   string    `json:"pid_ns,omitempty"`
	Addr    string    `json:"addr"`
	Armed   bool      `json:"armed"`
	Started time.Time `json:"started"`
	// LastTick is zero until the first tick — a serve that just started
	// has honestly not looked yet.
	LastTick time.Time         `json:"last_tick,omitzero"`
	NextTick time.Time         `json:"next_tick,omitzero"`
	Projects []ActivityProject `json:"projects,omitempty"`
	// WrittenAt is when serve last rewrote the file, and it is what makes
	// a dead serve legible: a pid that no longer exists plus this stamp's
	// age is "crashed, and roughly when".
	WrittenAt time.Time `json:"written_at"`
}

// ActivityProject is one project's heartbeat state as of the last write.
type ActivityProject struct {
	Project string `json:"project"`
	// Decision is the verdict text from the last tick — the gate's own
	// reason string, or the ticker's when the cool-off overrode it.
	Decision string `json:"decision,omitempty"`
	// Sweep reports that the last tick decided to sweep this project.
	Sweep bool `json:"sweep,omitempty"`
	// SweepStarted dates a sweep that is running right now; zero when
	// none is.
	SweepStarted time.Time `json:"sweep_started,omitzero"`
	SweptAt      time.Time `json:"swept_at,omitzero"`
	SweepClean   bool      `json:"sweep_clean,omitempty"`
	Fails        int       `json:"fails,omitempty"`
	CoolTicks    int       `json:"cool_ticks,omitempty"`
}

// Sweeping reports whether a sweep child is live for this project.
func (p ActivityProject) Sweeping() bool { return !p.SweepStarted.IsZero() }

// Cooling reports whether the project is serving out a failure cool-off.
func (p ActivityProject) Cooling() bool { return p.CoolTicks > 0 }

// Failed reports that the last finished sweep died. Only meaningful once
// a sweep has finished at all.
func (p ActivityProject) Failed() bool { return !p.SweptAt.IsZero() && !p.SweepClean }

// snapshot renders the record as the on-disk shape. Projects come out in
// id order so a diff of two writes reads as a change of state rather
// than a reshuffle of map iteration.
func (a *activity) snapshot(now time.Time) ActivitySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := ActivitySnapshot{
		Pid: a.pid,
		// Read per write rather than cached on the record: a live
		// process never changes namespace, so this is a readlink for
		// free correctness, and it keeps the field next to the pid it
		// qualifies.
		PidNS:     repolock.PidNamespace(),
		Addr:      a.addr,
		Armed:     a.armed,
		Started:   a.started,
		LastTick:  a.lastTick,
		NextTick:  a.nextTick(),
		WrittenAt: now,
	}
	for _, id := range slices.Sorted(maps.Keys(a.projects)) {
		p := a.projects[id]
		snap.Projects = append(snap.Projects, ActivityProject{
			Project:      id,
			Decision:     p.decision,
			Sweep:        p.sweep,
			SweepStarted: p.started,
			SweptAt:      p.sweptAt,
			SweepClean:   p.clean,
			Fails:        p.fails,
			CoolTicks:    p.coolTicks,
		})
	}
	return snap
}

// ActivityPath is where serve keeps its state file. Under `.moe/`, which
// carries a `*` gitignore, so a running serve never dirties the tree.
func ActivityPath(root string) string {
	return filepath.Join(root, ".moe", "serve.json")
}

// ReadActivitySnapshot reads the state file. ok is false when no serve
// has written one — the ordinary case for an operator who doesn't run
// serve, and the case that keeps `moe dash` byte-identical to what it
// was. A file that exists but won't parse is an error: a truncated write
// is worth a word rather than a silently missing panel.
func ReadActivitySnapshot(root string) (snap ActivitySnapshot, ok bool, err error) {
	body, err := os.ReadFile(ActivityPath(root))
	if os.IsNotExist(err) {
		return ActivitySnapshot{}, false, nil
	}
	if err != nil {
		return ActivitySnapshot{}, false, err
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return ActivitySnapshot{}, false, fmt.Errorf("serve: parse %s: %w", ActivityPath(root), err)
	}
	return snap, true, nil
}

// writeSnapshot writes the state file atomically — tmp then rename — so
// a reader mid-repaint never sees a half-written record. Called on
// listen and on every tick, spawn and exit; at one event every few
// minutes the write is free.
func writeSnapshot(root string, snap ActivitySnapshot) error {
	dir, err := repolock.EnsureDir(root)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, "serve.json.tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, ActivityPath(root))
}

// removeSnapshot drops the state file. Called on clean shutdown, which
// is what makes its *presence* mean something: a file left behind names
// a pid, and a pid that no longer exists is a serve that crashed.
func removeSnapshot(root string) error {
	err := os.Remove(ActivityPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ansiRE matches what a PTY tail is full of and a web page has no use
// for: CSI sequences, OSC strings, the bare two-byte escapes an agent's
// renderer emits between frames, and the control bytes either side of
// them. Tab and newline survive — they carry the shape of the error
// message, which is the only thing the tail is for.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]` +
	`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
	`|\x1b[@-Z\\-_]` +
	`|[\x00-\x08\x0b-\x1f\x7f]`)

// cleanTail turns raw PTY bytes into the snippet the panel shows: escape
// sequences gone, blank lines dropped (an agent's full-screen repaint is
// mostly padding), and only the last tailRenderLines kept. The newest
// lines are the ones that say what it died of.
func cleanTail(raw string) string {
	stripped := ansiRE.ReplaceAllString(raw, "")
	var lines []string
	for line := range strings.SplitSeq(stripped, "\n") {
		if line = strings.TrimRight(line, " \t"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > tailRenderLines {
		lines = lines[len(lines)-tailRenderLines:]
	}
	return strings.Join(lines, "\n")
}
