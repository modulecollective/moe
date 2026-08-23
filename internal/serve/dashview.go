package serve

import (
	"encoding/json"
	"fmt"
	"html/template"
	"maps"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
)

// noteHintRE matches a machine lineage hint's connector and target
// inside an already-HTML-escaped note: "chained → X", "spawned → X",
// "promoted → X", and "chained after X" (dash.settledChainHint, which
// spells the incoming chain edge in words rather than a "←" glyph). The
// target is a slug token ([a-z0-9][a-z0-9-]*) optionally carrying one
// "project/" segment. Those chars survive HTML escaping unchanged, so
// matching after escape is safe: note content can't forge or break the
// anchor we inject.
var noteHintRE = regexp.MustCompile(`((?:chained|spawned|promoted) → |chained after )([a-z0-9][a-z0-9-]*(?:/[a-z0-9][a-z0-9-]*)?)`)

// noteHTML escapes note for HTML and wraps the target of each machine
// lineage hint in a link to its run page. Every producer emits qualified
// "<project>/<slug>" targets, which link as-is; a bare target (a stray
// legacy value) is tolerated and qualified with the row's own project. Every producer
// pre-checks that the target is on the board, so the /run/ link
// resolves; a since-pruned target 404s like any stale run URL. Notes
// without a hint connector pass through escaped-only.
func noteHTML(project, note string) template.HTML {
	escaped := template.HTMLEscapeString(note)
	linked := noteHintRE.ReplaceAllStringFunc(escaped, func(m string) string {
		sub := noteHintRE.FindStringSubmatch(m)
		connector, target := sub[1], sub[2]
		href := "/run/" + target
		if !strings.Contains(target, "/") {
			href = "/run/" + template.HTMLEscapeString(project) + "/" + target
		}
		return connector + "<a href=\"" + href + "\">" + target + "</a>"
	})
	return template.HTML(linked)
}

// factoryFrameCount is how many factory-art frames the dash bakes into
// the page for the client cross-fade. Lives in code, not config —
// single-operator project.
const factoryFrameCount = 10

// dashRowVM is one row in the HTML dash — already string-formatted
// so the template stays a flat presentation layer with no time math
// or bucket-constant inspection.
type dashRowVM struct {
	Project string
	Run     string
	Note    template.HTML // escaped note with lineage targets linkified (noteHTML)
	When    string        // dash.HumanAgo output
	// URL is where the row's slug links. Runs, ideas and intents go to
	// /run/; a due chore isn't a run yet and goes to /chore/. The backlog
	// section renders both kinds, so it links through this rather than
	// building the path itself.
	URL string
	// Live is true when the row's run is currently parented by this
	// serve process — the per-run page has buttons. Only meaningful
	// for active rows; backlog/completed always render Live=false.
	Live bool
	// Depth is how many levels the row renders nested under a parent —
	// 0 top-level, 1 for a completed spawned run (a tailed pulse), 2+ for
	// deeper spawn lineage. The template draws a "↳" connector for
	// Depth ≥ 1 and indents further per level. Lineage only — chain
	// membership is Chained.
	Depth int
	// Chained marks an active row that follows its chain parent. It
	// renders flush-left with a "→" connector instead of indenting.
	Chained bool
	// Agent marks a machine-opened run. Deliberately independent of
	// Depth: a spawned run whose spawner isn't on the board renders
	// top-level, and the operator still needs to see that the machine put
	// it there. AgentTitle is the hover text, which names the ride level
	// when the journal recorded one.
	Agent      bool
	AgentTitle string
}

// agentMark decides whether a row wears the `agent` badge and what its
// hover says. One badge, two reasons: the machine *opened* the run, or
// the machine *placed* it in its chain. A row can be both, and the
// stronger claim — how the run came to exist at all — wins the title.
//
// The ride level only appears where the journal recorded one; a
// pre-trailer edge badges (the groom is inferable from ChainedChild
// alone once EdgeConsent has an entry, so in practice this is
// post-landing) without inventing a consent word.
func agentMark(r dash.Row) (bool, string) {
	switch {
	case r.Agent:
		return true, "opened by the machine"
	case r.EdgeAgent:
		if r.EdgeConsent == "" {
			return true, "chained here by a pulse groom"
		}
		return true, "chained here by a pulse groom (" + r.EdgeConsent + " ride)"
	}
	return false, ""
}

