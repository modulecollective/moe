// Package ui is moe ui's terminal console: a board pane with selection
// and a detail pane for the selected item's facts, a small text editor
// and a command line. It owns the event loop, selection, panes, the key
// decoder and the text buffer, and nothing else: what is on the board,
// what an item's details say and what an action does all come from the
// caller (internal/cli) as plain data and one callback. The package
// imports no workflow policy, and no terminal type leaves it.
//
// One goroutine owns the model and is the only writer to the terminal.
// Input, board refreshes and mutation results arrive on channels as
// complete values; mutations run one at a time on a worker so the board
// stays live while a write waits on the repository lock.
package ui

import "time"

// ItemKind is what a board entry is.
type ItemKind int

const (
	ItemRun ItemKind = iota
	ItemIdea
	ItemIntent
	ItemChore
)

// Item is one selectable board entry. Project, Run and Bucket identify
// the occurrence: a capture with an open edit session appears twice,
// once per bucket.
type Item struct {
	Project, Run string
	Bucket       int
	Kind         ItemKind
	// Line is the board line the item is painted on.
	Line int
	// Detail is what the detail pane shows, one fact per line. Plain
	// text; the console makes it safe to paint.
	Detail []string
	// Body is a capture's canvas (ideas and intents).
	Body string
	// Editable: an open capture whose body can be edited, closed and —
	// for ideas — tagged.
	Editable bool
	// Notes: the run takes operator notes.
	Notes bool
	// Controls: run controls (pause, handoff, park, finish, resume) apply.
	Controls bool
	// Question is the open question the operator would answer, if any.
	Question *Question
	// Generation is the live owner's generation when one answered; a
	// park and a terminal reveal are bound to it.
	Generation string
	// HoldRevision is the hold a resume is rendered against.
	HoldRevision int
	// Terminal: the live owner reported a Rex terminal to reveal.
	Terminal bool
}

// Label is the item's project/run.
func (it Item) Label() string { return it.Project + "/" + it.Run }

// Question is an open operator question, by its id in the input record.
type Question struct {
	ID   int
	Text string
}

// Board is one complete board: its painted lines and the items on them,
// ordered by Line.
type Board struct {
	Lines []string
	Items []Item
	// Header is the one-line context beside the title (serve state).
	Header string
	// Seq increases with every board the caller builds; the console
	// ignores an older one arriving late.
	Seq uint64
	At  time.Time
}

// ActionKind is a mutation the console asks the caller to make.
type ActionKind int

const (
	ActNote ActionKind = iota
	ActAnswer
	ActControl
	ActCreate
	ActEdit
	ActClose
	ActTag
	ActReveal
)

// Action is one mutation, bound to the identity it was captured against
// when the operator committed to it. The console never retargets it.
type Action struct {
	Kind         ActionKind
	Project, Run string
	// Text is a note, answer or capture body.
	Text string
	// QuestionID binds an answer to the question it answers.
	QuestionID int
	// Control is pause, handoff, park, finish or resume; RequestID is
	// kept across an uncertain outcome so a retry reads back rather than
	// asking twice.
	Control   string
	RequestID string
	// Generation binds a park (and a reveal) to the owner it addressed;
	// HoldRevision binds a resume to the hold that was shown.
	Generation   string
	HoldRevision int
	// Expected is the capture body an edit started from.
	Expected string
	// Workflow is the capture workflow to create (idea, intent), or the
	// tag's workflow ("" untags). DesignOnly narrows an sdlc tag.
	Workflow   string
	DesignOnly bool
	// Client is the Rex app client a reveal targets, once chosen.
	Client string
}

// Result is what became of an Action.
type Result struct {
	OK bool
	// Uncertain: the outcome is unknown. Lines carry the readback; the
	// console keeps the draft and asks before a new submission.
	Uncertain bool
	// Changed: a capture edit refused because the canvas moved on.
	Changed bool
	Message string
	Lines   []string
	// Choices asks the operator to pick a Rex client for a reveal.
	Choices []Choice
	// RequestID is the control request id used, for a retry.
	RequestID string
}

// Choice is one option in a pick list.
type Choice struct{ ID, Label string }
