// Package input owns the run-local human-input record: prose the
// operator pushes at a run, and prose a pulse asks the operator for.
// Both reach the run's next agent turn through its stage prompt.
//
// The record is projects/<project>/runs/<run>/inputs.json, sibling of
// run.json. It exists only for runs that have received something, so
// nothing pays for the feature until it is used.
//
// One entry grammar carries both directions:
//
//   - text, no question — an operator note, pending until delivered.
//   - question, no text — an open ping: a pulse's question awaiting the
//     operator. It delivers nothing, and is what the queue surfaces as
//     needing a human.
//   - question and text — an answered ping, which is a pending note with
//     its context attached, delivered as the pair.
//
// Nothing here holds anything. An open ping does not stop a stage turn
// or a kick; stillness is the pulse's park to write, re-stated per
// sweep. The record's job is to make the question durable and
// answerable, not to enforce quiet.
//
// A run may carry at most one open ping — that is what lets a reply
// address the run rather than an id. Notes are unlimited.
//
// Add, Ask, Answer, MarkDelivered, and Carry each write one journal
// commit through the shared lock/pull/push pipeline. Each reads the
// record and rewrites it inside that lock — the file is rewritten
// whole, so a read taken before the acquire would clobber whatever a
// concurrent writer landed in between. None starts an agent: the commit
// moves the journal, and the next heartbeat sweep is what picks the
// work back up.
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
	"github.com/modulecollective/moe/internal/trailers"
)

// FileName is the per-run record's basename, sibling of run.json.
const FileName = "inputs.json"

var (
	// ErrMalformed reports an inputs.json that doesn't satisfy the
	// invariants below. Read as a refusal, never repaired in place: the
	// file is machine-written, so a violation is a bug to see, not
	// noise to route around.
	ErrMalformed = errors.New("input: malformed inputs.json")
	// ErrOpenPing reports an ask on a run that already has one open. One
	// open ping per run is the whole reason a reply needs no id.
	ErrOpenPing = errors.New("input: run already has an open ping")
	// ErrNoOpenPing reports an answer aimed at a run with nothing open.
	ErrNoOpenPing = errors.New("input: run has no open ping")
	// ErrStalePing reports an answer naming an id that isn't the open
	// one — a phone tab that has been sitting on a question since it was
	// answered and replaced.
	ErrStalePing = errors.New("input: id is not the run's open ping")
	// ErrEmpty reports a note or question that is blank after trimming.
	ErrEmpty = errors.New("input: entry is empty")
	// ErrNotLive reports an add, ask, or answer against a run that is not
	// in progress. A terminal run has no next turn, so nothing written on
	// one could ever be delivered.
	ErrNotLive = errors.New("input: run is not in progress")
)

// Entry is one item in a run's input record — an operator note, an open
// ping, or an answered ping. See the package comment for the grammar.
type Entry struct {
	// ID is one-based and dense within the file: entry n is at index
	// n-1. Ids are per-run, so an entry's address is <project>/<run>#<id>.
	ID int `json:"id"`
	// AskedBy is the qualified "<project>/<run>" of the pulse that asked,
	// set on pings and empty on operator notes.
	AskedBy string `json:"asked_by,omitempty"`
	// Question is a pulse's prose question, empty on an operator note.
	Question string `json:"question,omitempty"`
	// Text is the operator's prose — the note, or the ping's answer.
	// Empty means the ping is still open.
	Text string `json:"text,omitempty"`
	// DeliveredTo names the stage doc whose turn consumed this entry.
	// Empty means pending. Only ever set on an entry with text: an open
	// ping delivers nothing, so it consumes nothing.
	DeliveredTo string `json:"delivered_to,omitempty"`
}

// IsPing reports whether a pulse asked this entry, answered or not.
func (e Entry) IsPing() bool { return e.Question != "" }

// Open reports whether this is a ping still awaiting the operator.
func (e Entry) Open() bool { return e.Question != "" && e.Text == "" }

// Pending reports whether this entry has prose no turn has consumed yet.
func (e Entry) Pending() bool { return e.Text != "" && e.DeliveredTo == "" }

