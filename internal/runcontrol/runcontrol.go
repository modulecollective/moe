// Package runcontrol owns a run's operator control record: requests
// about *how a run continues* — hold it, hand it to a human, have its
// current owner yield, finish it — and what became of each.
//
// The record is projects/<project>/runs/<run>/control.json, sibling of
// inputs.json. It is structured operator intent plus its receipts, not
// another authority for run status (trailers), ownership (the runowner
// flock) or liveness (the owner's endpoint). Prose stays in inputs.json:
// a note never holds anything and a hold never needs prose.
//
// Kinds:
//
//   - pause — hold the run: the owner settles work already begun, then
//     yields before any further workflow work. The hold is committed at
//     acceptance, so it closes admission at once and survives sweeps,
//     disconnects and restarts until Resume clears it.
//   - handoff — the same hold, labelled as handing the run to a human,
//     with the stage/backend whose conversation the human picks up.
//   - park — ask the current owner to settle and yield once. Nothing is
//     held; later pickup follows ordinary eligibility.
//   - finish — at the next boundary, continue this run with `!!`
//     authority through the existing gates and ship route, then end its
//     chain ride. Authorises only this run.
//   - resume — recorded by Resume, never accepted through Submit:
//     releasing a hold is an act of a guard holder, not a queued intent.
//
// A request moves accepted → applied → completed|failed. Applied means
// an owner acted on it at a boundary; for finish it is recorded before
// the continuation dispatches, so a crash after it is an uncertain
// outcome to reconcile, never a grant to replay.
//
// Every write is one journal commit under the repository lock, reading
// and rewriting the record inside the lock. A commit that fails restores
// the file and index as they were: an unacknowledged request must never
// become an invisible grant. A write with nothing to change makes no
// commit.
package runcontrol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

// FileName is the per-run record's basename, sibling of run.json.
const FileName = "control.json"

// Kind is what a request asks of the run.
type Kind string

const (
	KindPause   Kind = "pause"
	KindHandoff Kind = "handoff"
	KindPark    Kind = "park"
	KindFinish  Kind = "finish"
	KindResume  Kind = "resume"
)

// State is where a request is in its life.
type State string

const (
	StateAccepted  State = "accepted"
	StateApplied   State = "applied"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
)

var (
	// ErrMalformed reports a control.json that violates its invariants.
	// Callers that gate motion on the record refuse motion on it; it
	// never reads as "no hold".
	ErrMalformed = errors.New("runcontrol: malformed control.json")
	// ErrNotLive reports a request against a terminal run.
	ErrNotLive = errors.New("runcontrol: run is not live")
	// ErrBadKind reports a kind Submit does not accept.
	ErrBadKind = errors.New("runcontrol: unknown request kind")
	// ErrConflict reports a reused request id carrying different intent.
	ErrConflict = errors.New("runcontrol: request id already used for a different request")
	// ErrHeld refuses a finish while a hold is in force: the hold wins,
	// and resuming is the operator's explicit step first.
	ErrHeld = errors.New("runcontrol: run is held")
	// ErrRecovery refuses a finish while an earlier continuation's
	// outcome is unknown.
	ErrRecovery = errors.New("runcontrol: run needs recovery")
	// ErrNoOwner refuses a park, or a finish on a pushed run, when no
	// invocation owns the run: there is nothing to yield, and a pushed
	// run with no repair in flight has already shipped.
	ErrNoOwner = errors.New("runcontrol: no live owner")
	// ErrNotOwner reports a Resume without a live guard on the run.
	ErrNotOwner = errors.New("runcontrol: resume needs the run's owner guard")
	// ErrStale refuses a Resume rendered against an older hold.
	ErrStale = errors.New("runcontrol: hold has changed since it was shown")
)

