package cli

// moe ui's board source. The journal is authoritative for what is on the
// board and what state it is in; owners' live endpoints are an overlay
// on top. Every uiDiscovery the source gathers the board (the same pass
// as dash) and reconciles one watch subscription per owned run. A
// subscription's snapshot repaints the board from the last gather with
// no new scan, so a published phase reaches the console without waiting
// for the next tick, and a gather does not poll an endpoint whose watch
// is healthy.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/ui"
)

// uiDiscovery is how often the source re-gathers the board and
// reconciles subscriptions; a failed subscription retries on it too.
var uiDiscovery = 3 * time.Second

// liveRun is one subscription and what it last heard.
type liveRun struct {
	cancel context.CancelFunc
	token  int
	status *runsock.Status
	mode   string // "watching" or "polled"
	stale  bool   // contact lost: status is last-known only
	err    error
	heard  time.Time
}

type uiSource struct {
	root     string
	filter   DashFilter
	terminal *os.File // colour gates look at it
	gather   func(now time.Time, cached liveLookup) (DashSnapshot, error)
	serve    func() serveState

	boards chan ui.Board // one slot, latest wins
	poke   chan struct{} // a subscription heard something

	mu        sync.Mutex
	live      map[string]*liveRun
	tokens    int
	snap      DashSnapshot
	serveNow  serveState
	extra     map[string]uiExtra
	gathered  time.Time
	gerr      error
	seed      int64
	artState  dash.FactoryState
	artColor  bool
	artFrames [][]string
	seq       uint64
	reports   func() []cliout.ProgramStatus

	handshakes chan struct{} // bounds concurrent subscription handshakes
	wg         sync.WaitGroup
}

// uiExtra is per-item data the board needs beyond the snapshot, read at
// gather time so a repaint does no I/O: the input record and a capture's
// canvas.
type uiExtra struct {
	input    input.File
	inputErr error
	body     string
	bodyErr  error
}

func newUISource(root string, filter DashFilter, terminal *os.File) *uiSource {
	s := &uiSource{
		root: root, filter: filter, terminal: terminal,
		serve:      func() serveState { return readServeState(root) },
		boards:     make(chan ui.Board, 1),
		poke:       make(chan struct{}, 1),
		live:       map[string]*liveRun{},
		handshakes: make(chan struct{}, observeParallel),
	}
	s.gather = func(now time.Time, cached liveLookup) (DashSnapshot, error) {
		return gatherDashSnapshot(root, now, filter, cached)
	}
	return s
}

// run gathers and publishes until ctx ends, then joins every
// subscription.
func (s *uiSource) run(ctx context.Context) {
	defer s.wg.Wait()
	tick := time.NewTicker(uiDiscovery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, l := range s.live {
				l.cancel()
			}
			s.mu.Unlock()
			return
		case <-tick.C:
			s.refresh(ctx)
		case <-s.poke:
			s.publish()
		}
	}
}

// refresh is one discovery pass: gather, reconcile, publish.
func (s *uiSource) refresh(ctx context.Context) error {
	now := time.Now()
	snap, err := s.gather(now, s.cachedLive)
	st := s.serve()
	s.mu.Lock()
	s.serveNow = st
	s.gathered, s.gerr = now, err
	if err == nil {
		s.snap = snap
		s.extra = s.readExtras(snap)
		s.seed = now.UnixNano()
		s.reconcile(ctx, snap)
	}
	s.mu.Unlock()
	s.publish()
	return err
}

// cachedLive hands the gather a fresh watched snapshot so it does not
// poll that endpoint again.
func (s *uiSource) cachedLive(key string) (runsock.Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.live[key]
	if l == nil || l.status == nil || l.stale {
		return runsock.Status{}, false
	}
	return *l.status, true
}

func (s *uiSource) readExtras(snap DashSnapshot) map[string]uiExtra {
	out := map[string]uiExtra{}
	for key, md := range snap.Runs {
		var e uiExtra
		e.input, e.inputErr = input.Load(s.root, md.Project, md.ID)
		if doc, ok := dash.CaptureDocID(md.Workflow); ok {
			b, err := os.ReadFile(s.root + "/" + run.ContentPath(md.Project, md.ID, doc))
			e.body, e.bodyErr = string(b), err
		}
		out[key] = e
	}
	return out
}

