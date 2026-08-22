// Package cli — the human-input inbox.
//
// One noun: the request. A dynamic pulse writes a structured park on a
// thread position (see pulseRunAsk) and that opens a durable question on
// the run sitting there; `moe inbox list` shows what is waiting and
// `moe inbox answer` discharges it. The record and its state machine
// live in internal/input; this file is the operator's door to it, plus
// the two floors and the two prompt blocks that make an unanswered
// question actually hold work.
//
// Answering starts nothing. It writes one journal commit, which moves
// the journal tip, so an armed serve's next heartbeat offers the project
// to a pulse and the ordinary self-kick carries the thread on. The
// expedite path is a hand-typed `moe pulse new --dynamic <project>`.
package cli

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

func init() {
	g := NewCommandGroup("inbox", "human inputs a run is waiting on")
	g.Register(&Command{
		Name:    "list",
		Summary: "show every unanswered question, oldest first",
		Run:     runInboxList,
	})
	g.Register(&Command{
		Name:    "answer",
		Summary: "answer a run's open question by choice number",
		Run:     runInboxAnswer,
		argKind: argProjectRun,
	})
	RegisterGroup(g)
}

func runInboxList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe inbox list [project]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Every question a run is waiting on, oldest first. Each entry names the")
		moePrintln(stderr, "run, the question, the numbered choices, and the command that answers it.")
		moePrintln(stderr, "A question holds its thread: nothing on it rides until you answer.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	projectFilter := fs.Arg(0)
	if projectFilter != "" {
		if err := requireProject(root, projectFilter); err != nil {
			moePrintf(stderr, "inbox list: %v\n", err)
			return 1
		}
	}
	pending, errs := input.Scan(root, projectFilter)
	for _, err := range errs {
		moePrintf(stderr, "inbox list: %v\n", err)
	}
	if len(pending) == 0 {
		moePrintln(stdout, "inbox: nothing waiting")
		return 0
	}
	for i, p := range pending {
		if i > 0 {
			moePrintln(stdout, "")
		}
		moePrintf(stdout, "%s/%s — %s\n", p.Project, p.Run, p.Request.Question)
		for n, c := range p.Request.Choices {
			moePrintf(stdout, "  %d. %s\n", n+1, c)
		}
		moePrintf(stdout, "  moe inbox answer %s/%s <1-%d>\n", p.Project, p.Run, len(p.Request.Choices))
	}
	return 0
}

// runInboxAnswer takes no question id: v1 permits one open request per
// run, so the run *is* the address. The web posts an id anyway, because
// a phone tab can sit on a stale question for a day; a terminal cannot.
func runInboxAnswer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox answer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe inbox answer <project>/<run> <choice-number>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Records your choice on the run's open question and commits it. No")
		moePrintln(stderr, "agent starts: the commit moves the journal, so an armed serve's next")
		moePrintln(stderr, "sweep picks the work back up. `moe pulse new --dynamic <project>`")
		moePrintln(stderr, "is the expedite path.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "inbox answer: %v\n", err)
		return 2
	}
	choice, err := strconv.Atoi(fs.Arg(1))
	if err != nil {
		moePrintf(stderr, "inbox answer: %q is not a choice number\n", fs.Arg(1))
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "inbox answer: %v\n", err)
		return 1
	}
	req, err := input.Answer(root, projectID, runID, 0 /*any open*/, choice, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "inbox answer: %v\n", err)
		return 1
	}
	moePrintf(stdout, "answered %s/%s#%d — %s\n", projectID, runID, req.ID, req.Answer())
	return 0
}

// refuseOnOpenInput is the stage-entry floor: a run with an unanswered
// question does not open a stage, whoever asked and however headlessly.
//
// The refusal points at `moe inbox answer` rather than offering to take
// the answer here. An attended turn that answered a *different,
// unrecorded* version of the question would leave the durable record
// open and the next sweep still holding the thread — which is the exact
// failure the record exists to prevent.
//
// A malformed record refuses too, for the same reason the kick's floor
// holds on one: the file is machine-written, so a violation is a bug to
// see, not a reason to run work whose held-ness nothing can now answer.
func refuseOnOpenInput(root, projectID, runID string, stderr io.Writer) int {
	f, err := input.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	req, ok := f.Open()
	if !ok {
		return 0
	}
	moePrintf(stderr, "%s/%s is awaiting input — %s\n", projectID, runID, req.Question)
	for n, c := range req.Choices {
		moePrintf(stderr, "  %d. %s\n", n+1, c)
	}
	moePrintf(stderr, "hint: moe inbox answer %s/%s <1-%d>\n", projectID, runID, len(req.Choices))
	return 1
}

// refuseRideOnOpenInput is the same floor at the head of a ride, asked
// of every live member rather than one run. The stage floor would catch
// the member eventually — but "eventually" means after the runs ahead of
// it have already shipped, which turns one answerable question into a
// half-ridden chain. The head asks first so the refusal costs nothing.
//
// graph is the caller's snapshot (see liveChainParent), so the walk here
// is the same one the kick is about to ride. A nil graph reads as "no
// thread to walk" and defers to the stage floor, which is where the
// caller's own guard already refused.
//
// Returns 0 when the ride may proceed.
func refuseRideOnOpenInput(root string, graph *run.ChainGraph, md *run.Metadata, stderr io.Writer) int {
	if graph == nil {
		return 0
	}
	for _, key := range graph.Thread(md.Project + "/" + md.ID) {
		proj, runID, err := splitProjectRun(key)
		if err != nil {
			continue
		}
		if code := refuseOnOpenInput(root, proj, runID, stderr); code != 0 {
			return code
		}
	}
	return 0
}