// Request is one control request and its outcome.
type Request struct {
	// ID is caller-chosen and stable: a client that lost the
	// acknowledgement retries with the same id and reads the recorded
	// request back instead of acting twice.
	ID         string    `json:"id"`
	Kind       Kind      `json:"kind"`
	AcceptedAt time.Time `json:"accepted_at"`
	// Owner binds a park to the owner generation the requester saw, so
	// it retires rather than parking a successor. Submit refuses a new
	// park without one.
	Owner string `json:"owner,omitempty"`
	// Revision is the hold revision a resume was rendered against; zero
	// accepts whatever hold is current.
	Revision int `json:"revision,omitempty"`
	// Stage and Agent are a handoff's conversation address: the last
	// stage that actually ran and the backend that ran it.
	Stage string `json:"stage,omitempty"`
	Agent string `json:"agent,omitempty"`
	State State  `json:"state"`
	// AppliedStatus is the run's status when a finish was applied. A
	// run that was already pushed then (a CI repair) cannot prove by
	// its status alone that this finish shipped anything.
	AppliedStatus string `json:"applied_status,omitempty"`
	// Boundary names where an owner acted on the request ("stage code",
	// "push", "chain ride", …).
	Boundary string `json:"boundary,omitempty"`
	// Outcome is one human line describing what actually happened.
	Outcome string `json:"outcome,omitempty"`
}

// Settled reports whether nothing more will happen to r.
func (r Request) Settled() bool { return r.State == StateCompleted || r.State == StateFailed }

// ShippedBy reports whether a run now at status has shipped since the
// finish r was applied: it was in progress then and is pushed or merged
// now. A finish applied to an already-pushed run (a CI repair) has no
// such evidence, so this says no.
func (r Request) ShippedBy(status string) bool {
	return r.AppliedStatus == run.StatusInProgress &&
		(status == run.StatusPushed || status == run.StatusMerged)
}

// sameIntent reports whether b, offered under a's id, asks for what a
// asked for. State and receipts are a's own history, not intent. A
// retry that names no owner generation (it no longer knows which owner
// it addressed) reads back the recorded park.
func (a Request) sameIntent(b Request) bool {
	return a.Kind == b.Kind && (a.Owner == b.Owner || b.Owner == "") && a.Revision == b.Revision
}