// reconcile keeps one subscription per owned run and ends the rest.
// Ownership comes from the gather's guard probe; a missing endpoint
// never stands in for "not owned". s.mu is held.
func (s *uiSource) reconcile(ctx context.Context, snap DashSnapshot) {
	owned := map[string]bool{}
	for key, o := range snap.Observations {
		if o.Owned {
			owned[key] = true
		}
	}
	for key, l := range s.live {
		if !owned[key] {
			l.cancel()
			delete(s.live, key)
		}
	}
	for key := range owned {
		if _, ok := s.live[key]; ok {
			continue
		}
		o := snap.Observations[key]
		sctx, cancel := context.WithCancel(ctx)
		s.tokens++
		l := &liveRun{cancel: cancel, token: s.tokens, mode: "watching"}
		if o.Live != nil {
			st := *o.Live
			l.status, l.heard = &st, time.Now()
		}
		s.live[key] = l
		id := runsock.Identity{Root: s.root, Project: o.Project, Run: o.Run}
		s.wg.Add(1)
		go s.subscribe(sctx, key, l.token, id)
	}
}

// subscribe follows one owner until cancelled: watch, falling back to
// polling an owner too old to watch, and retrying on the discovery
// cadence after a failure. Reconnecting always starts from a complete
// snapshot.
func (s *uiSource) subscribe(ctx context.Context, key string, token int, id runsock.Identity) {
	defer s.wg.Done()
	for ctx.Err() == nil {
		select {
		case s.handshakes <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var once sync.Once
		release := func() { once.Do(func() { <-s.handshakes }) }
		err := runsock.Watch(ctx, runsock.RuntimeDir(), id, func(st runsock.Status) {
			release()
			s.heard(key, token, &st, "watching", nil)
		})
		release()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, runsock.ErrWatchUnsupported) {
			s.poll(ctx, key, token, id)
			return
		}
		s.heard(key, token, nil, "watching", err)
		select {
		case <-time.After(uiDiscovery):
		case <-ctx.Done():
			return
		}
	}
}

// poll is the fallback for an owner that predates watch: the bounded
// one-shot status read, on the discovery cadence, labelled polled.
func (s *uiSource) poll(ctx context.Context, key string, token int, id runsock.Identity) {
	for {
		pctx, cancel := context.WithTimeout(ctx, observeProbeTimeout)
		st, err := runsock.ReadStatus(pctx, runsock.RuntimeDir(), id)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.heard(key, token, nil, "polled", err)
		} else {
			s.heard(key, token, &st, "polled", nil)
		}
		select {
		case <-time.After(uiDiscovery):
		case <-ctx.Done():
			return
		}
	}
}

// heard records a subscription's news and pokes a repaint. A callback
// from a subscription that has since been replaced is ignored.
func (s *uiSource) heard(key string, token int, st *runsock.Status, mode string, err error) {
	s.mu.Lock()
	l := s.live[key]
	if l == nil || l.token != token {
		s.mu.Unlock()
		return
	}
	l.mode = mode
	if st != nil {
		l.status, l.stale, l.err, l.heard = st, false, nil, time.Now()
	} else {
		l.stale, l.err = true, err
	}
	s.mu.Unlock()
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// publish builds a board from the last gather and the live overlay and
// offers it, replacing any board the console has not taken yet.
func (s *uiSource) publish() {
	s.mu.Lock()
	b, reports := s.buildBoard(time.Now())
	s.reports = func() []cliout.ProgramStatus { return reports }
	s.mu.Unlock()
	select {
	case <-s.boards:
	default:
	}
	s.boards <- b
}

func (s *uiSource) currentReports() []cliout.ProgramStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reports == nil {
		return nil
	}
	return s.reports()
}

// overlay is the snapshot's observations with the live subscriptions'
// latest snapshots applied, and each row's Control text recomputed from
// them. s.mu is held.
func (s *uiSource) overlay(now time.Time) ([]dash.Row, map[string]RunObservation) {
	rows := slices.Clone(s.snap.Rows)
	obs := make(map[string]RunObservation, len(s.snap.Observations))
	for key, o := range s.snap.Observations {
		if l := s.live[key]; l != nil && l.status != nil && !l.stale {
			st := *l.status
			o.Live, o.LiveErr = &st, nil
		} else if l != nil && l.stale {
			o.Live, o.LiveErr = nil, fmt.Errorf("contact lost: %v", l.err)
		}
		obs[key] = o
	}
	for i := range rows {
		if o, ok := obs[rows[i].Project+"/"+rows[i].Run]; ok {
			rows[i].Control = o.Summary(now)
		}
	}
	return rows, obs
}

