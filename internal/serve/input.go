// Package serve — the operator's input channel to a run.
//
// Two surfaces: /input, the board-wide queue of questions runs have
// asked and notes they are still carrying, and one POST per run that
// takes either a fresh note or a reply to that run's open question.
// Both are journal-only. The write lands one commit and returns; the
// heartbeat is what turns that commit back into motion, exactly as it
// does for the advance mark.
//
// internal/input is imported directly rather than crossing the seam as
// an Options callback, the same way runopen is: neither package needs
// the workflow registry, and a callback would only be indirection.
package serve

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

// inputQueueVM is the /input page: the questions needing the operator,
// each with its own reply box, then what has already been given and is
// waiting on a turn.
type inputQueueVM struct {
	Serve servePanelVM
	// Project scopes the page when the operator arrived from a project
	// hub; empty is the board-wide view.
	Project string
	Asked   []openPingVM
	Given   []pendingVM
}

// openPingVM is one unanswered question with its run link and a reply
// box. PingID rides the form so a phone tab that has been sitting on
// this page since the question was answered and replaced cannot answer
// the new one by accident.
type openPingVM struct {
	ID       string
	Project  string
	Slug     string
	URL      string
	Question string
	PingID   int
}

// pendingVM is one entry the operator has already given, listed
// read-only: it needs a turn, not a human.
type pendingVM struct {
	ID   string
	URL  string
	Line string
}

func newOpenPing(projectID, slug string, e input.Entry) openPingVM {
	return openPingVM{
		ID:       projectID + "/" + slug,
		Project:  projectID,
		Slug:     slug,
		URL:      "/run/" + projectID + "/" + slug,
		Question: e.Question,
		PingID:   e.ID,
	}
}

// handleInputQueue renders what the board is carrying: questions first,
// because those are the ones that need the operator, then the notes
// already in flight so one page answers "what needs me, and what's
// already moving." Read-only in the GET sense, so it serves on an
// unarmed serve like the rest of the browse routes.
//
// A malformed record degrades to a missing row and a log line rather
// than a 500: the operator came here to answer the other questions.
func (s *Server) handleInputQueue(w http.ResponseWriter, r *http.Request) {
	// ?project= scopes the page the way the hub's link means it: the
	// count on a project page has to name what that page's link opens.
	projectID := r.URL.Query().Get("project")
	waiting, errs := input.Scan(s.opts.Root, projectID)
	for _, err := range errs {
		s.logf("input: %v", err)
	}
	vm := inputQueueVM{Serve: s.activity.panel(time.Now().UTC()), Project: projectID}
	for _, wt := range waiting {
		if wt.Entry.Open() {
			vm.Asked = append(vm.Asked, newOpenPing(wt.Project, wt.Run, wt.Entry))
			continue
		}
		vm.Given = append(vm.Given, pendingVM{
			ID:   wt.Project + "/" + wt.Run,
			URL:  "/run/" + wt.Project + "/" + wt.Run,
			Line: wt.Entry.FirstLine(),
		})
	}
	s.render(w, r, "input.html", vm)
}

// openPingCount is how many questions are waiting on the operator, for
// the dash and hub links that render only when there is something to
// link to. A count, not a permanent navigation landmark: the queue
// should be invisible on a board with nothing asking.
//
// Counts questions only, not pending notes. The link's job is "something
// needs you"; a note already given needs a turn, not a tap.
//
// Errors are logged and counted as zero — a link is not the place to
// surface a broken record.
func (s *Server) openPingCount(projectID string) int {
	waiting, errs := input.Scan(s.opts.Root, projectID)
	for _, err := range errs {
		s.logf("input count: %v", err)
	}
	n := 0
	for _, wt := range waiting {
		if wt.Entry.Open() {
			n++
		}
	}
	return n
}

