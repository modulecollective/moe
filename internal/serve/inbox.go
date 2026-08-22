// Package serve — the human-input inbox.
//
// Two surfaces: /inbox, the board-wide queue of questions runs are
// waiting on, and one answer POST per run. Both are journal-only. The
// answer writes a commit and returns; the heartbeat is what turns that
// commit back into motion, exactly as it does for the advance mark.
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

// inboxVM is the /inbox page: one entry per open question, oldest first.
type inboxVM struct {
	Serve servePanelVM
	// Project scopes the page when the operator arrived from a project
	// hub; empty is the board-wide view.
	Project string
	Entries []inboxEntryVM
}

// inboxEntryVM is one open question with its run link and one button per
// choice. RequestID rides every button so a phone tab that has been
// sitting on this page since the question was answered and replaced
// cannot answer the new one by accident.
type inboxEntryVM struct {
	ID        string
	Project   string
	Slug      string
	URL       string
	Question  string
	RequestID int
	Choices   []inboxChoiceVM
}

type inboxChoiceVM struct {
	Number int
	Text   string
}

func newInboxEntry(p input.Pending) inboxEntryVM {
	e := inboxEntryVM{
		ID:        p.Project + "/" + p.Run,
		Project:   p.Project,
		Slug:      p.Run,
		URL:       "/run/" + p.Project + "/" + p.Run,
		Question:  p.Request.Question,
		RequestID: p.Request.ID,
	}
	for i, c := range p.Request.Choices {
		e.Choices = append(e.Choices, inboxChoiceVM{Number: i + 1, Text: c})
	}
	return e
}

// handleInbox renders every open question on the board. Read-only, so it
// serves on an unarmed serve like the rest of the browse routes.
//
// A malformed record degrades to a missing row and a log line rather
// than a 500: the operator came here to answer the other questions.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	// ?project= scopes the page the way the hub's link means it: the
	// count on a project page has to name what that page's link opens.
	projectID := r.URL.Query().Get("project")
	pending, errs := input.Scan(s.opts.Root, projectID)
	for _, err := range errs {
		s.logf("inbox: %v", err)
	}
	vm := inboxVM{Serve: s.activity.panel(time.Now().UTC()), Project: projectID}
	for _, p := range pending {
		vm.Entries = append(vm.Entries, newInboxEntry(p))
	}
	s.render(w, r, "inbox.html", vm)
}

// inboxCount is how many questions are open, for the dash and hub links
// that render only when there is something to link to. A count, not a
// permanent navigation landmark: the inbox should be invisible on a
// board with nothing waiting.
//
// Errors are logged and counted as zero — a link is not the place to
// surface a broken record, and the floors that refuse work already do.
func (s *Server) inboxCount(projectID string) int {
	pending, errs := input.Scan(s.opts.Root, projectID)
	for _, err := range errs {
		s.logf("inbox count: %v", err)
	}
	return len(pending)
}

// handleInputAnswer records the operator's choice on a run's open
// question. Journal-only — one commit, no agent, no child — which is why
// it carries no dynamic-mode gate, same as the advance mark.
//
// The POST carries the request id the page rendered and the answer route
// re-derives everything else. A stale id is 409, not a silent answer to
// whatever happens to be open now: the phone is the surface most likely
// to be looking at yesterday's question.
func (s *Server) handleInputAnswer(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	requestID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("request")))
	if err != nil {
		http.Error(w, "answer: request id is not a number", http.StatusBadRequest)
		return
	}
	choice, err := strconv.Atoi(strings.TrimSpace(r.FormValue("choice")))
	if err != nil {
		http.Error(w, "answer: choice is not a number", http.StatusBadRequest)
		return
	}
	switch _, err := input.Answer(s.opts.Root, projectID, slug, requestID, choice, s.syncWriter(), s.syncWriter()); {
	case err == nil:
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, input.ErrStaleRequest), errors.Is(err, input.ErrNoOpenRequest), errors.Is(err, input.ErrNotLive):
		http.Error(w, "answer: "+err.Error(), http.StatusConflict)
		return
	case errors.Is(err, input.ErrChoiceOutOfRange):
		http.Error(w, "answer: "+err.Error(), http.StatusBadRequest)
		return
	default:
		s.logf("answer %s: %v", id, err)
		http.Error(w, "answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Back where the operator was. The inbox is the phone surface, and
	// answering from it should leave them looking at what's left rather
	// than on a run page they never opened. Only the path is honoured, and
	// only when it is the inbox — a Referer is attacker-controllable, so
	// it picks between two known destinations here rather than becoming
	// one.
	if u, err := url.Parse(r.Referer()); err == nil && u.Path == "/inbox" {
		http.Redirect(w, r, "/inbox?"+u.RawQuery, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// runInputsVM is a run page's input history: every question the run has
// been asked, the answers it got, and the choice buttons for the one
// still open.
type runInputsVM struct {
	Entries []runInputVM
	// Open, when non-nil, is the entry the page renders buttons for.
	Open *inboxEntryVM
}

type runInputVM struct {
	Question string
	Answer   string
}

// gatherRunInputs reads a run's input history for its page. A run with
// no record — nearly all of them — renders no section. Terminal runs
// keep their history: the questions and answers are part of how the run
// went, even though it has dropped out of the inbox.
func (s *Server) gatherRunInputs(projectID, slug string, md *run.Metadata) runInputsVM {
	f, err := input.Load(s.opts.Root, projectID, slug)
	if err != nil {
		s.logf("run inputs %s/%s: %v", projectID, slug, err)
		return runInputsVM{}
	}
	var vm runInputsVM
	for _, req := range f.Requests {
		vm.Entries = append(vm.Entries, runInputVM{Question: req.Question, Answer: req.Answer()})
		if req.Answered() || md.Status != run.StatusInProgress {
			continue
		}
		e := newInboxEntry(input.Pending{Project: projectID, Run: slug, Request: req})
		vm.Open = &e
	}
	return vm
}