// buildBoard renders the full dashboard frame into lines and indexes
// its rows as items. The art's randomness is fixed per gather, so a
// live repaint does not re-roll it. s.mu is held.
func (s *uiSource) buildBoard(now time.Time) (ui.Board, []cliout.ProgramStatus) {
	s.seq++
	b := ui.Board{Seq: s.seq, At: now, ProjectFilter: s.filter.ProjectFilter, WorkflowFilter: s.filter.WorkflowFilter}
	rows, obs := s.overlay(now)
	if s.gerr != nil {
		b.Issues = append(b.Issues, "observation failed: "+s.gerr.Error()+" (showing the last good board)")
	}
	for key, o := range obs {
		for name, err := range map[string]error{"control": o.ControlErr, "owner": o.OwnerErr, "live": o.LiveErr, "receipts": o.UnrecordedErr} {
			if err != nil {
				b.Issues = append(b.Issues, key+": "+name+" observation failed: "+err.Error())
			}
		}
	}
	for key, ex := range s.extra {
		if ex.inputErr != nil {
			b.Issues = append(b.Issues, key+": input record unreadable: "+ex.inputErr.Error())
		}
		if ex.bodyErr != nil {
			b.Issues = append(b.Issues, key+": canvas unreadable: "+ex.bodyErr.Error())
		}
	}
	slices.Sort(b.Issues)
	snap := s.snap
	snap.Rows, snap.Observations = rows, obs

	buf := &ttyFrameBuffer{terminal: s.terminal}
	banner.Dash(buf, now, ui.SafeText(s.serveNow.bannerCluster(now)))
	// Printf's reset may follow its newline. This is exactly one banner row.
	b.Banner = strings.ReplaceAll(buf.String(), "\n", "")
	buf.Reset()
	// Service detail is external text. Render without styles, then escape it
	// before adding the trusted dashboard chrome below.
	var service strings.Builder
	s.serveNow.renderLines(&service, now)
	for _, line := range strings.Split(strings.TrimSuffix(service.String(), "\n"), "\n") {
		if service.Len() > 0 {
			fmt.Fprintln(buf, ui.SafeText(line))
		}
	}
	if s.gerr != nil {
		moePrintf(buf, "observation failed: %s (showing the last good board)\n", ui.SafeText(s.gerr.Error()))
	}
	serviceLines := buf.String()
	buf.Reset()
	state := dash.FactoryStateFromRows(rows)
	histogram := dash.BuildActivityHistogram(snap.Histogram)
	color := cliout.Enabled(buf)
	if s.artFrames == nil || color != s.artColor || state.BacklogCount != s.artState.BacklogCount || state.CompletedCount != s.artState.CompletedCount || !slices.Equal(state.ActiveStages, s.artState.ActiveStages) {
		s.artFrames = dash.StyledFactoryFrames(buf, state, 8, rand.New(rand.NewSource(s.seed)))
		s.artState, s.artColor = state, color
	}
	artStart := dash.RenderOverview(buf, histogram, s.artFrames[0])
	b.Art = ui.Art{Start: artStart, Height: len(s.artFrames[0]), Frames: s.artFrames}
	b.Overview = strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	buf.Reset()
	// Escape external labels before the renderer adds its own trusted SGR.
	paintRows := slices.Clone(rows)
	for i := range paintRows {
		r := &paintRows[i]
		r.Project, r.Run = ui.SafeText(r.Project), ui.SafeText(r.Run)
		r.Note, r.Control = ui.SafeText(r.Note), ui.SafeText(r.Control)
	}
	lines := dash.RenderIndexedRows(buf, now, paintRows, snap.ProjectCount, snap.ActiveProjects, true)
	b.Lines = strings.Split(strings.TrimSuffix(serviceLines+buf.String(), "\n"), "\n")
	offset := strings.Count(serviceLines, "\n")

	for i, row := range rows {
		if lines[i] < 0 {
			continue
		}
		it := s.item(row, now, obs)
		it.Line = offset + lines[i]
		b.Items = append(b.Items, it)
	}
	// Dash omits retired standing direction. The console also offers
	// capture inspection, so list closed intents from the canonical scan
	// without changing dash rows, counts, art or status reports.
	var closed []string
	for key, md := range snap.Runs {
		if md.Workflow != dash.IntentWorkflow || md.Status != run.StatusClosed {
			continue
		}
		if s.filter.ProjectFilter != "" && md.Project != s.filter.ProjectFilter {
			continue
		}
		if s.filter.WorkflowFilter != "" && md.Workflow != s.filter.WorkflowFilter {
			continue
		}
		closed = append(closed, key)
	}
	slices.Sort(closed)
	if len(closed) > 0 {
		b.Lines = append(b.Lines, "", fmt.Sprintf("CLOSED INTENTS (%d)", len(closed)))
		for _, key := range closed {
			md := snap.Runs[key]
			it := s.item(dash.Row{Project: md.Project, Run: md.ID, Bucket: dash.BucketCompletedRuns}, now, obs)
			it.Line = len(b.Lines)
			b.Lines = append(b.Lines, "  "+ui.SafeText(it.Label())+" · closed")
			b.Items = append(b.Items, it)
		}
	}
	slices.SortStableFunc(b.Items, func(x, y ui.Item) int { return x.Line - y.Line })

	watching := 0
	for _, l := range s.live {
		if l.status != nil && !l.stale {
			watching++
		}
	}
	b.Header = fmt.Sprintf("%s · gathered %s · %d live", filterLabel(s.filter), s.gathered.Format("15:04:05"), watching)
	return b, dashStatusReports(snap, s.gerr, s.serveNow, s.filter)
}

