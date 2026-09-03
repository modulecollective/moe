package serve

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/md"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// slugPattern is the kebab-case shape `moe idea new` accepts. Mirrors
// the validation moe does itself so a bad slug fails at the form rather
// than at the open. Lowercase letters, digits, and hyphens; must start
// with a letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// splitID parses the single `project/slug` field the new-idea form
// takes into its two halves, mirroring the CLI's splitProjectRun
// (internal/cli/args.go): cut on the first slash, reject either half
// empty. Kept local rather than shared because internal/cli imports
// internal/serve, so serve can't import the original back without a
// cycle — the two are meant to stay in sync by eye.
func splitID(id string) (project, slug string, err error) {
	project, slug, ok := strings.Cut(id, "/")
	if !ok || project == "" || slug == "" {
		return "", "", errors.New("expected `project/slug`")
	}
	return project, slug, nil
}

// runVM backs the per-run page (GET /run/{project}/{slug}). It is a
// static panel — no PTY tail, no chain-prompt buttons, no
// remote-controlled end-agent affordance — so the same shape covers
// both the live-parented and read-only render paths.
type runVM struct {
	ID      string
	Project string
	Slug    string
	// RowNote / RowWhen are the dash-row Note and (humanised) When for
	// this run, computed the same way the dash computes them. Empty
	// strings when the row gathered as "no row" (e.g. the run classified
	// into BucketNone, or no GatherRunRow callback wired) — template
	// falls back to the Started / Status line in that case.
	RowNote template.HTML
	RowWhen string
	// NextStage is the run's bare next-stage name (row.Stage), or "" when
	// there's no next stage / no row. The cascade trio keys off it,
	// rendering only when the next stage is spawnable (see
	// composeRunActions).
	NextStage string
	// Started / Status are the fallback meta line shown when the
	// dash-row lookup didn't return a row. Started is empty on the
	// read-only path; Status is "live" / "exited: …" / "exited
	// cleanly" / run.Status.
	Started     string
	Status      string
	Live        bool
	CanvasLinks []canvasLink
	// ChainMembers is the live batch hanging off a chain head, head→tail;
	// empty for every other workflow. The head's canvas is the operator's
	// purpose note, so this — not the canvas — is where membership and
	// per-member status live on the page.
	ChainMembers []chainMemberVM
	// Provenance is the run's origin story, root first: the actor or run
	// it descends from, then a step per hop down the spawn chain — by
	// whom, under what consent — ending at this run. Empty when no
	// callback is wired or the walk failed; the section then doesn't
	// render.
	Provenance []ProvHop
	// Actions is the peer-affordances block on the per-run page. For
	// an in-progress idea this is edit, promote, and close; for a
	// closed idea this is reopen; other runs render no actions.
	Actions []runAction
	// Inputs is the run's human-input history: every question it put to
	// the operator, the answers, and the choice buttons for the one still
	// open. Zero value renders no section, which is nearly every run.
	Inputs runInputsVM
	// Traces is what the run left behind outside its canvases —
	// follow-ups, lore, and twin notes — each landed one linked to
	// what it became. Zero value renders no sections.
	Traces runTracesVM
	// Reaped is the heartbeat reap's tombstone, rendered. Nil for nearly
	// every run: it is set only between a machine turn dying and the next
	// session start on the run clearing it.
	Reaped *reapedVM
}

// reapedVM is the warning line a tombstoned run carries above its meta:
// which stage died, when the reap noticed, and the abandoned branch tip
// the transcript is still readable at.
type reapedVM struct {
	Doc string
	At  string
	Tip string
	Ago string
}

// RunTraces is the non-canvas residue of one run, as the run page shows
// it: the checklist entries it left in followups.md, feedback/lore.md,
// and feedback/twin.md. Bodies cross the seam as markdown; serve
// renders them, same as every other doc.
type RunTraces struct {
	Followups []RunTrace
	Lore      []RunTrace
	Twin      []RunTrace
}

// RunTrace is one checklist entry from followups.md, feedback/lore.md,
// or feedback/twin.md. A harvested entry (Done) that still resolves to
// what it became carries TargetURL; one the operator checked by hand to
// drop it doesn't. Raw is set instead of Slug/Title when the line
// didn't match the grammar — display is lenient where harvest is
// strict, so a malformed file degrades to plain text rather than a 500.
type RunTrace struct {
	Done  bool
	Raw   string
	Slug  string
	Title string
	Body  string // markdown; the indented `Why:` block, "" when absent
	// TargetURL is the promoted idea run (/run/<p>/<slug>) or lore entry
	// (/lore/<slug>); "" when nothing landed or the target is gone.
	TargetURL string
	// TargetStatus is the target run's current status, rendered as a
	// badge. Empty for lore, which has no lifecycle.
	TargetStatus string
}

// runTracesVM is the rendered form of RunTraces.
type runTracesVM struct {
	Followups []runTraceVM
	Lore      []runTraceVM
	Twin      []runTraceVM
}

type runTraceVM struct {
	Done         bool
	Raw          string
	Slug         string
	Title        string
	BodyHTML     template.HTML
	TargetURL    string
	TargetStatus string
}

