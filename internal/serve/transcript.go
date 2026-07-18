package serve

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/transcript"
)

// pageUnits is the tail window size: how many render units the transcript
// page shows on first load, and the chunk size each "load earlier" pull
// fetches. 200 answers the operator's usual "what just happened" without
// rendering a thousands-event file up front.
const pageUnits = 200

// resultCollapseLines mirrors the text renderer's 40-line elision cutoff
// (transcript's defaultMaxOutputLines): a tool result at or under this
// many lines renders with its <details> open; longer output starts
// collapsed so a big Read or test dump doesn't swamp the page. The web
// page keeps the full output either way and lets <details> do the hiding
// — elision is a terminal-only concern.
const resultCollapseLines = 40

// transcriptVM backs the transcript page and its load-earlier fragment.
type transcriptVM struct {
	Project string
	Slug    string
	Stage   string
	Agent   string
	// OtherAgent is the other backend's thread when it also exists on
	// disk; the header links to it. Empty when this stage has only one.
	OtherAgent string
	// Models are the distinct non-empty models seen across the whole
	// parse (not just the visible window), in first-appearance order, so
	// the header chips are accurate on first load. MultiModel is true when
	// there's more than one — a run resumed under a different model — and
	// gates the per-block model chips that disambiguate which produced what.
	Models     []string
	MultiModel bool
	Units      []unitVM
	// EarlierBefore is the ?before= cursor the load-earlier control points
	// at: the start index of the current window. AtStart is true when that
	// window already begins at unit 0, so the control renders inert.
	EarlierBefore int
	AtStart       bool
	Empty         bool   // file present but parsed to zero render units
	Missing       bool   // no thread file on disk (stale bookmark, or turn not closed yet)
	Path          string // absolute thread path, surfaced in the empty/missing states
	Fragment      bool   // render just the chunk (a load-earlier fetch), no page chrome
}

// unitVM is one render unit: a message, a system event, or a tool call
// with its paired result folded in.
type unitVM struct {
	Kind        string // "user" | "assistant" | "system" | "tool"
	Text        string // body for user/assistant/system
	Model       string // per-event model (assistant blocks)
	ShowModel   bool   // render the per-block model chip (MultiModel && Model != "")
	Tool        string // tool name
	Args        string // tool args summary
	HasResult   bool   // a paired (or orphan) tool result is present
	Result      string // tool output
	ResultError bool   // the tool reported failure
	ResultLines int    // output line count, shown in the <details> summary
	ResultOpen  bool   // <details open> — short output starts expanded
}