// bannerArtVM is the shared banner-art block both the home dash and a
// project hub draw between the banner and the run sections: the daily
// activity histogram over the factory-art rail, plus the frames the
// client cross-fades through. Embedded in dashVM and hubVM so the
// {{define "bannerart"}} partial renders the same way on both pages.
type bannerArtVM struct {
	// FactoryArt is the peripheral-vision rail the CLI dash also draws
	// under its banner — backlog feed, station glyphs for active runs,
	// completed-output dots. One-line empty state, three lines populated.
	// It is frame[0] of the frames below: the server-rendered, no-JS fallback.
	FactoryArt []template.HTML
	// Histogram is the daily run-activity chart drawn between the banner
	// and the factory art — HistRows bar rows, a blank spacer, then a
	// caption, or a single "(quiet)" line in the cold state. Static text (a
	// per-render snapshot), so unlike the factory frames it carries no JS
	// animation.
	Histogram []template.HTML
	// FactoryFramesJSON is json.Marshal of all factory-art frames, embedded
	// in the page so the client can cross-fade through them without an XHR.
	// It is template.JS, not string: html/template treats <script> content
	// as a JS value context and would string-wrap a plain string (breaking
	// JSON.parse), so the pre-marshalled JSON is emitted verbatim. Safe to do
	// so because json.Marshal escapes <,>,& — which is also what keeps the
	// frames' own span markup from closing the script element early.
	FactoryFramesJSON template.JS
}

// artClass is the web half of the band mapping: one CSS class per band,
// each pointing at an --art-* custom property. BandNone is the padding
// between glyphs and stays bare — a class per space would triple the
// markup for no paint.
func artClass(b dash.Band) string {
	switch b {
	case dash.BandDim:
		return "art-dim"
	case dash.BandMid:
		return "art-mid"
	case dash.BandBright:
		return "art-bright"
	case dash.BandPlasma:
		return "art-plasma"
	}
	return ""
}

// artHTML renders one art line's spans as markup. Span text is escaped
// even though the glyph set is entirely builder-owned (no user content
// reaches an art line) — the art is written into the page as HTML now,
// so the escape is what keeps that true if a builder ever grows a glyph
// with meaning in HTML.
func artHTML(spans []dash.Span) template.HTML {
	var b strings.Builder
	for _, sp := range spans {
		class := artClass(sp.Band)
		if class != "" {
			b.WriteString(`<span class="`)
			b.WriteString(class)
			b.WriteString(`">`)
		}
		template.HTMLEscape(&b, []byte(sp.Text))
		if class != "" {
			b.WriteString(`</span>`)
		}
	}
	return template.HTML(b.String())
}

// artLines renders a whole art block — one HTML string per line, ready
// for the <pre> and for the frames JSON.
func artLines(spans [][]dash.Span) []template.HTML {
	out := make([]template.HTML, len(spans))
	for i, line := range spans {
		out[i] = artHTML(line)
	}
	return out
}

// newBannerArt builds the banner-art block from the (already
// project-scoped, if filtering) dash rows and the trailing-HistDays
// activity counts. The factory art reflects exactly the rows passed in,
// so a project hub gets its own rail and histogram.
func newBannerArt(now time.Time, rows []dash.Row, histogram []int) bannerArtVM {
	state := dash.FactoryStateFromRows(rows)
	r := rand.New(rand.NewSource(now.UnixNano()))
	frames := dash.BuildFactoryFrames(state, dash.ArtWidth, factoryFrameCount, r)
	// Bake each frame's markup server-side. The swap JS then just writes
	// innerHTML — classification lives in dash, in one language.
	painted := make([][]template.HTML, len(frames))
	for i, f := range frames {
		painted[i] = artLines(dash.FactorySpans(f))
	}
	framesJSON, _ := json.Marshal(painted) // strings never fail to marshal
	return bannerArtVM{
		FactoryArt:        painted[0],
		Histogram:         artLines(dash.HistogramSpans(dash.BuildActivityHistogram(histogram))),
		FactoryFramesJSON: template.JS(framesJSON),
	}
}

// completedVM is one COMPLETED section: the rows that fit the requested
// cap, plus what the "show more" link needs to draw itself. The home
// dash and a project hub each hold one and the fragment response *is*
// one, so all three paths render the section from the same partial.
type completedVM struct {
	Rows  []dashRowVM
	Total int // pre-cap row count; lets the header show "N of M"
	// Next is the cap the "show more" link asks for. Zero when every row
	// is already shown, which is also how the templates decide whether to
	// draw the link at all.
	Next int
}