// Delivered reports whether a turn has already consumed this entry.
func (e Entry) Delivered() bool { return e.DeliveredTo != "" }

// FirstLine is the entry reduced to one identifying line, for the
// surfaces that list many at once — the pulse kickoff, the CLI's queue
// view, the web queue's read-only half.
//
// A ping shows its question even once answered: the question is what
// both sides recognise the entry by, where the answer alone ("Adopt the
// new default.") reads as a fragment. A bare note has only its text.
func (e Entry) FirstLine() string {
	body := e.Question
	if body == "" {
		body = e.Text
	}
	line, _, _ := strings.Cut(body, "\n")
	return strings.TrimSpace(line)
}

// File is the on-disk shape of inputs.json.
type File struct {
	Notes []Entry `json:"notes"`
}

// OpenPing returns the run's single unanswered ping, if it has one.
func (f File) OpenPing() (Entry, bool) {
	for _, e := range f.Notes {
		if e.Open() {
			return e, true
		}
	}
	return Entry{}, false
}

// Pending returns every entry carrying prose no turn has consumed,
// oldest first — what the next stage prompt delivers.
func (f File) Pending() []Entry {
	var out []Entry
	for _, e := range f.Notes {
		if e.Pending() {
			out = append(out, e)
		}
	}
	return out
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

// validate enforces the file's invariants: dense one-based ids, prose in
// at least one of the two fields, no blank-but-present field (so the
// predicates above can compare against "" exactly), a delivery stamp
// only where there is text to have delivered, and at most one open ping.
func (f File) validate() error {
	open := 0
	for i, e := range f.Notes {
		if e.ID != i+1 {
			return fmt.Errorf("entry %d has id %d", i+1, e.ID)
		}
		if e.Question == "" && e.Text == "" {
			return fmt.Errorf("entry %d has neither a question nor text", e.ID)
		}
		if e.Question != "" && strings.TrimSpace(e.Question) == "" {
			return fmt.Errorf("entry %d has a blank question", e.ID)
		}
		if e.Text != "" && strings.TrimSpace(e.Text) == "" {
			return fmt.Errorf("entry %d has blank text", e.ID)
		}
		if e.DeliveredTo != "" && e.Text == "" {
			return fmt.Errorf("entry %d is marked delivered with no text", e.ID)
		}
		if e.Open() {
			open++
		}
	}
	if open > 1 {
		return fmt.Errorf("%d open pings; at most one is allowed", open)
	}
	return nil
}

// Add appends an operator note to a run's record and commits it.
//
// No consent trailer: a note is the operator's own act, which is the
// whole point of the record.
func Add(root, projectID, runID, text string) (Entry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Entry{}, fmt.Errorf("%w: note has no text", ErrEmpty)
	}
	var e Entry
	err := commit(root, projectID, runID, "input-add", func() (File, string, error) {
		md, f, err := loadLive(root, projectID, runID)
		if err != nil {
			return File{}, "", err
		}
		e = Entry{ID: len(f.Notes) + 1, Text: text}
		f.Notes = append(f.Notes, e)

		ref := fmt.Sprintf("%s/%s#%d", projectID, runID, e.ID)
		return f, fmt.Sprintf("input: note %s\n\n", ref) +
			trailers.Block{
				Run:        runID,
				Project:    projectID,
				Workflow:   md.Workflow,
				InputAdded: ref,
			}.String(), nil
	})
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Ask opens one prose ping on a run and commits it. askedBy is the
// qualified "<project>/<run>" of the asking pulse; consent is the
// MoE-Consent value of the walk it made the ask under (a pulse always
// has one — the ask is a machine act).
//
// Refuses a run that isn't in progress (ErrNotLive), an empty question
// (ErrEmpty), and a run that already has one open (ErrOpenPing). All
// three are the caller's cue to warn and carry on rather than fail the
// sweep.
func Ask(root, projectID, runID, askedBy, question, consent string) (Entry, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Entry{}, fmt.Errorf("%w: question has no text", ErrEmpty)
	}
	var e Entry
	err := commit(root, projectID, runID, "input-ask", func() (File, string, error) {
		md, f, err := loadLive(root, projectID, runID)
		if err != nil {
			return File{}, "", err
		}
		if open, ok := f.OpenPing(); ok {
			return File{}, "", fmt.Errorf("%w: %s/%s#%d — %s", ErrOpenPing, projectID, runID, open.ID, open.Question)
		}
		e = Entry{ID: len(f.Notes) + 1, AskedBy: askedBy, Question: question}
		f.Notes = append(f.Notes, e)

		ref := fmt.Sprintf("%s/%s#%d", projectID, runID, e.ID)
		return f, fmt.Sprintf("input: ask %s\n\n", ref) +
			trailers.Block{
				Run:        runID,
				Project:    projectID,
				Workflow:   md.Workflow,
				Consent:    consent,
				InputAsked: ref,
			}.String(), nil
	})
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Answer fills a run's open ping with the operator's prose and commits
// it. id is the ping the caller believes is open; zero means "whatever
// is open" — the CLI passes zero because a run has at most one, and the
// web passes the rendered id so a stale phone tab cannot answer a newer
// question.
//
// The answered ping becomes an ordinary pending entry: the next turn's
// prompt carries the question and the answer as a pair.
func Answer(root, projectID, runID string, id int, text string) (Entry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Entry{}, fmt.Errorf("%w: answer has no text", ErrEmpty)
	}
	var answered Entry
	err := commit(root, projectID, runID, "input-answer", func() (File, string, error) {
		md, f, err := loadLive(root, projectID, runID)
		if err != nil {
			return File{}, "", err
		}
		open, ok := f.OpenPing()
		if !ok {
			return File{}, "", fmt.Errorf("%w: %s/%s", ErrNoOpenPing, projectID, runID)
		}
		if id != 0 && id != open.ID {
			return File{}, "", fmt.Errorf("%w: %s/%s has #%d open, not #%d", ErrStalePing, projectID, runID, open.ID, id)
		}
		f.Notes[open.ID-1].Text = text
		answered = f.Notes[open.ID-1]

		ref := fmt.Sprintf("%s/%s#%d", projectID, runID, answered.ID)
		return f, fmt.Sprintf("input: answer %s\n\n", ref) +
			trailers.Block{
				Run:           runID,
				Project:       projectID,
				Workflow:      md.Workflow,
				InputAnswered: ref,
			}.String(), nil
	})
	if err != nil {
		return Entry{}, err
	}
	return answered, nil
}