func filterLabel(f DashFilter) string {
	parts := []string{"all projects"}
	if f.ProjectFilter != "" {
		parts[0] = "project " + f.ProjectFilter
	}
	if f.WorkflowFilter != "" {
		parts = append(parts, "workflow "+f.WorkflowFilter)
	}
	return strings.Join(parts, ", ")
}

// item is one row as the console sees it: what it is, which actions
// apply, and its detail lines. s.mu is held.
func (s *uiSource) item(row dash.Row, now time.Time, obs map[string]RunObservation) ui.Item {
	key := row.Project + "/" + row.Run
	it := ui.Item{Project: row.Project, Run: row.Run, Bucket: int(row.Bucket)}
	if row.Bucket == dash.BucketChores {
		it.Kind = ui.ItemChore
		it.Detail = []string{"chore " + row.Project + "/" + row.Run, "due: " + row.Note,
			"Chores run from the pulse; nothing here acts on them."}
		return it
	}
	md := s.snap.Runs[key]
	if md == nil {
		it.Detail = []string{key, "not found in this gather"}
		return it
	}
	ex := s.extra[key]
	switch md.Workflow {
	case dash.IdeaWorkflow:
		it.Kind = ui.ItemIdea
	case dash.IntentWorkflow:
		it.Kind = ui.ItemIntent
	}
	inProgress := md.Status == run.StatusInProgress
	if it.Kind != ui.ItemRun {
		it.Body = ex.body
		it.Editable = inProgress
		it.Notes = inProgress && it.Kind == ui.ItemIdea
		it.Detail = s.captureDetail(md, ex)
		return it
	}
	o, observed := obs[key]
	it.Notes = inProgress || md.Status == run.StatusPushed
	it.Controls = observed && (inProgress || md.Status == run.StatusPushed) && operatorCascades(md.Workflow)
	if ping, open := ex.input.OpenPing(); open && ex.inputErr == nil && it.Notes {
		it.Question = &ui.Question{ID: ping.ID, Text: ping.Question}
	}
	l := s.live[key]
	fresh := l != nil && l.status != nil && !l.stale && o.Live != nil
	if fresh {
		it.Generation = o.Live.Generation
		it.Terminal = o.Live.Terminal != nil
	}
	if observed && o.ControlErr == nil {
		it.HoldRevision = o.Control.Revision
	}
	it.Detail = s.runDetail(md, row, o, observed, l, ex, now)
	return it
}