// reapedNotice renders md.Reaped for the run page, or nil when the run
// carries no tombstone — which is nearly always. An unparseable
// timestamp still renders: the sha is the part that has to reach the
// operator, and dropping the whole notice over a bad clock field would
// lose exactly what the note exists to preserve.
func reapedNotice(md *run.Metadata, now time.Time) *reapedVM {
	if md == nil || md.Reaped == nil {
		return nil
	}
	vm := &reapedVM{Doc: md.Reaped.Doc, At: md.Reaped.At, Tip: git.ShortSHA(md.Reaped.Tip)}
	if t, err := time.Parse(time.RFC3339, md.Reaped.At); err == nil {
		vm.Ago = dash.HumanAgo(now, t)
	}
	return vm
}

// gatherRunTraces resolves the run's non-canvas traces. No callback
// wired, or a gather error, yields the zero value — the page renders as
// it did before the sections existed. Same posture as gatherChainState:
// a broken trace file must degrade its section, never the page.
func (s *Server) gatherRunTraces(projectID, slug string) runTracesVM {
	if s.opts.GatherRunTraces == nil {
		return runTracesVM{}
	}
	traces, err := s.opts.GatherRunTraces(projectID, slug)
	if err != nil {
		s.logf("run traces %s/%s: %v", projectID, slug, err)
		return runTracesVM{}
	}
	return runTracesVM{
		Followups: traceVMs(traces.Followups),
		Lore:      traceVMs(traces.Lore),
		Twin:      traceVMs(traces.Twin),
	}
}

func traceVMs(traces []RunTrace) []runTraceVM {
	var out []runTraceVM
	for _, t := range traces {
		vm := runTraceVM{
			Done:         t.Done,
			Raw:          t.Raw,
			Slug:         t.Slug,
			Title:        t.Title,
			TargetURL:    t.TargetURL,
			TargetStatus: t.TargetStatus,
		}
		if t.Body != "" {
			vm.BodyHTML = template.HTML(md.Render(t.Body, nil))
		}
		out = append(out, vm)
	}
	return out
}

// runAction is one peer affordance on the per-run page. Empty Method
// renders as a link; POST renders as a small form button.
type runAction struct {
	Label  string
	Href   string
	Method string
}

type canvasLink struct {
	Stage   string
	URL     string // /run/<p>/<r>/canvas/<stage>
	ModTime string // human "Xm ago"
	// Transcripts are the per-agent transcript links for this stage (one
	// per backend thread on disk), rendered beside the canvas link.
	Transcripts []transcriptLink
}

type transcriptLink struct {
	Agent string // "claude" | "codex"
	URL   string // /run/<p>/<r>/transcript/<stage>?agent=<agent>
}

// ProvHop is one line of the per-run provenance section: a single claim
// about how some run came to be, already resolved to display strings so
// serve does no journal reading of its own. The cli side builds these
// (it owns the journal index and the pulse-canvas reader) and hands them
// over through Options.RunProvenance.
//
// The rendered shape is "<Subject> <Verb> <Object>". Hops come root
// first and the page draws every one after the first with a leading
// arrow, so all but the first carry an empty Subject: it is always the
// hop above, and the arrow already says so.
type ProvHop struct {
	// Subject is who the story starts from — "operator", or the run or
	// idea the chain descends from, qualified "<project>/<slug>". Set on
	// the first hop only.
	Subject    string
	SubjectURL string
	// Verb is the relationship prose, read downward: "opened", "spawned",
	// "promoted to", "reopened as", "shipped by" — plus "opened by
	// operator" for the one-line story that needs no chain.
	Verb string
	// Object is what the verb points at, qualified; "this run" (unlinked)
	// on the hop that lands on the page's own run, and empty for terminal
	// verbs that name nobody ("opened by operator").
	Object    string
	ObjectURL string
	// Agent marks the hop as the machine's doing, and Consent is the ride
	// level it acted under. Consent can be set while Agent is false only
	// if a writer regresses; readers treat Agent as the marker and
	// Consent as its detail. Empty Consent means unrecorded — the hop
	// renders no consent word rather than guessing one.
	Agent   bool
	Consent string
	// Why is the reason the spawner recorded for this run (a pulse gate's
	// spawn entry). Empty when unrecoverable — an old pulse, an edited
	// canvas — which degrades to a hop with no reason, never an error.
	Why string
}

// chainMemberVM is one run in a chain head's batch, rendered with the
// dash's own note and timestamp so the head page and the dash agree
// about what a member's state is called.
type chainMemberVM struct {
	ID   string // <project>/<slug>
	URL  string // /run/<project>/<slug>
	Note template.HTML
	When string
	// EdgeAgent / EdgeConsent mark the edge that placed this member here
	// as a pulse groom's doing, and name the ride it groomed under.
	// See dash.Row's fields of the same name — absence is unknown.
	EdgeAgent   bool
	EdgeConsent string
}

// gatherChainMembers reads the live batch behind a chain head — the runs
// `moe chain kick` would actually ride, so the page's count can't lie
// about what a kick from the terminal will walk. Everything else — a
// non-chain run, no callback wired, a gather error — yields nothing,
// which renders as the read-only page chain heads had before. A gather
// error is logged and swallowed rather than failing the page, matching
// fillRunRow: the canvas link and the meta line are still worth serving.
func (s *Server) gatherChainMembers(md *run.Metadata, projectID, slug string, now time.Time) []chainMemberVM {
	if md.Workflow != dash.ChainWorkflow || s.opts.ChainMembers == nil {
		return nil
	}
	rows, err := s.opts.ChainMembers(projectID, slug)
	if err != nil {
		s.logf("chain members %s/%s: %v", projectID, slug, err)
		return nil
	}
	var out []chainMemberVM
	for _, row := range rows {
		id := row.Project + "/" + row.Run
		out = append(out, chainMemberVM{
			ID:          id,
			URL:         "/run/" + id,
			Note:        noteHTML(row.Project, row.Note),
			When:        dash.HumanAgo(now, row.When),
			EdgeAgent:   row.EdgeAgent,
			EdgeConsent: row.EdgeConsent,
		})
	}
	return out
}