// newCompletedVM caps rows at the newest topCap top-level runs (nested
// descendants ride in with their parent — dash.CompletedCutoff owns that
// rule) and works out what the next click should ask for.
func newCompletedVM(rows []dashRowVM, topCap int) completedVM {
	cut := dash.CompletedCutoff(len(rows), topCap, func(i int) bool { return rows[i].Depth > 0 })
	vm := completedVM{Rows: rows[:cut], Total: len(rows)}
	if cut < len(rows) {
		// A short cut means the walk stopped on the (topCap+1)th
		// top-level row, so exactly topCap of them are on screen — no
		// need to recount before stepping.
		vm.Next = topCap + dash.CompletedStep
	}
	return vm
}

// rowBuckets is the Active/Intents/Backlog/Completed split of a dash
// snapshot, shared by the home dash and the project hub. Both embed it,
// so `.Active` and friends resolve unchanged in Go and in the templates.
type rowBuckets struct {
	Active    []dashRowVM
	Intents   []dashRowVM
	Backlog   []dashRowVM
	Completed completedVM
}

// newRowBuckets converts each row to its view model and files it into a
// bucket. topCap bounds the COMPLETED section — the dash and the hub
// pass their own.
func newRowBuckets(now time.Time, rows []dash.Row, topCap int) rowBuckets {
	var out rowBuckets
	var completed []dashRowVM
	for _, r := range rows {
		row := dashRowVM{
			Project: r.Project,
			Run:     r.Run,
			Note:    noteHTML(r.Project, r.Note),
			When:    dash.HumanAgo(now, r.When),
			URL:     rowURL(r),
			Depth:   r.Depth,
			Chained: r.Chained,
		}
		row.Agent, row.AgentTitle = agentMark(r)
		switch r.Bucket {
		case dash.BucketActiveRuns:
			out.Active = append(out.Active, row)
		case dash.BucketIntents:
			out.Intents = append(out.Intents, row)
		case dash.BucketChores, dash.BucketBacklog:
			// Chores head the backlog rather than holding a section of
			// their own — the same fold the CLI dash does, and BuildRows
			// has already ordered them ahead of the idea rows.
			out.Backlog = append(out.Backlog, row)
		case dash.BucketCompletedRuns:
			completed = append(completed, row)
		}
	}
	out.Completed = newCompletedVM(completed, topCap)
	return out
}

// servePanelVM is the serve record rendered for the web: one status line
// for the process, one line per project worth reporting on, and the
// recent-activity list from the ring. Server-rendered from memory on page
// load — no polling, no fragment fetch. Reload is the phone gesture, and
// page weight is a first-class constraint on this board.
//
// The whole panel is the /serve page. The boards spend only Cluster on
// it, in their header — status is ambient, a day's trace is not, and
// putting the trace on every dash load was the same noise the CLI's
// standalone status line was.
type servePanelVM struct {
	// Armed distinguishes a serve that may pulse from one that is
	// browse-only. An unarmed serve still renders the panel: "it's up but
	// will never pulse" is exactly the confusion this is fixing.
	Armed bool
	Up    string // "3d 2h"
	// NextSweep is how long until the next tick ("12m"), empty for an
	// unarmed serve, which never ticks.
	NextSweep string
	// Paused and Safe count the projects whose mode deviates from auto.
	// Both zero on the ordinary board, which is what keeps the cluster
	// line unchanged for an operator who never sets one.
	Paused int
	Safe   int
	// Cluster is the brief status the page headers link to /serve with —
	// the same line, byte for byte, that the CLI dash's banner carries.
	Cluster string
	// LastTick is how long ago the heartbeat last looked ("4m ago"), empty
	// before the first one. It is the liveness a page of all-quiet tick
	// entries used to communicate, in three words.
	LastTick string
	// Projects holds only the projects worth a row. Quiet ones — the ones
	// whose verdict restates "nothing to do" — collapse into QuietCount.
	Projects   []serveProjectVM
	QuietCount int
	Events     []serveEventVM
}

// serveStateQuiet is the one state /serve spends no row on: every quiet
// project's reason restates "nothing to do", so a count is the whole of
// what they add.
const serveStateQuiet = "quiet"

// serveProjectVM is one project's line in the strip: what the heartbeat
// is doing about it and why.
type serveProjectVM struct {
	Project string
	// State is the one-word status the line leads with — "sweeping",
	// "cooling", "failed", "held", "quiet".
	State  string
	Detail string // "(3m)", "(2 ticks left)", "" — qualifies State
	Reason string // the gate's own words
	// Run is the pulse run the current-or-last sweep minted, linked from
	// the row. Only the states a sweep earns carry one — sweeping,
	// cooling, failed — and it is empty when serve hasn't learned the
	// slug (a sweep that just started, one that died before minting).
	Run string
	// Mode is the project's mode when it isn't auto, empty otherwise.
	// Marked-state-only, like every other mode display: on an all-auto
	// board nothing renders.
	Mode string
	// Failed marks the states worth a colour: a dead last sweep, or a
	// cool-off serving one out.
	Failed bool
}

