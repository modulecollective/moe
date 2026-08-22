// Package input owns the run-local human-input record: one durable,
// run-linked question the operator answers from the CLI or the phone,
// whose unanswered state holds the work and whose answer reaches both
// the next pulse and the run's later agent turns.
//
// The record is projects/<project>/runs/<run>/inputs.json, sibling of
// run.json. It exists only for runs that have been asked something, so
// nothing pays for the feature until it is used.
//
// The state machine is deliberately small. A request has a question and
// two or three fixed choices; `selected` is the one-based choice number,
// and zero (omitted on disk) means unanswered. A run may carry at most
// one unanswered request at a time — a later pulse may ask the next
// question once the first is answered, and must not overwrite or
// duplicate an open one. Answered requests stay in the file rather than
// only in git, so every later stage reads the decision without
// reconstructing it from commits.
//
// Ask and Answer each write one journal commit through the shared
// lock/pull/push pipeline, stamped MoE-Input-Asked / MoE-Input-Answered.
// Neither starts an agent: answering moves the journal, and the next
// heartbeat sweep is what picks the work back up.
package input

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// FileName is the per-run record's basename, sibling of run.json.
const FileName = "inputs.json"

// Choice-count bounds. Two is the smallest question worth asking; three
// is where a fixed-choice prompt stops being answerable at a glance on a
// phone. A question that wants more than three answers is a question the
// survey hasn't finished narrowing.
const (
	MinChoices = 2
	MaxChoices = 3
)

var (
	// ErrMalformed reports an inputs.json that doesn't satisfy the
	// invariants below. Read as a refusal, never repaired in place: the
	// file is machine-written, so a violation is a bug to see, not
	// noise to route around.
	ErrMalformed = errors.New("input: malformed inputs.json")
	// ErrOpenRequest reports an ask on a run that already has an
	// unanswered request. One open question per run is the whole reason
	// `answer <run> <number>` needs no question id.
	ErrOpenRequest = errors.New("input: run already has an unanswered request")
	// ErrNoOpenRequest reports an answer aimed at a run with nothing open.
	ErrNoOpenRequest = errors.New("input: run has no unanswered request")
	// ErrStaleRequest reports an answer naming a request id that isn't
	// the open one — a phone tab that has been sitting on a question
	// since answered and replaced.
	ErrStaleRequest = errors.New("input: request id is not the open request")
	// ErrChoiceOutOfRange reports a choice number outside 1..len(choices).
	ErrChoiceOutOfRange = errors.New("input: choice out of range")
	// ErrBadQuestion reports a question the grammar refuses: empty text,
	// the wrong number of choices, or an empty or duplicated choice.
	ErrBadQuestion = errors.New("input: malformed question")
	// ErrNotLive reports an ask or answer against a run that is not in
	// progress. A terminal run drops out of the inbox, so a question on
	// one could never be discharged.
	ErrNotLive = errors.New("input: run is not in progress")
)

// Request is one question asked of the operator on one run.
type Request struct {
	// ID is one-based and dense within the file: request n is at index
	// n-1. Ids are per-run, so the answer address is <project>/<run>#<id>.
	ID int `json:"id"`
	// AskedBy is the qualified "<project>/<run>" of the pulse that asked.
	AskedBy string `json:"asked_by"`
	// Question is the one sentence that changes what happens next.
	Question string `json:"question"`
	// Choices are the two or three distinct answers. Order is the
	// numbering the operator answers with.
	Choices []string `json:"choices"`
	// Selected is the one-based chosen choice, or zero for unanswered.
	// Zero elides on disk, so an open request's JSON says nothing about
	// an answer rather than saying "0".
	Selected int `json:"selected,omitempty"`
}

// Answered reports whether the operator has picked a choice.
func (r Request) Answered() bool { return r.Selected != 0 }

// Answer returns the selected choice's text, or "" when unanswered.
func (r Request) Answer() string {
	if r.Selected < 1 || r.Selected > len(r.Choices) {
		return ""
	}
	return r.Choices[r.Selected-1]
}

// File is the on-disk shape of inputs.json.
type File struct {
	Requests []Request `json:"requests"`
}

// Open returns the run's single unanswered request, if it has one.
func (f File) Open() (Request, bool) {
	for _, req := range f.Requests {
		if !req.Answered() {
			return req, true
		}
	}
	return Request{}, false
}