// gatherProvenance reads the run's origin story. A missing callback or a
// gather error yields nothing and logs, matching fillRunRow: provenance
// is enrichment, and a page without it is still the page.
func (s *Server) gatherProvenance(projectID, slug string) []ProvHop {
	if s.opts.RunProvenance == nil {
		return nil
	}
	hops, err := s.opts.RunProvenance(projectID, slug)
	if err != nil {
		s.logf("provenance %s/%s: %v", projectID, slug, err)
		return nil
	}
	return hops
}

// loadRunOr404 loads a run's metadata for a handler whose only job on
// failure is to say so: a missing run is a 404, anything else a logged
// 500. verb prefixes both the 500 body and the log line ("close",
// "promote form", …). ok is false when the response has already been
// written, so callers just return.
//
// Deliberately not used by the post-mutation error switches
// (closeCaptureRun, handleIdeaReopen, handleCaptureEditSubmit): those
// map errors out of a mutation call and have conflict cases of their
// own, which this would have to grow knobs for.
func (s *Server) loadRunOr404(w http.ResponseWriter, projectID, slug, verb string) (*run.Metadata, bool) {
	id := projectID + "/" + slug
	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return nil, false
		}
		s.logf("%s %s: load: %v", verb, id, err)
		http.Error(w, verb+": "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return md, true
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if c, ok := s.children.get(id); ok {
		s.render(w, r, "run.html", s.buildRunVM(c, projectID, slug, id))
		return
	}
	vm, err := s.buildReadOnlyRunVM(projectID, slug, id)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("run page: %v", err)
		http.Error(w, "run page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "run.html", vm)
}

// handleClose closes a run from the per-run page. It loads the run's
// metadata and dispatches by workflow: capture runs (idea, intent) flip
// closed in-process via runopen.CloseCapture (no harvest, no sandbox);
// everything else
// routes through the CloseRun callback, which dispatches the full cli
// close pipeline by the run's own workflow with --no-edit semantics.
// One route, one guard set, regardless of run kind — a stale or
// replayed POST hits the same refusals.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, ok := s.loadRunOr404(w, projectID, slug, "close")
	if !ok {
		return
	}

	if dash.IsCapture(md.Workflow) {
		s.closeCaptureRun(w, r, projectID, slug, id, md.Workflow)
		return
	}
	s.closeWorkflowRun(w, r, projectID, slug, id)
}