// serveEventVM is one line of the recent-activity list.
type serveEventVM struct {
	When string // dash.HumanAgo
	Kind string
	// Subject is the child the line leads with ("heartbeat:moe",
	// "alpha/fix-it"), empty for a tick. RunURL is the run page it links
	// to when there is one to point at: the pulse run a sweep minted, or
	// the run a run-child already names.
	Subject string
	RunURL  string
	// Text is the rest of the line: for a tick, the per-project verdicts
	// joined; for a spawn/exit, what happened to the child.
	Text   string
	Failed bool
	// Tail is the child's output snippet, shown behind a <details> on a
	// failed exit and empty everywhere else. This is what turns "sweep
	// failed, exit 1" into "sweep failed: credit limit reached".
	Tail string
}

// panel renders the activity record for the web, board-wide. Single
// operator, a ring of at most 50 events: scoping it per project would
// buy a second rendering path for a page that's one scan long.
//
// Non-trivial first, throughout. The ring keeps recording every tick —
// it is also the CLI snapshot's source and a debugging trail — but a
// board of fourteen projects produces fourteen identical quiet rows and
// a paragraph of repeated verdicts per tick, and none of it is news. The
// filtering is here, at VM-build time: quiet projects become a count,
// all-trivial ticks leave the ring, and what a wall of them was actually
// saying — the heartbeat is alive — becomes LastTick.
func (a *activity) panel(now time.Time) servePanelVM {
	a.mu.Lock()
	defer a.mu.Unlock()

	vm := servePanelVM{Armed: a.armed, Up: dash.HumanDuration(now.Sub(a.started))}
	if next := a.nextTick(); !next.IsZero() {
		vm.NextSweep = dash.HumanDuration(max(next.Sub(now), 0))
	}
	if !a.lastTick.IsZero() {
		vm.LastTick = dash.HumanAgo(now, a.lastTick)
	}
	// Read per render rather than held on the record: modes live in
	// project.json, and `moe project mode` from a terminal has to show up
	// on the next page load rather than the next tick. It also covers the
	// projects the heartbeat has no record for at all — a paused one never
	// ticks, so its mode would otherwise be invisible in the counts.
	modes := projectModes(a.root)
	for _, m := range modes {
		switch m {
		case project.ModePaused:
			vm.Paused++
		case project.ModeSafe:
			vm.Safe++
		}
	}
	failing := 0
	for _, id := range slices.Sorted(maps.Keys(a.projects)) {
		p := a.projects[id]
		// A live sweep's run exists on disk seconds after the spawn, but
		// nothing tells serve about it until the child exits — and a sweep
		// that kicked a ride can be live for hours. One small read per live
		// sweep per page load buys the mid-flight link; once seen, the
		// record keeps it.
		if p.runID == "" && !p.started.IsZero() {
			p.runID = readSweepRun(a.root, id)
		}
		line := serveProjectLine(now, id, p)
		if m, ok := modes[id]; ok && m != project.ModeAuto {
			line.Mode = string(m)
		}
		if line.Failed {
			failing++
		}
		// Counted before the collapse, so the cluster's failing count is
		// exactly the rows below it — a quiet line is never Failed, but
		// tying the two to the same predicate is what keeps that true.
		if line.State == serveStateQuiet {
			vm.QuietCount++
			continue
		}
		vm.Projects = append(vm.Projects, line)
	}
	vm.Cluster = dash.ServeCluster(vm.Armed, vm.Up, vm.NextSweep,
		dash.ModeCounts{Paused: vm.Paused, Safe: vm.Safe}, failing)
	// Newest first: the ring stores in arrival order, and the question the
	// list answers is "what just happened".
	for i := len(a.events) - 1; i >= 0; i-- {
		if line, ok := serveEventLine(now, a.events[i]); ok {
			vm.Events = append(vm.Events, line)
		}
	}
	return vm
}

// projectModes reads every registered project's mode, keyed by id.
//
// One directory scan and a small read per project, on a page load that
// already forks git for the dash gather. Warn-free: an unreadable
// projects/ leaves the map empty, which renders as an all-auto board —
// the state the page showed before modes existed.
func projectModes(root string) map[string]project.Mode {
	mds, _, err := project.List(root)
	if err != nil {
		return nil
	}
	modes := make(map[string]project.Mode, len(mds))
	for _, md := range mds {
		modes[md.ID] = project.ModeOf(md)
	}
	return modes
}