// Path returns the record's path relative to the bureaucracy root.
func Path(projectID, runID string) string {
	return filepath.Join(run.Dir(projectID, runID), FileName)
}

// Load reads a run's input history. A run with no record reads as an
// empty File and no error — the common case, and the one every caller
// walks past silently. A record that exists but violates the invariants
// is ErrMalformed: it is machine-written, so a violation is a bug.
func Load(root, projectID, runID string) (File, error) {
	body, err := os.ReadFile(filepath.Join(root, Path(projectID, runID)))
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("input: read %s/%s: %w", projectID, runID, err)
	}
	var f File
	if err := json.Unmarshal(body, &f); err != nil {
		return File{}, fmt.Errorf("%w: %s/%s: %v", ErrMalformed, projectID, runID, err)
	}
	if err := f.validate(); err != nil {
		return File{}, fmt.Errorf("%w: %s/%s: %v", ErrMalformed, projectID, runID, err)
	}
	return f, nil
}

// validate enforces the file's invariants: dense one-based ids, a
// well-formed question per entry, an in-range selection, and at most one
// unanswered request.
func (f File) validate() error {
	open := 0
	for i, req := range f.Requests {
		if req.ID != i+1 {
			return fmt.Errorf("request %d has id %d", i+1, req.ID)
		}
		if err := ValidateQuestion(req.Question, req.Choices); err != nil {
			return fmt.Errorf("request %d: %v", req.ID, err)
		}
		if req.Selected < 0 || req.Selected > len(req.Choices) {
			return fmt.Errorf("request %d: selected %d out of range", req.ID, req.Selected)
		}
		if !req.Answered() {
			open++
		}
	}
	if open > 1 {
		return fmt.Errorf("%d unanswered requests; at most one is allowed", open)
	}
	return nil
}

// ValidateQuestion checks one question against the v1 grammar: non-empty
// text, MinChoices..MaxChoices choices, each non-empty and distinct.
// Exported because the pulse gate validates a proposed question before
// it reaches disk, and the two must agree by construction.
func ValidateQuestion(question string, choices []string) error {
	if strings.TrimSpace(question) == "" {
		return fmt.Errorf("%w: question is empty", ErrBadQuestion)
	}
	if len(choices) < MinChoices || len(choices) > MaxChoices {
		return fmt.Errorf("%w: %d choices; want %d to %d", ErrBadQuestion, len(choices), MinChoices, MaxChoices)
	}
	seen := make(map[string]bool, len(choices))
	for i, c := range choices {
		t := strings.TrimSpace(c)
		if t == "" {
			return fmt.Errorf("%w: choice %d is empty", ErrBadQuestion, i+1)
		}
		if seen[t] {
			return fmt.Errorf("%w: choice %d repeats %q", ErrBadQuestion, i+1, t)
		}
		seen[t] = true
	}
	return nil
}

// Ask appends one question to a run's record and commits it. askedBy is
// the qualified "<project>/<run>" of the asking pulse; consent is the
// MoE-Consent value of the walk it made the ask under (a pulse always
// has one — the ask is a machine act).
//
// Refuses a run that isn't in progress (ErrNotLive), a question the
// grammar rejects (ErrBadQuestion), and a run that already has something
// open (ErrOpenRequest). All three are the caller's cue to warn and
// carry on rather than fail the sweep.
func Ask(root, projectID, runID, askedBy, question string, choices []string, consent string, stdout, stderr io.Writer) (Request, error) {
	if err := ValidateQuestion(question, choices); err != nil {
		return Request{}, err
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return Request{}, err
	}
	if md.Status != run.StatusInProgress {
		return Request{}, fmt.Errorf("%w: %s/%s is %s", ErrNotLive, projectID, runID, md.Status)
	}
	f, err := Load(root, projectID, runID)
	if err != nil {
		return Request{}, err
	}
	if open, ok := f.Open(); ok {
		return Request{}, fmt.Errorf("%w: %s/%s#%d — %s", ErrOpenRequest, projectID, runID, open.ID, open.Question)
	}
	req := Request{
		ID:       len(f.Requests) + 1,
		AskedBy:  askedBy,
		Question: strings.TrimSpace(question),
		Choices:  trimAll(choices),
	}
	f.Requests = append(f.Requests, req)

	ref := projectID + "/" + runID
	msg := fmt.Sprintf("input: ask %s#%d\n\n", ref, req.ID) +
		trailers.Block{
			Run:        runID,
			Project:    projectID,
			Workflow:   md.Workflow,
			Consent:    consent,
			InputAsked: fmt.Sprintf("%s#%d", ref, req.ID),
		}.String()
	if err := commit(root, projectID, runID, f, "input-ask", msg, stdout, stderr); err != nil {
		return Request{}, err
	}
	return req, nil
}