// handleInput takes the operator's prose for a run: a reply when the
// form carries the id of the question it was rendered against, a fresh
// note when it doesn't. Journal-only — one commit, no agent, no child —
// which is why it carries no dynamic-mode gate, same as the advance
// mark.
//
// A reply naming a stale id is 409, not a silent answer to whatever
// happens to be open now: the phone is the surface most likely to be
// looking at yesterday's question.
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	// Browsers send \r\n in textarea bodies; the record lives on disk as
	// LF, same normalisation every other prose form here does.
	text := strings.ReplaceAll(r.FormValue("text"), "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		http.Error(w, "input: nothing to write", http.StatusBadRequest)
		return
	}

	var err error
	if raw := strings.TrimSpace(r.FormValue("ping")); raw != "" {
		pingID, convErr := strconv.Atoi(raw)
		if convErr != nil {
			http.Error(w, "input: ping id is not a number", http.StatusBadRequest)
			return
		}
		_, err = input.Answer(s.opts.Root, projectID, slug, pingID, text, s.syncWriter(), s.syncWriter())
	} else {
		_, err = input.Add(s.opts.Root, projectID, slug, text, s.syncWriter(), s.syncWriter())
	}
	switch {
	case err == nil:
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, input.ErrStalePing), errors.Is(err, input.ErrNoOpenPing), errors.Is(err, input.ErrNotLive):
		http.Error(w, "input: "+err.Error(), http.StatusConflict)
		return
	case errors.Is(err, input.ErrEmpty):
		http.Error(w, "input: "+err.Error(), http.StatusBadRequest)
		return
	default:
		s.logf("input %s: %v", id, err)
		http.Error(w, "input: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Back where the operator was. The queue is the phone surface, and
	// answering from it should leave them looking at what's left rather
	// than on a run page they never opened. Only the path is honoured, and
	// only when it is the queue — a Referer is attacker-controllable, so
	// it picks between two known destinations here rather than becoming
	// one.
	if u, err := url.Parse(r.Referer()); err == nil && u.Path == "/input" {
		http.Redirect(w, r, "/input?"+u.RawQuery, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// runInputsVM is a run page's input section: the full history with
// delivery markers, the open question's reply box, and — while the run
// is live — a box for an unprompted note.
type runInputsVM struct {
	Entries []runInputVM
	// Open, when non-nil, is the question the page renders a reply box
	// for.
	Open *openPingVM
	// Live gates the fresh-note box. A terminal run keeps its history and
	// loses the form: there is no next turn to deliver to.
	Live bool
	// PostURL is where both boxes submit.
	PostURL string
}

// Any reports whether the section has anything to render at all.
func (v runInputsVM) Any() bool { return len(v.Entries) > 0 || v.Live }

type runInputVM struct {
	// Question is empty on an operator note.
	Question string
	Text     string
	// State is the entry's one-word status for the badge: "unanswered",
	// "pending", or the doc that consumed it.
	State string
}

// gatherRunInputs reads a run's input history for its page. A run with
// no record and no next turn — nearly all of them — renders no section.
// Terminal runs keep their history: what the operator said is part of
// how the run went, even though nothing more can be written.
func (s *Server) gatherRunInputs(projectID, slug string, md *run.Metadata) runInputsVM {
	vm := runInputsVM{
		Live:    md.Status == run.StatusInProgress,
		PostURL: "/run/" + projectID + "/" + slug + "/input",
	}
	f, err := input.Load(s.opts.Root, projectID, slug)
	if err != nil {
		s.logf("run inputs %s/%s: %v", projectID, slug, err)
		return vm
	}
	for _, e := range f.Notes {
		vm.Entries = append(vm.Entries, runInputVM{
			Question: e.Question,
			Text:     e.Text,
			State:    entryState(e),
		})
		if e.Open() && vm.Live {
			p := newOpenPing(projectID, slug, e)
			vm.Open = &p
		}
	}
	return vm
}

func entryState(e input.Entry) string {
	switch {
	case e.Open():
		return "unanswered"
	case e.Delivered():
		return "read at " + e.DeliveredTo
	default:
		return "pending"
	}
}