// MarkDelivered stamps docID on the entries a successful stage turn
// carried, as its own journal commit. ids are the entries the prompt
// actually rendered; anything in the list that has since been delivered,
// or that never had text, is skipped.
//
// Called after the turn rather than folded into its commit, and reading
// the record fresh rather than trusting a snapshot: the session worktree
// pulled before the turn, so a note added *mid-turn* exists on main but
// not in the worktree's copy of this file. Stamping here, from the
// canonical root the session has already pushed into, leaves that note
// pending for the following turn instead of clobbering it.
//
// A turn that failed calls nothing, so the next attempt redelivers. An
// empty id list is a no-op with no commit and no lock — what nearly
// every turn does; a list whose entries have all been delivered already
// takes the lock to find that out, and still commits nothing.
//
// consent is the caller's MoE-Consent value, empty for an operator-driven
// turn. The stage that calls this is shared between a typed `moe sdlc
// <stage>` and a heartbeat walk, so it is one of the sites walkConsent's
// rule covers: the ride level under a walk, nothing otherwise.
func MarkDelivered(root, projectID, runID, docID string, ids []int, consent string) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return commit(root, projectID, runID, "input-delivered", func() (File, string, error) {
		f, err := Load(root, projectID, runID)
		if err != nil {
			return File{}, "", err
		}
		var marked []string
		for i := range f.Notes {
			e := &f.Notes[i]
			if !want[e.ID] || !e.Pending() {
				continue
			}
			e.DeliveredTo = docID
			marked = append(marked, strconv.Itoa(e.ID))
		}
		if len(marked) == 0 {
			return File{}, "", run.ErrNothingToCommit
		}

		ref := fmt.Sprintf("%s/%s#%s", projectID, runID, strings.Join(marked, ","))
		// No MoE-Document trailer, deliberately: that trailer is how the
		// harness reads "this stage's doc turn happened", and a bookkeeping
		// stamp is not a turn. The doc rides the delivered value instead.
		return f, fmt.Sprintf("input: delivered %s to %s\n\n", ref, docID) +
			trailers.Block{
				Run:            runID,
				Project:        projectID,
				Consent:        consent,
				InputDelivered: ref + " " + docID,
			}.String(), nil
	})
}