// handleTranscript renders a stage's agent transcript at
// GET /run/{project}/{slug}/transcript/{stage}. Read-only GET, same
// safe-mode bucket as the dash and canvas routes. ?agent=claude|codex
// picks the backend; ?before=<index> pages earlier; ?fragment=1 renders
// just the unit blocks for the load-earlier JS to prepend.
//
// Unknown run or stage is a 404; a missing thread file is a 200 empty
// state naming the path (a stale bookmark shouldn't punish the reader),
// matching handleCanvas.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	stage := r.PathValue("stage")

	if s.opts.RunStages == nil {
		http.Error(w, "transcript not configured (Options.RunStages is nil)", http.StatusInternalServerError)
		return
	}
	// RunStages loads the run and its workflow ladder; an unknown run (or
	// unknown workflow) errors, which is the same 404 contract as an
	// unknown stage below — resolution is a lookup, not a file stat.
	stages, err := s.opts.RunStages(projectID, slug)
	if err != nil {
		http.Error(w, "transcript: "+err.Error(), http.StatusNotFound)
		return
	}
	if !slices.Contains(stages, stage) {
		http.Error(w, "transcript: no such stage: "+stage, http.StatusNotFound)
		return
	}

	// Which backends have a thread on the canonical path. A mid-turn
	// session worktree is deliberately not consulted: the page is accurate
	// as of the last closed turn, same posture as `moe <wf> log`.
	claudePath := filepath.Join(s.opts.Root, run.ThreadPathFor("claude", projectID, slug, stage))
	codexPath := filepath.Join(s.opts.Root, run.ThreadPathFor("codex", projectID, slug, stage))
	claudeExists := fileExists(claudePath)
	codexExists := fileExists(codexPath)

	// Agent pick: an explicit ?agent= wins; with none, render the sole
	// thread, or claude when both (or neither) exist. Unlike the CLI's
	// pickLogThread there's no both-present refusal — a page can link to
	// the other backend instead of demanding the operator disambiguate.
	agent := r.URL.Query().Get("agent")
	switch agent {
	case "claude", "codex":
	case "":
		if codexExists && !claudeExists {
			agent = "codex"
		} else {
			agent = "claude"
		}
	default:
		http.Error(w, "transcript: agent must be claude or codex", http.StatusBadRequest)
		return
	}

	path := claudePath
	if agent == "codex" {
		path = codexPath
	}

	vm := transcriptVM{
		Project:  projectID,
		Slug:     slug,
		Stage:    stage,
		Agent:    agent,
		Path:     path,
		Fragment: r.URL.Query().Get("fragment") != "",
	}
	switch {
	case agent == "claude" && codexExists:
		vm.OtherAgent = "codex"
	case agent == "codex" && claudeExists:
		vm.OtherAgent = "claude"
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			vm.Missing = true
			s.render(w, r, "transcript.html", vm)
			return
		}
		s.logf("transcript open %s: %v", path, err)
		http.Error(w, "transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	events, err := transcript.Parse(agent, f)
	if err != nil {
		s.logf("transcript parse %s: %v", path, err)
		http.Error(w, "transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}

	vm.Models = distinctModels(events)
	vm.MultiModel = len(vm.Models) > 1

	units := pairUnits(events)
	if len(units) == 0 {
		vm.Empty = true
		s.render(w, r, "transcript.html", vm)
		return
	}

	// Windowing. Default (no ?before=) opens at the tail: the last
	// pageUnits units. ?before=X selects the pageUnits units ending just
	// before index X. Indices are into the full parse, so a file that
	// grows between requests never shifts an earlier cursor.
	end := len(units)
	if before := r.URL.Query().Get("before"); before != "" {
		n, err := strconv.Atoi(before)
		if err != nil || n < 0 {
			http.Error(w, "transcript: bad before cursor", http.StatusBadRequest)
			return
		}
		end = min(n, len(units))
	}
	start := max(0, end-pageUnits)

	window := units[start:end]
	vm.Units = make([]unitVM, len(window))
	for i, u := range window {
		v := u.view()
		v.ShowModel = vm.MultiModel && v.Model != ""
		vm.Units[i] = v
	}
	vm.EarlierBefore = start
	vm.AtStart = start == 0

	if vm.Fragment {
		s.render(w, r, "transcript_chunk.html", vm)
		return
	}
	s.render(w, r, "transcript.html", vm)
}

// unit is a tool call paired with its adjacent result, or any other
// single event. Pairing before slicing keeps a chunk boundary from ever
// splitting a call from its output.
type unit struct {
	event  transcript.Event
	result *transcript.Event // set only when event is a tool call with an adjacent result
}

// pairUnits folds each tool call and its immediately-following result
// into one unit, matching the text renderer's adjacency pairing (both
// backends emit call-then-result). Everything else is its own unit.
func pairUnits(ev []transcript.Event) []unit {
	units := make([]unit, 0, len(ev))
	for i := 0; i < len(ev); i++ {
		u := unit{event: ev[i]}
		if ev[i].Kind == transcript.KindToolCall &&
			i+1 < len(ev) && ev[i+1].Kind == transcript.KindToolResult {
			r := ev[i+1]
			u.result = &r
			i++
		}
		units = append(units, u)
	}
	return units
}

// view projects a unit into its template shape.
func (u unit) view() unitVM {
	e := u.event
	vm := unitVM{Model: e.Model}
	switch e.Kind {
	case transcript.KindUserText:
		vm.Kind = "user"
		vm.Text = e.Text
	case transcript.KindAssistantText:
		vm.Kind = "assistant"
		vm.Text = e.Text
	case transcript.KindSystem:
		vm.Kind = "system"
		vm.Text = e.Text
	case transcript.KindToolCall:
		vm.Kind = "tool"
		vm.Tool = e.Tool
		vm.Args = e.Args
	case transcript.KindToolResult:
		// An orphan result (no preceding call) — render it as a bare tool
		// block rather than drop it.
		vm.Kind = "tool"
		fillResult(&vm, e)
	}
	if u.result != nil {
		fillResult(&vm, *u.result)
	}
	return vm
}

func fillResult(vm *unitVM, e transcript.Event) {
	vm.HasResult = true
	vm.Result = e.Output
	vm.ResultError = e.Error
	vm.ResultLines = countLines(e.Output)
	vm.ResultOpen = vm.ResultLines <= resultCollapseLines
}

// distinctModels returns the non-empty models in first-appearance order.
func distinctModels(ev []transcript.Event) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range ev {
		if e.Model == "" || seen[e.Model] {
			continue
		}
		seen[e.Model] = true
		out = append(out, e.Model)
	}
	return out
}

// countLines counts the lines in s, ignoring a single trailing newline
// so "a\nb\n" reads as 2, not 3.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// fileExists reports whether path is a regular file (not a directory).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