// Hold is a pause or handoff in force.
type Hold struct {
	// Request is the id of the pause/handoff that set it.
	Request string `json:"request"`
	Kind    Kind   `json:"kind"`
	// Stage and Agent are the conversation address saved with it.
	Stage string `json:"stage,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// Recovery marks a continuation whose outcome could not be
// established. It blocks automatic continuation like a hold, until an
// operator Resume retires it.
type Recovery struct {
	Request string `json:"request"`
	Reason  string `json:"reason"`
}

// File is the on-disk shape of control.json.
type File struct {
	// Revision counts changes to Hold and Recovery. A resume carries the
	// revision it was shown, so it cannot clear a newer hold.
	Revision int       `json:"revision"`
	Hold     *Hold     `json:"hold,omitempty"`
	Recovery *Recovery `json:"recovery,omitempty"`
	Requests []Request `json:"requests"`
}

// Held reports whether a pause or handoff is in force.
func (f File) Held() bool { return f.Hold != nil }

// Find returns the request with id.
func (f File) Find(id string) (Request, bool) {
	for _, r := range f.Requests {
		if r.ID == id {
			return r, true
		}
	}
	return Request{}, false
}

// Open returns the unsettled requests of the given kinds, oldest first.
func (f File) Open(kinds ...Kind) []Request {
	var out []Request
	for _, r := range f.Requests {
		if r.Settled() {
			continue
		}
		if slices.Contains(kinds, r.Kind) {
			out = append(out, r)
		}
	}
	return out
}

func (f *File) find(id string) *Request {
	for i := range f.Requests {
		if f.Requests[i].ID == id {
			return &f.Requests[i]
		}
	}
	return nil
}

// Path returns the record's path relative to the bureaucracy root.
func Path(projectID, runID string) string {
	return filepath.Join(run.Dir(projectID, runID), FileName)
}

// Load reads a run's control record. No record reads as an empty File.
func Load(root, projectID, runID string) (File, error) {
	body, err := os.ReadFile(filepath.Join(root, Path(projectID, runID)))
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("runcontrol: read %s/%s: %w", projectID, runID, err)
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

func (f File) validate() error {
	seen := map[string]bool{}
	for i, r := range f.Requests {
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("request %d has no id", i+1)
		}
		if seen[r.ID] {
			return fmt.Errorf("request id %q repeats", r.ID)
		}
		seen[r.ID] = true
		switch r.Kind {
		case KindPause, KindHandoff, KindPark, KindFinish, KindResume:
		default:
			return fmt.Errorf("request %q has kind %q", r.ID, r.Kind)
		}
		switch r.State {
		case StateAccepted, StateApplied, StateCompleted, StateFailed:
		default:
			return fmt.Errorf("request %q has state %q", r.ID, r.State)
		}
	}
	if f.Revision < 0 {
		return fmt.Errorf("revision %d is negative", f.Revision)
	}
	if f.Hold != nil {
		r, ok := f.Find(f.Hold.Request)
		if !ok || (r.Kind != KindPause && r.Kind != KindHandoff) || r.Kind != f.Hold.Kind {
			return fmt.Errorf("hold names %q, which is not its pause/handoff request", f.Hold.Request)
		}
	}
	if f.Recovery != nil {
		if _, ok := f.Find(f.Recovery.Request); !ok {
			return fmt.Errorf("recovery names unknown request %q", f.Recovery.Request)
		}
	}
	return nil
}

// NewID mints a request id for callers that have none of their own.
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "c-" + hex.EncodeToString(b[:])
}

// now is the acceptance clock; tests pin it.
var now = func() time.Time { return time.Now().UTC().Truncate(time.Second) }

// Submit records a pause, handoff, park or finish and returns the
// request as stored. Workflow eligibility (which workflows take
// controls at all) is the caller's policy; Submit enforces the rules
// that depend on the record and the run.
//
// A repeated id returns the recorded request untouched — even once the
// run has gone terminal — and ErrConflict if its intent differs, so a
// retry after a lost acknowledgement never acts twice.
//
// A pause or handoff while a hold is already in force is satisfied by
// that hold: Submit returns the hold's own request and commits nothing.
// Otherwise the hold is set here, before any owner reaches a boundary,
// and any finish not yet applied is superseded. With no owner running
// there is no turn to settle, so the request completes at once.
//
// A repeated finish while one is still open returns that one.
func Submit(root, projectID, runID string, req Request) (Request, error) {
	switch req.Kind {
	case KindPause, KindHandoff, KindPark, KindFinish:
	default:
		return Request{}, fmt.Errorf("%w: %q", ErrBadKind, req.Kind)
	}
	if req.ID == "" {
		req.ID = NewID()
	}
	var out Request
	err := commit(root, projectID, runID, "control-"+string(req.Kind), func(f *File) (string, error) {
		if prev, ok := f.Find(req.ID); ok {
			if !prev.sameIntent(req) {
				return "", fmt.Errorf("%w: %s is a %s", ErrConflict, req.ID, prev.Kind)
			}
			out = prev
			return "", run.ErrNothingToCommit
		}
		md, err := run.Load(root, projectID, runID)
		if err != nil {
			return "", err
		}
		if md.Status != run.StatusInProgress && md.Status != run.StatusPushed {
			return "", fmt.Errorf("%w: %s/%s is %s", ErrNotLive, projectID, runID, md.Status)
		}
		busy, err := runowner.Busy(root, projectID, runID)
		if err != nil {
			return "", err
		}
		r := Request{ID: req.ID, Kind: req.Kind, AcceptedAt: now(), Owner: req.Owner,
			Stage: req.Stage, Agent: req.Agent, State: StateAccepted}
		switch req.Kind {
		case KindPause, KindHandoff:
			if f.Held() {
				out, _ = f.Find(f.Hold.Request)
				return "", run.ErrNothingToCommit
			}
			f.Hold = &Hold{Request: r.ID, Kind: r.Kind, Stage: r.Stage, Agent: r.Agent}
			f.Revision++
			for i := range f.Requests {
				if p := &f.Requests[i]; p.Kind == KindFinish && p.State == StateAccepted {
					p.State, p.Outcome = StateCompleted, "superseded by "+string(r.Kind)+" "+r.ID
				}
			}
			if !busy {
				r.State, r.Outcome = StateCompleted, "held; no owner was running"
			}
		case KindPark:
			if req.Owner == "" {
				return "", fmt.Errorf("%w: a park must name the owner generation it addresses", ErrNoOwner)
			}
			if !busy {
				return "", fmt.Errorf("%w: %s/%s has no owner to park", ErrNoOwner, projectID, runID)
			}
		case KindFinish:
			if f.Held() {
				return "", fmt.Errorf("%w by %s %s; resume it first", ErrHeld, f.Hold.Kind, f.Hold.Request)
			}
			if f.Recovery != nil {
				return "", fmt.Errorf("%w: %s", ErrRecovery, f.Recovery.Reason)
			}
			if md.Status == run.StatusPushed && !busy {
				return "", fmt.Errorf("%w: %s/%s has already shipped and no CI repair is running", ErrNoOwner, projectID, runID)
			}
			if open := f.Open(KindFinish); len(open) > 0 {
				out = open[0]
				return "", run.ErrNothingToCommit
			}
		}
		f.Requests = append(f.Requests, r)
		out = r
		return message(projectID, runID, md.Workflow, string(r.Kind)+" "+r.ID, ""), nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}

// Settlement is what an owner records about a request at a boundary.
type Settlement struct {
	State    State
	Boundary string
	Outcome  string
	// Recovery marks the outcome as unknown: automatic continuation is
	// blocked until an operator Resume retires it.
	Recovery bool
}

// Settle records an owner's progress on a request; applying one also
// stamps the run's status at that moment. Moving a request to
// the state it already has, or touching one already settled, is a no-op
// with no commit — boundaries that re-read the record call it on every
// pass. Completing a pause/handoff leaves its hold in force; only
// Resume lifts it.
//
// consent is the settling walk's MoE-Consent value.
func Settle(root, projectID, runID, id string, s Settlement, consent string) error {
	switch s.State {
	case StateApplied, StateCompleted, StateFailed:
	default:
		return fmt.Errorf("runcontrol: cannot settle to %q", s.State)
	}
	err := commit(root, projectID, runID, "control-settle", func(f *File) (string, error) {
		r := f.find(id)
		if r == nil {
			return "", fmt.Errorf("runcontrol: %s/%s has no request %q", projectID, runID, id)
		}
		if r.Settled() || (r.State == s.State && !s.Recovery) {
			return "", run.ErrNothingToCommit
		}
		r.State, r.Boundary, r.Outcome = s.State, s.Boundary, s.Outcome
		if s.State == StateApplied {
			md, err := run.Load(root, projectID, runID)
			if err != nil {
				return "", err
			}
			r.AppliedStatus = md.Status
		}
		if s.Recovery {
			f.Recovery = &Recovery{Request: id, Reason: s.Outcome}
			f.Revision++
		}
		return message(projectID, runID, workflowOf(root, projectID, runID), string(s.State)+" "+id, consent), nil
	})
	if err != nil || s.State == StateApplied {
		return err
	}
	// The record now carries id settled (here, or already): any copy kept
	// while its commit was failing is spent.
	return dropUnrecorded(root, projectID, runID, id)
}

// Resume clears a hold and retires an unknown-outcome recovery. owner
// must be a live guard on exactly this run, and the run must have no
// unresolved session: resuming is an act of normal admission, so a
// contending owner or unsettled turn leaves the hold intact.
//
// req.ID makes the resume idempotent like any request; req.Revision,
// when non-zero, must match the record's current revision. Returns the
// stored resume request and whether anything was released; with
// nothing held it commits nothing.
//
// Resume restores no authority: a finish whose outcome was unknown
// stays failed.
func Resume(root, projectID, runID string, owner *runowner.Handle, req Request) (Request, bool, error) {
	return release(root, projectID, runID, owner, req, true)
}

// ReleaseForEntry is Resume for an explicit interactive stage entry that
// has already been admitted: its own session reservation is the one
// that would otherwise refuse it, so the reservation check is skipped.
// Every other guard (owner, revision, idempotency) is the same.
func ReleaseForEntry(root, projectID, runID string, owner *runowner.Handle) (Request, bool, error) {
	return release(root, projectID, runID, owner, Request{}, false)
}

func release(root, projectID, runID string, owner *runowner.Handle, req Request, checkSessions bool) (Request, bool, error) {
	if !owner.Owns(root, projectID, runID) {
		return Request{}, false, fmt.Errorf("%w: %s/%s", ErrNotOwner, projectID, runID)
	}
	if req.ID == "" {
		req.ID = NewID()
	}
	req.Kind = KindResume
	var (
		out      Request
		released bool
	)
	err := commit(root, projectID, runID, "control-resume", func(f *File) (string, error) {
		if prev, ok := f.Find(req.ID); ok {
			if !prev.sameIntent(req) {
				return "", fmt.Errorf("%w: %s is a %s", ErrConflict, req.ID, prev.Kind)
			}
			out = prev
			return "", run.ErrNothingToCommit
		}
		if !f.Held() && f.Recovery == nil {
			return "", run.ErrNothingToCommit
		}
		if req.Revision != 0 && req.Revision != f.Revision {
			return "", fmt.Errorf("%w (shown revision %d, now %d)", ErrStale, req.Revision, f.Revision)
		}
		if checkSessions {
			if err := session.RequireUnreserved(root, projectID, runID); err != nil {
				return "", err
			}
		}
		var what []string
		if f.Hold != nil {
			if h := f.find(f.Hold.Request); h != nil && !h.Settled() {
				h.State, h.Outcome = StateCompleted, "held until resume "+req.ID
			}
			what = append(what, "released "+string(f.Hold.Kind)+" "+f.Hold.Request)
			f.Hold = nil
		}
		if f.Recovery != nil {
			what = append(what, "retired recovery for "+f.Recovery.Request)
			f.Recovery = nil
		}
		f.Revision++
		out = Request{ID: req.ID, Kind: KindResume, AcceptedAt: now(), Revision: req.Revision,
			State: StateCompleted, Outcome: strings.Join(what, "; ")}
		f.Requests = append(f.Requests, out)
		released = true
		return message(projectID, runID, workflowOf(root, projectID, runID), "resume "+out.ID, ""), nil
	})
	if err != nil {
		return Request{}, false, err
	}
	return out, released, nil
}

func workflowOf(root, projectID, runID string) string {
	if md, err := run.Load(root, projectID, runID); err == nil {
		return md.Workflow
	}
	return ""
}

func message(projectID, runID, workflow, what, consent string) string {
	return fmt.Sprintf("control: %s %s/%s\n\n", what, projectID, runID) +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
			Consent:  consent,
		}.String()
}

// testLocked, if set, fires inside the lock just before prepare runs —
// the interleave seam, as in internal/input.
var testLocked func()

// commit runs prepare against the record read inside the repository
// lock and lands what it leaves as one journal commit. prepare returns
// run.ErrNothingToCommit for a no-op, which writes and commits nothing.
// A failed commit puts the file and its index entry back as they were.
func commit(root, projectID, runID, purpose string, prepare func(*File) (string, error)) error {
	rel := Path(projectID, runID)
	abs := filepath.Join(root, rel)
	err := repolock.With(root, repolock.Options{
		Purpose: purpose,
		Run:     projectID + "/" + runID,
	}, func() error {
		if testLocked != nil {
			testLocked()
		}
		prev, err := os.ReadFile(abs)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("runcontrol: read %s/%s: %w", projectID, runID, err)
		}
		f, err := Load(root, projectID, runID)
		if err != nil {
			return err
		}
		msg, err := prepare(&f)
		if err != nil {
			return err
		}
		if err := writeFile(abs, f); err != nil {
			return err
		}
		if err := run.StageAndCommit(root, msg, rel); err != nil {
			return restore(root, rel, abs, prev, existed, err)
		}
		return nil
	})
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
}

// restore undoes this writer's file and index change after a failed
// commit, touching nothing else in the tree.
func restore(root, rel, abs string, prev []byte, existed bool, cause error) error {
	var errs []error
	if err := git.Run(root, "reset", "-q", "--", rel); err != nil {
		errs = append(errs, err)
	}
	if existed {
		errs = append(errs, os.WriteFile(abs, prev, 0o644))
	} else if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("runcontrol: commit failed (%w); restoring %s also failed: %v", cause, rel, err)
	}
	return fmt.Errorf("runcontrol: commit failed, nothing recorded: %w", cause)
}

// writeFile replaces the record atomically: boundaries read it without
// the lock, and a torn read would read as malformed.
func writeFile(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if f.Requests == nil {
		f.Requests = []Request{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".control-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o644)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