func (s *Server) closeCaptureRun(w http.ResponseWriter, r *http.Request, projectID, slug, id, workflow string) {
	if err := runopen.CloseCapture(s.opts.Root, projectID, slug); err != nil {
		switch {
		case errors.Is(err, run.ErrRunNotFound):
			http.Error(w, "no such run: "+id, http.StatusNotFound)
		case errors.Is(err, runopen.ErrNotCapture):
			http.Error(w, "run "+id+" is not a closable "+workflow, http.StatusConflict)
		default:
			s.logf("close %s %s: %v", workflow, id, err)
			http.Error(w, "close "+workflow+": "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// closeWorkflowRun closes an in-progress non-capture run through the
// CloseRun callback, which dispatches the registered close pipeline by
// the run's workflow. serve owns the PTY children it spawned, so the
// one guard it applies itself is the live-child refusal: closing while
// the agent is mid-turn would yank the sandbox clone out from under
// it. Every other guard (pushed, terminal, canvas-empty, no registered
// close) lives on the cli side and surfaces through the callback's
// error.
func (s *Server) closeWorkflowRun(w http.ResponseWriter, r *http.Request, projectID, slug, id string) {
	if s.opts.CloseRun == nil {
		http.Error(w, "close not configured (Options.CloseRun is nil)", http.StatusInternalServerError)
		return
	}
	if c, ok := s.children.get(id); ok {
		if exited, _ := c.snapshot(); !exited {
			http.Error(w,
				"run "+id+" has a live agent mid-turn — wait for it to finish, then close",
				http.StatusConflict)
			return
		}
	}
	if err := s.opts.CloseRun(projectID, slug); err != nil {
		if _, ok := errors.AsType[*runopen.NotClosableError](err); ok {
			http.Error(w, "close: "+err.Error(), http.StatusConflict)
			return
		}
		s.logf("close run %s: %v", id, err)
		http.Error(w, "close: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The run is gone; drop any lingering exited-child entry so the dash
	// and run page stop marking it parented.
	s.children.remove(id)
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

func (s *Server) handleIdeaReopen(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := runopen.ReopenIdea(s.opts.Root, projectID, slug); err != nil {
		switch {
		case errors.Is(err, run.ErrRunNotFound):
			http.Error(w, "no such run: "+id, http.StatusNotFound)
		case errors.Is(err, runopen.ErrNotReopenableIdea):
			http.Error(w, "reopen idea: "+err.Error(), http.StatusConflict)
		default:
			s.logf("reopen idea %s: %v", id, err)
			http.Error(w, "reopen idea: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// handleIdeaTag stamps a workflow tag onto an in-progress idea. The
// workflow rides the chip's query string (`?workflow=sdlc`) and is
// resolved against Options.TagWorkflows; an absent value takes that
// list's default. `design_only=1` alongside it narrows the licence to
// one design turn — the second chip, same route, because it is the same
// write. Journal-only — the tag is a licence a later sweep spends, it
// starts nothing here.
func (s *Server) handleIdeaTag(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("workflow"))
	wf, ok := s.tagWorkflow(name)
	if !ok {
		http.Error(w, "tag idea: unknown workflow "+strconv.Quote(name), http.StatusBadRequest)
		return
	}
	s.setIdeaTag(w, r, wf, r.FormValue("design_only") == "1")
}

// tagWorkflow resolves a submitted (or query-string) workflow name
// against Options.TagWorkflows. An empty name falls back to the first
// entry — the list's default — so a stale page that POSTs without the
// field keeps working.
func (s *Server) tagWorkflow(name string) (string, bool) {
	if name == "" && len(s.opts.TagWorkflows) > 0 {
		return s.opts.TagWorkflows[0], true
	}
	if slices.Contains(s.opts.TagWorkflows, name) {
		return name, true
	}
	return "", false
}

// handleIdeaUntag clears an idea's workflow tag — the per-idea pause.
// It clears the design-only narrowing with it: untag is the removal of
// the whole licence, not of one of its halves.
func (s *Server) handleIdeaUntag(w http.ResponseWriter, r *http.Request) {
	s.setIdeaTag(w, r, "", false)
}

// setIdeaTag is the body both tag routes share. An idea already in the
// requested state comes back as run.ErrNothingToCommit, which is a
// success: a double-tap on the chip is a no-op, not a 500.
func (s *Server) setIdeaTag(w http.ResponseWriter, r *http.Request, workflow string, designOnly bool) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	err := runopen.TagIdea(s.opts.Root, projectID, slug, workflow, designOnly)
	switch {
	case err == nil, errors.Is(err, run.ErrNothingToCommit):
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, runopen.ErrNotTaggableIdea):
		http.Error(w, "tag idea: "+err.Error(), http.StatusConflict)
		return
	default:
		s.logf("tag idea %s: %v", id, err)
		http.Error(w, "tag idea: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// handleAdvance marks the run's current stage advanced: the operator
// read the canvas and approves, so the run's next pickup starts at the
// successor stage instead of re-opening this one. Journal-only — an
// empty marker commit and a push, no agent, no child — which is why it
// carries no dynamic-mode gate. The mark is one of the shapes a kick
// already admits, and the commit moves the journal, so the next
// heartbeat tick sweeps and rides it.
//
// The "advance past <stage>" chip on the per-run page posts here.
// Everything is re-derived server-side; the button is never trusted.
func (s *Server) handleAdvance(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, ok := s.loadRunOr404(w, projectID, slug, "advance")
	if !ok {
		return
	}
	// A live agent mid-turn is about to land its own work turn, which
	// would out-date a marker written now. Mirror the close route's
	// live-child refusal.
	if c, ok := s.children.get(id); ok {
		if exited, _ := c.snapshot(); !exited {
			http.Error(w,
				"run "+id+" has a live agent mid-turn — wait for it to finish, then advance",
				http.StatusConflict)
			return
		}
	}
	stage, ok := s.markableStage(w, projectID, slug, md)
	if !ok {
		return
	}
	// A replayed POST is harmless and deliberately not special-cased: a
	// second marker is the same claim as the first, and stageSatisfied
	// reads the freshest one.
	switch err := runopen.MarkAdvanced(s.opts.Root, projectID, slug, stage); {
	case err == nil:
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, runopen.ErrNotAdvanceable):
		http.Error(w, "advance: "+err.Error(), http.StatusConflict)
		return
	default:
		s.logf("advance %s: %v", id, err)
		http.Error(w, "advance: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// markableStage re-derives which stage an advance mark would land on and
// refuses when the answer isn't one the web may mark. Shared by the
// route and the chip: whatever the page offers, the POST re-asks.
//
// The stage is the run's own next stage — the one it is parked at, which
// for a "worked, not advanced" run is the stage whose canvas the
// operator just read. It must be in the workflow's declared set, which
// is what keeps push out: sdlc excludes it, and a marker on push would
// satisfy the ladder without anything ever being pushed.
//
// ok=false means the response is already written.
func (s *Server) markableStage(w http.ResponseWriter, projectID, slug string, md *run.Metadata) (string, bool) {
	id := projectID + "/" + slug
	if s.opts.WorkflowUI == nil {
		http.Error(w, "advance not configured (Options.WorkflowUI is nil)", http.StatusInternalServerError)
		return "", false
	}
	ui, ok := s.opts.WorkflowUI(md.Workflow)
	if !ok {
		http.Error(w, "workflow "+md.Workflow+" does not advance from serve", http.StatusConflict)
		return "", false
	}
	stage, err := s.nextStage(projectID, slug)
	if err != nil {
		s.logf("advance %s: next stage: %v", id, err)
		http.Error(w, "advance: "+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	if !slices.Contains(ui.Stages, stage) {
		http.Error(w,
			"run "+id+" has no markable stage (next="+strconv.Quote(stage)+")",
			http.StatusConflict)
		return "", false
	}
	// A marker landed against a blocked gate is inert — stageSatisfied
	// ANDs the gate in, and the re-edit turn that later flips it
	// out-dates the marker anyway. Refusing here buys no safety; it buys
	// honesty. A stale tab (chip rendered while the gate was ready) or a
	// hand-rolled POST would otherwise get a 303 and silence, which is
	// exactly the failure this check exists to kill.
	if s.opts.CheckStageGate != nil {
		switch ok, err := s.opts.CheckStageGate(md, stage); {
		case err != nil:
			s.logf("advance %s: gate for %s: %v", id, stage, err)
			http.Error(w, "advance: "+err.Error(), http.StatusInternalServerError)
			return "", false
		case !ok:
			http.Error(w,
				"run "+id+": stage "+stage+" gate not satisfied",
				http.StatusConflict)
			return "", false
		}
	}
	return stage, true
}

// nextStage re-derives a run's bare next-stage name through the
// GatherRunRow callback (the same lookup fillRunRow uses for the
// dash-row meta). Returns "" when no callback is wired or the row
// gathered as not-found / filtered — callers treat "" as "no
// advanceable stage" and refuse.
func (s *Server) nextStage(projectID, slug string) (string, error) {
	if s.opts.GatherRunRow == nil {
		return "", nil
	}
	row, ok, err := s.opts.GatherRunRow(projectID, slug)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return row.Stage, nil
}

// stageWorked reports whether a stage has a committed work turn — the
// half of stageSatisfied's advance rule a marker can't supply itself.
// A read failure reads as "no": offering a mark the route would refuse
// is worse than not offering it, and the CLI's chain prompt is still
// there.
func (s *Server) stageWorked(projectID, slug, stage string) bool {
	sha, _, err := run.LatestWorkTurnSHA(s.opts.Root, projectID, slug, stage)
	if err != nil {
		s.logf("advance chip %s/%s: work turn for %s: %v", projectID, slug, stage, err)
		return false
	}
	return sha != ""
}

// stageGateOK reports whether a stage's satisfiability gate passes —
// the other half stageSatisfied wants, and the half a stage can fail
// while still having a work turn (a test canvas committed with its gate
// fence blocked). Same log-and-false rule as stageWorked: a read
// failure reads as "no", because offering a mark the ladder would
// ignore is worse than not offering it.
//
// Gate-less stages (design, code) report true, so the only canvas read
// this adds is on stages that registered one.
func (s *Server) stageGateOK(projectID, slug, stage string, md *run.Metadata) bool {
	if s.opts.CheckStageGate == nil {
		return true
	}
	ok, err := s.opts.CheckStageGate(md, stage)
	if err != nil {
		s.logf("advance chip %s/%s: gate for %s: %v", projectID, slug, stage, err)
		return false
	}
	return ok
}

// buildReadOnlyRunVM constructs a runVM from on-disk state for a run
// not currently parented by this serve. For in-progress idea runs,
// the page surfaces edit/promote affordances; for sdlc runs, just the
// dash-row meta and canvas links.
func (s *Server) buildReadOnlyRunVM(projectID, slug, id string) (runVM, error) {
	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		return runVM{}, err
	}
	now := time.Now()
	vm := runVM{
		ID:          id,
		Project:     projectID,
		Slug:        slug,
		Status:      md.Status,
		CanvasLinks: s.canvasLinks(projectID, slug, now),
		Traces:      s.gatherRunTraces(projectID, slug),
		Inputs:      s.gatherRunInputs(projectID, slug, md),
		Reaped:      reapedNotice(md, now),
	}
	s.fillRunRow(&vm, projectID, slug, now)
	vm.Provenance = s.gatherProvenance(projectID, slug)
	vm.ChainMembers = s.gatherChainMembers(md, projectID, slug, now)
	// No live child on the read-only path (this serve isn't parenting the
	// run), so the chips gate on live=false. fillRunRow ran first so
	// vm.NextStage is populated.
	vm.Actions = s.composeRunActions(projectID, slug, vm.NextStage, md, false)
	return vm, nil
}

// composeRunActions returns the peer-affordances list for the per-run
// page. Every chip here writes a journal commit or nothing at all —
// none of them starts an agent, which is the heartbeat's job alone — so
// none is gated on dynamic mode.
//
// Ideas keep their bespoke chips (edit / tag / untag / close / reopen —
// idea has no stage ladder to derive from). Staged workflows get the
// advance mark, keyed off the re-derived next stage and its declared
// stage set (Options.WorkflowUI). Workflows with a close pipeline get a
// close-run chip when close is the routine idle-page next move;
// perpetual workflows keep close off the idle page but still expose it
// while a child is live. A chain head gets nothing: it has no stages to
// mark, and kicking one is a terminal act (`moe chain kick`) because a
// hand-staged head is a deliberate staging fence.
//
// nextStage is the bare next-stage name re-derived from the dash row;
// live is true when an agent is mid-turn. The advance mark drops while
// live — that agent is about to land a work turn that would out-date the
// marker. Close chips stay for non-perpetual workflows, and for live
// perpetual pages; the close route's own live-child refusal guards the
// click.
func (s *Server) composeRunActions(projectID, slug, nextStage string, md *run.Metadata, live bool) []runAction {
	base := "/run/" + projectID + "/" + slug
	if md.Workflow == dash.ChainWorkflow {
		return nil
	}
	if dash.IsCapture(md.Workflow) {
		switch md.Status {
		case run.StatusInProgress:
			// edit / close are journal-only, so both captures get them
			// on an unarmed serve. So is tagging: the tag parks a
			// licence, and the sweep that spends it rides under its own
			// consent. Tag chips stay idea-only — ideas are the only
			// capture that promotes, and promotion is now the sweep's
			// move, not a button's.
			out := []runAction{{Label: "edit " + md.Workflow, Href: base + "/edit"}}
			if md.Workflow == dash.IdeaWorkflow {
				out = append(out, ideaTagActions(base, md, s.opts.TagWorkflows)...)
			}
			return append(out, runAction{Label: "close " + md.Workflow, Href: base + "/close", Method: "POST"})
		case run.StatusClosed:
			// Reopen stays idea-only: the intent verb set has no reopen
			// (cli/intent.go), and the web must not exceed the CLI's.
			if md.Workflow == dash.IdeaWorkflow {
				return []runAction{
					{Label: "reopen idea", Href: base + "/reopen", Method: "POST"},
				}
			}
		}
		return nil
	}
	if s.opts.WorkflowUI == nil {
		return nil
	}
	ui, ok := s.opts.WorkflowUI(md.Workflow)
	if !ok || md.Status != run.StatusInProgress {
		return nil
	}
	var out []runAction
	// A "" or excluded next stage (sdlc's push) yields no stage chips:
	// push stays terminal/CLI-only — the bang vocabulary collapses there
	// — so a run parked right before push shows only the close chip.
	markable := !live && slices.Contains(ui.Stages, nextStage)
	// The advance mark is journal-only — an empty marker commit a later
	// tick rides — so it stays on an unarmed serve, beside the idea
	// chips. It renders only where the mark would mean something:
	// stageSatisfied wants a marker, a work turn, *and* the stage's own
	// gate, so a mark on a never-worked stage or one whose canvas says
	// blocked is a mark the ladder ignores. The gate check trails the
	// work-turn check so the canvas read only happens for a worked
	// stage.
	if markable && s.stageWorked(projectID, slug, nextStage) &&
		s.stageGateOK(projectID, slug, nextStage, md) {
		out = append(out, runAction{Label: "advance past " + nextStage, Href: base + "/advance", Method: "POST"})
	}
	if ui.Close && (!ui.Perpetual || live) {
		out = append(out, runAction{Label: "close run", Href: base + "/close", Method: "POST"})
	}
	return out
}

// fillRunRow populates RowNote / RowWhen from the dash-row lookup.
// Errors are swallowed (logged) so a row-gather hiccup never breaks
// the per-run page; the template falls back to the Started / Status
// meta line when the row note is empty.
func (s *Server) fillRunRow(vm *runVM, projectID, slug string, now time.Time) {
	if s.opts.GatherRunRow == nil {
		return
	}
	row, ok, err := s.opts.GatherRunRow(projectID, slug)
	if err != nil {
		s.logf("run row gather %s/%s: %v", projectID, slug, err)
		return
	}
	if !ok {
		return
	}
	vm.RowNote = noteHTML(row.Project, row.Note)
	vm.RowWhen = dash.HumanAgo(now, row.When)
	vm.NextStage = row.Stage
}

// ideaTagActions are the tag/untag chips on an in-progress idea — the
// dash face of `moe idea tag`, and the operator's one-tap way to hand
// the machine a license to start a parked idea.
//
// Two chips per entry in Options.TagWorkflows — the same list the tag
// route resolves against, so the two surfaces can't disagree on what a
// valid destination is — rendered as chips rather than a select because
// the chip row carries no form fields. "tag <workflow>" is the ship
// licence; "tag <workflow>, design only" narrows it to one design turn
// and a hold, which is the phone-shaped rung between shipping and
// waiting for a terminal. The pair the idea already carries gets no
// chip (re-tapping it would be a no-op), so the row always shows the
// states the idea isn't in. An "untag" chip appears once any tag is on,
// which is the per-idea pause.
func ideaTagActions(base string, md *run.Metadata, workflows []string) []runAction {
	var out []runAction
	for _, wf := range workflows {
		current := wf == md.PromoteTo
		if !current || md.DesignOnly {
			out = append(out, runAction{
				Label:  "tag " + wf,
				Href:   base + "/tag?workflow=" + url.QueryEscape(wf),
				Method: "POST",
			})
		}
		if !current || !md.DesignOnly {
			out = append(out, runAction{
				Label:  "tag " + wf + ", design only",
				Href:   base + "/tag?workflow=" + url.QueryEscape(wf) + "&design_only=1",
				Method: "POST",
			})
		}
	}
	if md.PromoteTo != "" {
		out = append(out, runAction{Label: "untag", Href: base + "/untag", Method: "POST"})
	}
	return out
}

// buildRunVM assembles the per-run page from the live child's state
// and the on-disk canvas listing.
func (s *Server) buildRunVM(c *child, projectID, slug, id string) runVM {
	exited, exitErr := c.snapshot()
	now := time.Now()
	vm := runVM{
		ID:      id,
		Project: projectID,
		Slug:    slug,
		Started: dash.HumanAgo(now, c.started),
		Live:    !exited,
	}
	switch {
	case !exited:
		vm.Status = "live"
	case exitErr != nil:
		vm.Status = "exited: " + exitErr.Error()
	default:
		vm.Status = "exited cleanly"
	}
	vm.CanvasLinks = s.canvasLinks(projectID, slug, now)
	vm.Traces = s.gatherRunTraces(projectID, slug)
	s.fillRunRow(&vm, projectID, slug, now)
	vm.Provenance = s.gatherProvenance(projectID, slug)
	// A live-parented run is usually sdlc, but opening a chore can spawn
	// any configured workflow (e.g. a chore's own workflow), so don't
	// assume the workflow here — gate the action chips on the on-disk
	// metadata (composeRunActions keys off the workflow's declaration).
	// A load failure just drops the chips.
	if md, err := run.Load(s.opts.Root, projectID, slug); err != nil {
		s.logf("run page %s: load for actions: %v", id, err)
	} else {
		vm.Inputs = s.gatherRunInputs(projectID, slug, md)
		vm.Reaped = reapedNotice(md, now)
		vm.ChainMembers = s.gatherChainMembers(md, projectID, slug, now)
		// !exited == an agent mid-turn; composeRunActions drops the
		// advance mark in that case. fillRunRow above populated
		// vm.NextStage.
		vm.Actions = s.composeRunActions(projectID, slug, vm.NextStage, md, !exited)
	}
	return vm
}

// canvasLinks enumerates the run's stage canvas files (rendered in
// workflow ladder order) with their mtimes. Only stages whose
// content.md actually exists are surfaced.
//
// Resolution routes through Options.ResolveCanvas — the same callback
// the canvas route and `moe sdlc cat` use — so an in-progress run
// whose canonical-root documents/ is empty still surfaces links to
// the live session's worktree copy. Before this, canvasLinks did its
// own `ReadDir` on the canonical docs dir; for in-progress runs that
// directory is empty (the agent edits live under .moe/worktrees/…),
// so no links were emitted and the canvas was effectively invisible
// on the run page.
//
// A nil ResolveCanvas or RunStages yields no links. `moe serve` wires
// both in cli/serve.go; tests that want canvas links must too. Note
// that the session-vs-canonical decision baked into ResolveCanvas
// depends on session.List finding worktrees under <Root>/.moe — i.e.
// serve must run from the bureaucracy root, not from inside a session
// worktree, or the live-edit branch silently falls back to canonical.
func (s *Server) canvasLinks(projectID, slug string, now time.Time) []canvasLink {
	if s.opts.ResolveCanvas == nil || s.opts.RunStages == nil {
		return nil
	}
	ladder, err := s.opts.RunStages(projectID, slug)
	if err != nil {
		return nil
	}
	out := make([]canvasLink, 0, len(ladder))
	for _, stage := range ladder {
		path, err := s.opts.ResolveCanvas(projectID, slug, stage)
		if err != nil {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		out = append(out, canvasLink{
			Stage:       stage,
			URL:         "/run/" + projectID + "/" + slug + "/canvas/" + stage,
			ModTime:     dash.HumanAgo(now, st.ModTime()),
			Transcripts: s.transcriptLinks(projectID, slug, stage),
		})
	}
	return out
}

// transcriptLinks returns the per-agent transcript links for a stage —
// one per backend thread present on the canonical path. Threads mirror
// there when a turn closes, so an in-progress stage mid-first-turn has
// none yet (accurate as of the last closed turn, same posture as the
// canvas route's live view). Chat runs are where this pays most: the
// chat canvas is only session markers, the transcript is the content.
func (s *Server) transcriptLinks(projectID, slug, stage string) []transcriptLink {
	var links []transcriptLink
	for _, agent := range []string{"claude", "codex"} {
		path := filepath.Join(s.opts.Root, run.ThreadPathFor(agent, projectID, slug, stage))
		if !fileExists(path) {
			continue
		}
		links = append(links, transcriptLink{
			Agent: agent,
			URL:   "/run/" + projectID + "/" + slug + "/transcript/" + stage + "?agent=" + agent,
		})
	}
	return links
}

// listProjectIDs returns the sorted set of registered project IDs.
// Shared by the new-run and new-idea forms; the idea form needs
// nothing else from gatherNewRunVM, so this stays a small helper
// rather than dragging a workspace listing through the idea path.
func (s *Server) listProjectIDs() ([]string, error) {
	mds, warns, err := project.List(s.opts.Root)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		s.logf("project list: skipping %s: %v", w.ID, w.Err)
	}
	projectIDs := make([]string, 0, len(mds))
	for _, md := range mds {
		projectIDs = append(projectIDs, md.ID)
	}
	sort.Strings(projectIDs)
	return projectIDs, nil
}

// requireKnownProject rejects a project id that isn't in the registered
// set, mirroring the CLI's requireProject (internal/cli/idea.go) so the
// web forms fail the same way the CLI does. The dropdown the forms used
// to carry made an unknown project unreachable; a free-text field
// doesn't, so the check moves server-side — catching it here yields a
// clean "unknown project" banner instead of leaking a downstream
// runopen.Open error.
func (s *Server) requireKnownProject(projectID string) error {
	ids, err := s.listProjectIDs()
	if err != nil {
		return err
	}
	if !slices.Contains(ids, projectID) {
		return errors.New("unknown project: " + projectID)
	}
	return nil
}

// newIdeaVM backs the new-idea form. Projects are gathered from disk
// at request time; there are no workspace / agent dropdowns because
// idea runs don't host a PTY session and have no workspace binding.
type newIdeaVM struct {
	Projects    []string
	ErrorBanner string
	// ID, Body echo the operator's submitted values back on an error
	// re-render so a validation failure doesn't wipe a typed-out idea.
	// ID is the raw `project/slug` text, echoed verbatim.
	ID   string
	Body string
}

func (s *Server) handleNewIdeaForm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.gatherNewIdeaVM()
	if err != nil {
		s.logf("new-idea form gather: %v", err)
		http.Error(w, "new-idea form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "new_idea.html", vm)
}

// handleNewIdeaSubmit validates the form and opens an idea run
// in-process via runopen.Open. No PTY spawn — idea runs are a
// single-stage doc with no live agent — so the handler redirects
// straight to the per-run page once the open commit lands.
//
// Body is taken verbatim with CRLF normalised to LF (browsers send
// \r\n in textarea bodies; canvases live on disk as LF). An empty
// body falls back to "# {slug}\n", matching the CLI stub
// (internal/cli/idea.go:185).
func (s *Server) handleNewIdeaSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	body := strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")
	// Echo the raw typed id and body on every error path so the operator
	// never loses a multi-line idea to a validation slip.
	fail := func(msg string) { s.renderIdeaFormError(w, r, id, body, msg) }

	projectID, slug, err := splitID(id)
	if err != nil {
		fail(err.Error())
		return
	}
	if !slugPattern.MatchString(slug) {
		fail("slug: must be kebab-case (lowercase, digits, hyphens; start with letter/digit)")
		return
	}
	if err := s.requireKnownProject(projectID); err != nil {
		fail(err.Error())
		return
	}

	seed := body
	if seed == "" {
		seed = "# " + slug + "\n"
	}
	md, err := runopen.Open(s.opts.Root, projectID, run.Options{
		ID:       slug,
		Workflow: dash.IdeaWorkflow,
		SeedDocs: map[string]string{dash.IdeaDocID: seed},
	})
	if err != nil {
		fail("open: " + err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+md.Project+"/"+md.ID, http.StatusSeeOther)
}

func (s *Server) renderIdeaFormError(w http.ResponseWriter, r *http.Request, id, body, msg string) {
	vm, err := s.gatherNewIdeaVM()
	if err != nil {
		http.Error(w, msg+" (and form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	vm.ID = id
	vm.Body = body
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "new_idea.html", vm)
}

func (s *Server) gatherNewIdeaVM() (newIdeaVM, error) {
	projectIDs, err := s.listProjectIDs()
	if err != nil {
		return newIdeaVM{}, err
	}
	return newIdeaVM{Projects: projectIDs}, nil
}

// editCaptureVM backs the per-capture edit page (GET
// /run/{p}/{s}/edit) for both ideas and intents. Body is the current
// canvas content (seeded into the textarea); a missing file falls back
// to empty and the operator can save into it. Kind is the capture's
// workflow, so the page's chrome says "edit intent" on an intent.
// ErrorBanner is populated on POST validation failure.
type editCaptureVM struct {
	Project     string
	Slug        string
	Kind        string
	Body        string
	ErrorBanner string
}

// handleCaptureEditForm renders the textarea seeded with the capture's
// canvas. 404 when the run is missing, 409 when it isn't an in-progress
// capture — the submit below re-derives both.
func (s *Server) handleCaptureEditForm(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, ok := s.loadRunOr404(w, projectID, slug, "edit form")
	if !ok {
		return
	}
	docID, ok := dash.CaptureDocID(md.Workflow)
	if !ok || md.Status != run.StatusInProgress {
		http.Error(w,
			"run "+id+" is not an editable capture (workflow="+md.Workflow+", status="+md.Status+")",
			http.StatusConflict)
		return
	}

	body, err := os.ReadFile(filepath.Join(s.opts.Root, run.ContentPath(projectID, slug, docID)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logf("edit form: read canvas %s/%s: %v", projectID, slug, err)
		http.Error(w, "edit form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "edit_idea.html", editCaptureVM{
		Project: projectID,
		Slug:    slug,
		Kind:    md.Workflow,
		Body:    string(body),
	})
}

// handleCaptureEditSubmit writes the textarea body to the capture's
// canvas and commits with the trailers the matching CLI edit verb
// produces. CRLF is normalised to LF (mirrors handleNewIdeaSubmit).
// Defends against a replayed POST landing on a now-terminal capture by
// re-checking the gate inside runopen.EditCapture.
func (s *Server) handleCaptureEditSubmit(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")

	err := runopen.EditCapture(s.opts.Root, projectID, slug, body)
	switch {
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, runopen.ErrNotCapture):
		http.Error(w,
			"run "+id+" is not an editable capture",
			http.StatusConflict)
		return
	case errors.Is(err, run.ErrNothingToCommit):
		// No-op edit — body matched on-disk content. Treat as success;
		// the operator wanted to land their text and it's there.
	case err != nil:
		s.renderCaptureEditError(w, r, projectID, slug, body, "edit: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// renderCaptureEditError re-renders the edit page with the operator's
// unsaved body and a banner. It re-loads the run only for the Kind
// label — the happy path never pays for this — and leaves Kind empty
// if the load fails, which the template falls back on.
func (s *Server) renderCaptureEditError(w http.ResponseWriter, r *http.Request, projectID, slug, body, msg string) {
	kind := ""
	if md, err := run.Load(s.opts.Root, projectID, slug); err == nil {
		kind = md.Workflow
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "edit_idea.html", editCaptureVM{
		Project:     projectID,
		Slug:        slug,
		Kind:        kind,
		Body:        body,
		ErrorBanner: msg,
	})
}