// Carry copies a source run's undelivered input onto a destination run
// as one journal commit on the destination's record. The promotion edge
// is the only caller: an idea's entries are addressed to work that has
// not started yet, and promoting the idea makes the destination run the
// thing that work happens on. Without this the entries would be
// stranded — a promoted idea is terminal, so Scan drops it and no turn
// ever renders them.
//
// Copy, not move: the source record is left untouched, including its
// delivery stamps (nothing consumed the entries there). While the
// source is terminal its originals are invisible to every scan, so
// nothing shows twice; and if the destination is abandoned and the idea
// reopened, the originals are live again and the next promote re-carries
// them.
//
// Pending notes, answered pings (question and answer travel together),
// and the source's open ping all ride. Already-delivered entries do not.
// If the source has an open ping and the destination already has one,
// the source's is dropped with a warning: one open ping per run is what
// lets a reply address the run, and a machine's unanswered question is
// re-askable on the destination by the next sweep.
//
// consent is the caller's MoE-Consent value, empty for an operator's own
// promote. Returns the number of entries carried; zero with no error and
// no commit is the overwhelmingly common case (no record, or nothing
// left undelivered) — it costs one brief lock hold to find that out,
// since the source read has to sit under the same lock as the write.
func Carry(root, srcProject, srcRun, dstProject, dstRun, consent string, stderr io.Writer) (int, error) {
	var carried int
	// One prepare closure reads both records: the lock is repo-wide, so
	// a single hold covers the source read and the destination rewrite.
	err := commit(root, dstProject, dstRun, "input-carry", func() (File, string, error) {
		src, err := Load(root, srcProject, srcRun)
		if err != nil {
			return File{}, "", err
		}
		var carry []Entry
		for _, e := range src.Notes {
			if e.Pending() || e.Open() {
				carry = append(carry, e)
			}
		}
		if len(carry) == 0 {
			return File{}, "", run.ErrNothingToCommit
		}
		md, dst, err := loadLive(root, dstProject, dstRun)
		if err != nil {
			return File{}, "", err
		}
		_, dstOpen := dst.OpenPing()

		var srcIDs, dstIDs []string
		for _, e := range carry {
			if e.Open() {
				if dstOpen {
					fmt.Fprintf(stderr, "input: dropping open ping %s/%s#%d — %s/%s already has one open\n",
						srcProject, srcRun, e.ID, dstProject, dstRun)
					continue
				}
				dstOpen = true
			}
			copied := Entry{ID: len(dst.Notes) + 1, AskedBy: e.AskedBy, Question: e.Question, Text: e.Text}
			dst.Notes = append(dst.Notes, copied)
			srcIDs = append(srcIDs, strconv.Itoa(e.ID))
			dstIDs = append(dstIDs, strconv.Itoa(copied.ID))
		}
		if len(srcIDs) == 0 {
			return File{}, "", run.ErrNothingToCommit
		}
		carried = len(srcIDs)

		srcRef := fmt.Sprintf("%s/%s#%s", srcProject, srcRun, strings.Join(srcIDs, ","))
		dstRef := fmt.Sprintf("%s/%s#%s", dstProject, dstRun, strings.Join(dstIDs, ","))
		return dst, fmt.Sprintf("input: carried %s → %s\n\n", srcRef, dstRef) +
			trailers.Block{
				Run:      dstRun,
				Project:  dstProject,
				Workflow: md.Workflow,
				Consent:  consent,
			}.String(), nil
	})
	if err != nil {
		return 0, err
	}
	return carried, nil
}