func (s *uiSource) captureDetail(md *run.Metadata, ex uiExtra) []string {
	what := md.Workflow
	out := []string{fmt.Sprintf("%s %s/%s · %s", what, md.Project, md.ID, md.Status)}
	if md.Workflow == dash.IdeaWorkflow {
		lic := "untagged: the machine will not pick it up"
		if md.PromoteTo != "" {
			lic = "tagged " + promoteTagLabel(md.PromoteTo, md.DesignOnly) + ": a later sweep may pick it up"
		}
		out = append(out, "licence: "+lic)
	} else {
		out = append(out, "operator-authored standing direction")
	}
	out = append(out, inputLines(ex)...)
	if ex.bodyErr != nil {
		out = append(out, "canvas unreadable: "+ex.bodyErr.Error())
	}
	if md.Status == run.StatusInProgress {
		out = append(out, "e edit · :close retire")
		if md.Workflow == dash.IdeaWorkflow {
			out = append(out, "n note · :tag sdlc | :tag design | :untag")
		}
	}
	return out
}

func inputLines(ex uiExtra) []string {
	if ex.inputErr != nil {
		return []string{"input record unreadable: " + ex.inputErr.Error()}
	}
	if len(ex.input.Notes) == 0 {
		return nil
	}
	out := []string{plural(len(ex.input.Pending()), "pending note")}
	for _, e := range ex.input.Notes {
		out = append(out, "")
		if e.Open() {
			out = append(out, fmt.Sprintf("open question #%d", e.ID))
			out = append(out, strings.Split(e.Question, "\n")...)
			continue
		}
		state := "pending"
		if !e.Pending() {
			state = "delivered to " + e.DeliveredTo
		}
		kind := "note"
		if e.IsPing() {
			kind = "question"
		}
		out = append(out, fmt.Sprintf("%s #%d · %s", kind, e.ID, state))
		if e.IsPing() {
			out = append(out, "question:")
			out = append(out, strings.Split(e.Question, "\n")...)
			out = append(out, "answer:")
		}
		out = append(out, strings.Split(e.Text, "\n")...)
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (s *uiSource) runDetail(md *run.Metadata, row dash.Row, o RunObservation, observed bool, l *liveRun, ex uiExtra, now time.Time) []string {
	head := fmt.Sprintf("%s %s/%s · %s", md.Workflow, md.Project, md.ID, md.Status)
	if row.Note != "" {
		head += " · " + row.Note
	}
	out := []string{head}
	if !observed {
		if md.Status != run.StatusInProgress && md.Status != run.StatusPushed {
			out = append(out, "completed: inspect only; no live actions")
		}
		return append(out, inputLines(ex)...)
	}
	if l != nil && l.status != nil {
		switch {
		case l.stale:
			st := *l.status
			out = append(out, fmt.Sprintf("live: contact lost %s ago (%v); last known below is stale", dash.HumanDuration(now.Sub(l.heard)), l.err))
			o.Live, o.LiveErr = &st, nil
		default:
			out = append(out, "live: "+l.mode+", heard "+dash.HumanAgo(now, l.heard))
		}
	}
	out = append(out, o.Details(now)...)
	if o.ControlErr != nil {
		out = append(out, "control: unreadable — "+o.ControlErr.Error())
	} else {
		f := o.Control
		if f.Hold != nil {
			r, _ := f.Find(f.Hold.Request)
			out = append(out, fmt.Sprintf("hold: %s %s — %s", f.Hold.Kind, f.Hold.Request, holdPhase(o, r)))
		} else {
			out = append(out, "hold: none")
		}
		if f.Recovery != nil {
			out = append(out, fmt.Sprintf("recovery required: %s (%s)", f.Recovery.Reason, f.Recovery.Request))
		}
		out = append(out, o.localLines()...)
		for _, r := range f.Requests {
			if r.Settled() && now.Sub(r.AcceptedAt) > 24*time.Hour {
				continue
			}
			out = append(out, fmt.Sprintf("%s %s %s — %s", r.AcceptedAt.Local().Format("01-02 15:04"), r.Kind, r.ID, o.describe(r)))
		}
	}
	out = append(out, inputLines(ex)...)
	stage, agent := conversationAddress(md, o.Live)
	if f := o.Control; f.Hold != nil && f.Hold.Stage != "" {
		stage, agent = f.Hold.Stage, f.Hold.Agent
	}
	if cmd := conversationCommand(md, stage, agent); cmd != "" {
		out = append(out, "conversation: "+cmd)
	}
	if !operatorCascades(md.Workflow) {
		out = append(out, "controls: a "+md.Workflow+" run takes none")
	}
	return out
}