// serveProjectLine turns one project's record into its line. State
// precedence is what the operator needs first: a live sweep, then a
// cool-off holding the project back, then a dead last sweep, then a
// sweep the gate held, then quiet.
//
// held comes last of the earned states on purpose: it is this tick's
// verdict, and a project whose last sweep died has a bigger problem than
// why this tick stood down.
func serveProjectLine(now time.Time, id string, p *activityProject) serveProjectVM {
	line := serveProjectVM{Project: id, State: serveStateQuiet, Reason: p.decision}
	switch {
	case !p.started.IsZero():
		line.State = "sweeping"
		line.Detail = "(" + dash.HumanDuration(now.Sub(p.started)) + ")"
		line.Run = p.runID
	case p.coolTicks > 0:
		line.State, line.Failed = "cooling", true
		line.Detail = "(" + dash.Plural(p.coolTicks, "tick") + " left)"
		line.Reason = fmt.Sprintf("last sweep failed %s", dash.HumanAgo(now, p.sweptAt))
		line.Run = p.runID
	case !p.sweptAt.IsZero() && !p.clean:
		line.State, line.Failed = "failed", true
		line.Detail = "(" + dash.HumanAgo(now, p.sweptAt) + ")"
		// The payoff: the run a dead sweep left open is exactly what the
		// operator wants one click away.
		line.Run = p.runID
	case p.held:
		line.State = "held"
	}
	return line
}

// serveEventLine turns one ring entry into its line. A tick's body is
// the verdicts that are news — sweeps and holds, each with its reason —
// plus a tally of the ones that aren't; a child's is what happened to
// it. ok is false for a tick that was entirely trivial: it stays in the
// ring, but a "nothing happened" line is what the status line's last
// tick says once, for all of them.
func serveEventLine(now time.Time, ev activityEvent) (serveEventVM, bool) {
	vm := serveEventVM{When: dash.HumanAgo(now, ev.At), Kind: ev.Kind, Failed: ev.Failed}
	if ev.Kind == "tick" {
		var parts []string
		quiet := 0
		for _, d := range ev.Decisions {
			switch {
			case d.Sweep:
				parts = append(parts, fmt.Sprintf("%s sweeping — %s", d.Project, d.Reason))
			case d.Held:
				parts = append(parts, fmt.Sprintf("%s held — %s", d.Project, d.Reason))
			default:
				quiet++
			}
		}
		if len(parts) == 0 {
			return serveEventVM{}, false
		}
		if quiet > 0 {
			parts = append(parts, fmt.Sprintf("%d quiet", quiet))
		}
		vm.Text = strings.Join(parts, " · ")
		return vm, true
	}
	vm.Subject, vm.Text = ev.Subject, ev.Detail
	vm.RunURL = eventRunURL(ev)
	if ev.Failed {
		vm.Tail = ev.Tail
	}
	return vm, true
}

// eventRunURL is where a child event's subject links, and "" when it
// links nowhere. Two kinds of child, two ways to reach the run: a
// heartbeat sweep carries the run it minted (read out of its emit file
// at exit), and every other child's subject already *is* a run id, which
// needs no channel at all.
func eventRunURL(ev activityEvent) string {
	if project := heartbeatProject(ev.Subject); project != "" {
		if ev.Run == "" {
			return ""
		}
		return "/run/" + project + "/" + ev.Run
	}
	project, slug, ok := strings.Cut(ev.Subject, "/")
	if !ok || !slugPattern.MatchString(project) || !slugPattern.MatchString(slug) {
		return ""
	}
	return "/run/" + ev.Subject
}

// dashVM is the data the dash template renders against. Same three
// buckets as the CLI dash; COMPLETED arrives pre-capped so the template
// needs no slice math.
type dashVM struct {
	bannerArtVM
	rowBuckets
	Serve          servePanelVM
	ProjectCount   int
	ActiveProjects int
	// InputCount is how many questions are waiting on an answer. The
	// header links the queue only when this is non-zero — a count where
	// there is something to count, not a permanent landmark.
	InputCount int
}

// rowURL is where a dash row's slug links. Only a chore row differs: it
// is a registration, not a run, and has its own /chore/ page.
func rowURL(r dash.Row) string {
	if r.Bucket == dash.BucketChores {
		return "/chore/" + r.Project + "/" + r.Run
	}
	return "/run/" + r.Project + "/" + r.Run
}

func newDashVM(now time.Time, rows []dash.Row, projectCount, activeProjects int, histogram []int, topCap int) dashVM {
	return dashVM{
		bannerArtVM:    newBannerArt(now, rows, histogram),
		rowBuckets:     newRowBuckets(now, rows, topCap),
		ProjectCount:   projectCount,
		ActiveProjects: activeProjects,
	}
}