// loadLive resolves a run that may still receive input: in progress, and
// with a readable record.
func loadLive(root, projectID, runID string) (*run.Metadata, File, error) {
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return nil, File{}, err
	}
	if md.Status != run.StatusInProgress {
		return nil, File{}, fmt.Errorf("%w: %s/%s is %s", ErrNotLive, projectID, runID, md.Status)
	}
	f, err := Load(root, projectID, runID)
	if err != nil {
		return nil, File{}, err
	}
	return md, f, nil
}

// testLocked, if set, fires inside the lock just before prepare runs.
// It is the seam the interleave tests use to stand in for another
// process that won an earlier hold and rewrote the record: real
// contention can't drive them, because repolock refuses a nested
// acquire from the same process. Nil in production.
var testLocked func()

// commit runs prepare under the repo lock and lands what it returns as
// one journal commit through the shared lock/pull/push pipeline.
//
// prepare rather than a ready File: every state-dependent read a verb
// makes — the record, run.json, the check-then-act refusals, the id it
// assigns — has to happen inside the lock. A read taken before the
// acquire is a snapshot, and rewriting the whole file from it silently
// drops whatever another writer landed in between.
//
// A prepare with nothing to write returns run.ErrNothingToCommit, which
// skips the commit (repolock.With passes it through untouched) and
// maps back to nil here — the no-op verbs' contract.
func commit(root, projectID, runID, purpose string, prepare func() (File, string, error)) error {
	rel := Path(projectID, runID)
	err := repolock.With(root, repolock.Options{
		Purpose: purpose,
		Run:     projectID + "/" + runID,
	}, func() error {
		if testLocked != nil {
			testLocked()
		}
		f, msg, err := prepare()
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(root, rel), f); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, rel)
	})
	if errors.Is(err, run.ErrNothingToCommit) {
		return nil
	}
	return err
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

// Waiting is one entry the board is still carrying: an open ping needing
// the operator, or a pending note needing a turn.
type Waiting struct {
	Project string
	Run     string
	Entry   Entry
	// When is the record's last git activity. Every entry in one run
	// shares it, which is enough to order runs against each other —
	// oldest queue first — and the only ordering these surfaces claim.
	// Zero when the record isn't in git yet.
	When time.Time
}

// Ref renders the address the CLI and the web both print.
func (w Waiting) Ref() string {
	return w.Project + "/" + w.Run + "#" + strconv.Itoa(w.Entry.ID)
}

// Scan returns everything still in flight on the board, oldest first:
// open pings the operator owes an answer, and pending notes a turn owes
// a pickup. Callers partition with Entry.Open().
//
// Terminal runs drop out: their history stays visible on the run page,
// but there is nothing left to discharge and no turn left to deliver to.
//
// projectFilter scopes to one project when non-empty.
//
// A run whose record is malformed is skipped with its error collected —
// one bad file degrades to one missing row rather than an empty queue.
func Scan(root, projectFilter string) ([]Waiting, []error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, []error{err}
	}
	var out []Waiting
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
		var live []Entry
		if open, ok := f.OpenPing(); ok {
			live = append(live, open)
		}
		live = append(live, f.Pending()...)
		if len(live) == 0 {
			continue
		}
		// Best-effort: a record not yet in git (a write mid-flight, a
		// hand-made fixture) sorts as zero rather than dropping the rows.
		when, _ := run.LastFileActivity(root, Path(md.Project, md.ID))
		for _, e := range live {
			out = append(out, Waiting{Project: md.Project, Run: md.ID, Entry: e, When: when})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].When.Equal(out[j].When) {
			return out[i].When.Before(out[j].When)
		}
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		if out[i].Run != out[j].Run {
			return out[i].Run < out[j].Run
		}
		return out[i].Entry.ID < out[j].Entry.ID
	})
	return out, errs
}