// humanInputsSection is the `Human inputs` block every stage prompt on a
// run with answered questions carries: each question and the choice the
// operator picked.
//
// Answered only, and not because open ones are uninteresting — because a
// stage turn cannot reach this with one open. refuseOnOpenInput sits at
// the top of runStageSession, so by the time a prompt is assembled the
// record's open request is either answered or the turn never started.
// Rendering "(unanswered)" here would be describing a state the caller
// has already refused.
//
// It is context, not permission. A stage reading "the operator chose
// option 2" learns what to build; it does not learn that it may go back
// and rewrite an upstream canvas to match. The block says so, because
// the alternative is a code stage helpfully re-deciding a design.
//
// Returns "" for the overwhelming majority of runs, which have no record
// at all.
func humanInputsSection(root string, md *run.Metadata) string {
	f, err := input.Load(root, md.Project, md.ID)
	if err != nil {
		return ""
	}
	var answered []input.Request
	for _, req := range f.Requests {
		if req.Answered() {
			answered = append(answered, req)
		}
	}
	if len(answered) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Human inputs\n\n")
	b.WriteString("Questions this run put to the operator, and what they answered. The\n")
	b.WriteString("answer is context for the work ahead of you — it is not licence to\n")
	b.WriteString("rewrite an earlier canvas to match it.\n\n")
	for _, req := range answered {
		fmt.Fprintf(&b, "- %s\n  answer: %s\n", req.Question, req.Answer())
	}
	return b.String()
}

// openInputsBlock is the pulse kickoff's view of the inbox: every open
// question on the board this sweep can see, plus the answers that landed
// since the previous sweep, plus the bar for asking a new one.
//
// The guidance is not decoration. A precomputed list of questions with
// no standard for discharging them is inert — the survey would read
// "awaiting input" as one more parked thread and write the park again.
// So the block says what an answer obliges: interpret it, drop the park,
// and let the ordinary floors carry the thread.
//
// Board-wide by project, matching every other kickoff block. Empty when
// nothing is open and nothing landed, which is the normal sweep.
func openInputsBlock(sc *pulseScan, projectID, currentRunID string) string {
	pending, _ := input.Scan(sc.root, projectID)
	landed := answeredSince(sc, projectID, previousPulseAt(sc, projectID, currentRunID))

	if len(pending) == 0 && len(landed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Human inputs. A question you asked on a run holds that run's whole thread " +
		"until the operator answers it — the harness refuses the kick, not just this sweep.\n")
	if len(landed) > 0 {
		b.WriteString("\nAnswered since your last sweep — read each one, then let the thread run. " +
			"An answer is not a licence to re-park: if the answer says the work should stay " +
			"still, write a plain `park` reason saying why, and otherwise write no park at all " +
			"and let the ordinary floors decide.\n")
		for _, a := range landed {
			fmt.Fprintf(&b, "- `%s/%s` — %s → **%s**\n", a.Project, a.Run, a.Request.Question, a.Request.Answer())
		}
	}
	if len(pending) > 0 {
		b.WriteString("\nStill unanswered — do not ask these again, and do not ask a second question " +
			"on the same run:\n")
		for _, p := range pending {
			fmt.Fprintf(&b, "- `%s/%s` — %s\n", p.Project, p.Run, p.Request.Question)
		}
	}
	b.WriteString("\nTo ask one, write a thread entry as " +
		"`{\"run\": \"<slug>\", \"park\": {\"question\": \"…?\", \"choices\": [\"…\", \"…\"]}}` — an " +
		"existing run at its own position, two or three distinct choices, no free text. " +
		"Ask only the single question that changes what happens next, and only where every " +
		"choice is usable implementation guidance. If what you need is an operator *act* — " +
		"close the run, tag an idea, change the project's mode — write a plain `park` naming " +
		"that act instead: the inbox cannot discharge it.\n")
	return b.String()
}

// previousPulseAt is when this project's previous sweep last moved,
// which is the cutoff "since your last sweep" means. The current run is
// excluded — it is this sweep, and its own commits are not news to it.
// Zero when the project has never swept, which reads as "everything is
// since", the right answer for a first sweep.
func previousPulseAt(sc *pulseScan, projectID, currentRunID string) time.Time {
	var latest time.Time
	for _, md := range sc.mds {
		if md.Project != projectID || md.Workflow != pulseWorkflow || md.ID == currentRunID {
			continue
		}
		when := sc.idx.LastActivity[md.Project+"/"+md.ID]
		if when.After(latest) {
			latest = when
		}
	}
	return latest
}

// answeredSince lists the answers that landed on this project's runs
// after cutoff.
//
// The test is the record's own last write, which the one-open-per-run
// rule makes exact for the case that matters: a record written after the
// cutoff whose newest request is answered was written by an answer
// commit, because an ask would have left that request open. The case it
// under-reports is an answer *and* a fresh ask both landing in the same
// window — and there the run shows up in the open list instead, which is
// the more urgent half. The run's own stage prompt carries the full
// history either way.
func answeredSince(sc *pulseScan, projectID string, cutoff time.Time) []input.Pending {
	var out []input.Pending
	for _, md := range sc.mds {
		if md.Project != projectID {
			continue
		}
		f, err := input.Load(sc.root, md.Project, md.ID)
		if err != nil || len(f.Requests) == 0 {
			continue
		}
		newest := f.Requests[len(f.Requests)-1]
		if !newest.Answered() {
			continue
		}
		when, err := run.LastFileActivity(sc.root, input.Path(md.Project, md.ID))
		if err != nil || !when.After(cutoff) {
			continue
		}
		out = append(out, input.Pending{Project: md.Project, Run: md.ID, Request: newest, Asked: when})
	}
	return out
}