// Answer records the operator's choice on a run's open request and
// commits it. id is the request the caller believes is open; zero means
// "whatever is open" — the CLI passes zero because v1 permits only one,
// and the web passes the rendered id so a stale phone tab cannot answer
// a newer question.
//
// No consent trailer: the answer is the operator's own act, which is the
// whole point of the inbox.
func Answer(root, projectID, runID string, id, choice int, stdout, stderr io.Writer) (Request, error) {
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return Request{}, err
	}
	if md.Status != run.StatusInProgress {
		return Request{}, fmt.Errorf("%w: %s/%s is %s", ErrNotLive, projectID, runID, md.Status)
	}
	f, err := Load(root, projectID, runID)
	if err != nil {
		return Request{}, err
	}
	open, ok := f.Open()
	if !ok {
		return Request{}, fmt.Errorf("%w: %s/%s", ErrNoOpenRequest, projectID, runID)
	}
	if id != 0 && id != open.ID {
		return Request{}, fmt.Errorf("%w: %s/%s has #%d open, not #%d", ErrStaleRequest, projectID, runID, open.ID, id)
	}
	if choice < 1 || choice > len(open.Choices) {
		return Request{}, fmt.Errorf("%w: %d is not 1..%d", ErrChoiceOutOfRange, choice, len(open.Choices))
	}
	f.Requests[open.ID-1].Selected = choice
	answered := f.Requests[open.ID-1]

	ref := projectID + "/" + runID
	msg := fmt.Sprintf("input: answer %s#%d — %s\n\n", ref, answered.ID, answered.Answer()) +
		trailers.Block{
			Run:           runID,
			Project:       projectID,
			Workflow:      md.Workflow,
			InputAnswered: fmt.Sprintf("%s#%d", ref, answered.ID),
		}.String()
	if err := commit(root, projectID, runID, f, "input-answer", msg, stdout, stderr); err != nil {
		return Request{}, err
	}
	return answered, nil
}

// commit writes the record and lands it as one journal commit through
// the shared lock/pull/push pipeline.
func commit(root, projectID, runID string, f File, purpose, msg string, stdout, stderr io.Writer) error {
	rel := Path(projectID, runID)
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: purpose,
		Run:     projectID + "/" + runID,
	}, stdout, stderr, func() error {
		if err := writeFile(filepath.Join(root, rel), f); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, rel)
	})
}

func writeFile(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func trimAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.TrimSpace(x)
	}
	return out
}

// Pending is one open request as the inbox lists it.
type Pending struct {
	Project string
	Run     string
	Request Request
	// Asked is when the record last moved, which for a run with an open
	// request is the ask commit that opened it — a later answer would
	// have closed it. Zero when the record isn't in git yet.
	Asked time.Time
}

// Ref renders the answer address the CLI and the web both print.
func (p Pending) Ref() string {
	return p.Project + "/" + p.Run + "#" + strconv.Itoa(p.Request.ID)
}

// Scan returns every open request on the board, oldest first. Terminal
// runs drop out: their history stays visible on the run page, but there
// is nothing left to discharge, so they are not the operator's queue.
//
// projectFilter scopes to one project when non-empty.
//
// A run whose record is malformed is skipped with its error collected —
// one bad file degrades to one missing row rather than an empty inbox.
func Scan(root, projectFilter string) ([]Pending, []error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, []error{err}
	}
	var out []Pending
	var errs []error
	for _, md := range mds {
		if md.Status != run.StatusInProgress {
			continue
		}
		if projectFilter != "" && md.Project != projectFilter {
			continue
		}
		f, err := Load(root, md.Project, md.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		req, ok := f.Open()
		if !ok {
			continue
		}
		// Best-effort: a record not yet in git (an ask mid-flight, a
		// hand-made fixture) sorts as zero rather than dropping the row.
		asked, _ := run.LastFileActivity(root, Path(md.Project, md.ID))
		out = append(out, Pending{Project: md.Project, Run: md.ID, Request: req, Asked: asked})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Asked.Equal(out[j].Asked) {
			return out[i].Asked.Before(out[j].Asked)
		}
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Run < out[j].Run
	})
	return out, errs
}
