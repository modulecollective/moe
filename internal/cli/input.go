// Package cli — the operator's input channel to a run.
//
// One noun: the entry. The operator pushes prose at a run with
// `moe input add`, a dynamic pulse asks for prose with a gate `ask`
// entry, and `moe input answer` fills it in. `moe input list` shows both
// halves of what is still in flight. The record and its grammar live in
// internal/input; this file is the operator's door to it, plus the
// prompt section that delivers entries and the kickoff block that tells
// a survey what the board is carrying.
//
// Nothing here holds anything. Writing an entry starts nothing and stops
// nothing: it writes one journal commit, which moves the journal tip, so
// an armed serve's next heartbeat offers the project to a pulse and the
// ordinary kick carries the thread on. The expedite path is a
// hand-typed `moe pulse new --dynamic <project>`.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

func init() {
	g := NewCommandGroup("input", "prose the operator and a run's agents exchange")
	g.Register(&Command{
		Name:    "add",
		Summary: "push a note at a run's next agent turn",
		Run:     runInputAdd,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "answer",
		Summary: "answer the question a run asked you",
		Run:     runInputAnswer,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "show open questions and undelivered notes, oldest first",
		Run:     runInputList,
	})
	RegisterGroup(g)
}

func runInputAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("input add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe input add <project>/<run> [text ...]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Pushes a chunk of prose at the run. With no text arguments the note is")
		moePrintln(stderr, "read from stdin, so a paragraph can be piped or heredoc'd. The run's")
		moePrintln(stderr, "next successful agent turn receives it verbatim in its prompt, once.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Starts nothing: the commit moves the journal, so an armed serve's next")
		moePrintln(stderr, "sweep picks the work back up. `moe pulse new --dynamic <project>` is")
		moePrintln(stderr, "the expedite path.")
	}
	return inputWrite(fs, args, stdout, stderr, "input add",
		func(root, projectID, runID, text string) (input.Entry, error) {
			return input.Add(root, projectID, runID, text, stdout, stderr)
		},
		func(projectID, runID string, e input.Entry) string {
			return fmt.Sprintf("noted on %s/%s#%d\n", projectID, runID, e.ID)
		})
}

func runInputAnswer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("input answer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe input answer <project>/<run> [text ...]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Fills the run's open question with your prose. With no text arguments")
		moePrintln(stderr, "the answer is read from stdin. The run's next successful agent turn")
		moePrintln(stderr, "receives the question and your answer as a pair.")
		moePrintln(stderr, "")
		moePrintln(stderr, "A run carries at most one open question, so the run is the address. A")
		moePrintln(stderr, "question you'd rather not answer is discharged by saying so in prose —")
		moePrintln(stderr, "there is no dismiss verb.")
	}
	return inputWrite(fs, args, stdout, stderr, "input answer",
		func(root, projectID, runID, text string) (input.Entry, error) {
			return input.Answer(root, projectID, runID, 0 /*whatever is open*/, text, stdout, stderr)
		},
		func(projectID, runID string, e input.Entry) string {
			return fmt.Sprintf("answered %s/%s#%d — %s\n", projectID, runID, e.ID, e.Question)
		})
}

// inputWrite is the shared body of `add` and `answer`: same argument
// shape, same stdin fallback, same refusal reporting — only the write
// and the confirmation line differ.
func inputWrite(fs *flag.FlagSet, args []string, stdout, stderr io.Writer, verb string,
	write func(root, projectID, runID, text string) (input.Entry, error),
	confirm func(projectID, runID string, e input.Entry) string,
) int {
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 2
	}
	text := strings.Join(fs.Args()[1:], " ")
	if strings.TrimSpace(text) == "" {
		// No text on the line means the prose is on stdin — the phone
		// path's terminal equivalent, and the only way to write more than
		// a sentence without fighting the shell's quoting.
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			moePrintf(stderr, "%s: read stdin: %v\n", verb, err)
			return 1
		}
		text = string(body)
	}
	if strings.TrimSpace(text) == "" {
		moePrintf(stderr, "%s: nothing to write — pass text or pipe it on stdin\n", verb)
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	e, err := write(root, projectID, runID, text)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	moePrintf(stdout, "%s", confirm(projectID, runID, e))
	return 0
}

func runInputList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("input list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe input list [project]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Two lists, oldest first. Questions runs have asked you and you haven't")
		moePrintln(stderr, "answered, then notes already given that no turn has picked up yet.")
		moePrintln(stderr, "Neither holds anything — a run with an unanswered question still runs,")
		moePrintln(stderr, "on the agent's best judgment.")
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
			moePrintf(stderr, "input list: %v\n", err)
			return 1
		}
	}
	waiting, errs := input.Scan(root, projectFilter)
	for _, err := range errs {
		moePrintf(stderr, "input list: %v\n", err)
	}
	open, pending := partitionWaiting(waiting)
	if len(open) == 0 && len(pending) == 0 {
		moePrintln(stdout, "input: nothing waiting")
		return 0
	}
	if len(open) > 0 {
		moePrintln(stdout, "asked you:")
		for _, w := range open {
			moePrintf(stdout, "  %s/%s — %s\n", w.Project, w.Run, w.Entry.Question)
			moePrintf(stdout, "    moe input answer %s/%s <text>\n", w.Project, w.Run)
		}
	}
	if len(pending) > 0 {
		if len(open) > 0 {
			moePrintln(stdout, "")
		}
		moePrintln(stdout, "given, awaiting pickup:")
		for _, w := range pending {
			moePrintf(stdout, "  %s/%s — %s\n", w.Project, w.Run, w.Entry.FirstLine())
		}
	}
	return 0
}

// partitionWaiting splits a Scan into the half that needs the operator
// and the half that needs a turn. Both surfaces that list the record —
// the CLI above and the web queue — want exactly this cut, and both want
// Scan's ordering preserved within each half.
func partitionWaiting(waiting []input.Waiting) (open, pending []input.Waiting) {
	for _, w := range waiting {
		if w.Entry.Open() {
			open = append(open, w)
		} else {
			pending = append(pending, w)
		}
	}
	return open, pending
}

// operatorInputSection is the `## Operator input` block a stage prompt
// carries when the run has anything live: every pending entry verbatim,
// plus an open ping's standing "no reply yet" line.
//
// The returned ids are the pending entries this prompt rendered, which
// runStageSession stamps as delivered after the turn succeeds. An open
// ping contributes no id: it delivers nothing, so it consumes nothing,
// and it keeps reappearing until the operator answers.
//
// An answered ping renders as the question/answer pair rather than the
// answer alone — "B" is cryptic where "asked: which policy? — B" is
// exact.
//
// It is context, not permission. A stage reading the operator's
// direction learns what to build; it does not learn that it may go back
// and rewrite an upstream canvas to match. The block says so, because
// the alternative is a code stage helpfully re-deciding a design.
//
// Returns "" for the overwhelming majority of runs, which have no record
// at all.
func operatorInputSection(root string, md *run.Metadata) (string, []int) {
	f, err := input.Load(root, md.Project, md.ID)
	if err != nil {
		return "", nil
	}
	pending := f.Pending()
	openPing, hasOpen := f.OpenPing()
	if len(pending) == 0 && !hasOpen {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Operator input\n\n")
	ids := make([]int, 0, len(pending))
	if len(pending) > 0 {
		b.WriteString("The operator pushed this at the run. Act on it where it bears on this\n")
		b.WriteString("stage, and fold anything durable into your canvas — the canvas is what\n")
		b.WriteString("downstream stages read, and you are the only turn that receives this.\n")
		b.WriteString("It is direction for the work ahead of you, not licence to rewrite an\n")
		b.WriteString("earlier canvas to match it.\n\n")
		for _, e := range pending {
			ids = append(ids, e.ID)
			if e.IsPing() {
				fmt.Fprintf(&b, "- asked: %s\n  answer: %s\n", e.Question, indentContinuation(e.Text))
				continue
			}
			fmt.Fprintf(&b, "- %s\n", indentContinuation(e.Text))
		}
	}
	if hasOpen {
		if len(pending) > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "This run asked the operator: %s\n", openPing.Question)
		b.WriteString("No reply yet. Proceed on your best judgment and note the call you made\n")
		b.WriteString("on the canvas — nothing is waiting on this.\n")
	}
	return b.String(), ids
}

// indentContinuation keeps a multi-line note readable inside a markdown
// bullet: the first line sits after the dash, the rest line up under it.
func indentContinuation(text string) string {
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n  ")
}

// pendingInputBlock is the pulse kickoff's view of the record: what the
// operator has pushed at runs on this project's board and no turn has
// picked up yet, what those runs have asked and the operator hasn't
// answered, and the bar for asking.
//
// The pending list deliberately carries only each entry's first line.
// The run's own turn receives the entry in full; the survey needs to
// know that the operator is pushing that run — so don't park it
// "awaiting input" — not to act on the content itself.
//
// Board-wide by project, matching every other kickoff block. Empty when
// nothing is live, which is the normal sweep.
func pendingInputBlock(sc *pulseScan, projectID string) string {
	waiting, _ := input.Scan(sc.root, projectID)
	open, pending := partitionWaiting(waiting)
	if len(open) == 0 && len(pending) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Operator input. Prose the operator pushed at a run reaches that run's " +
		"next successful turn, once. It holds nothing and licenses nothing beyond that.\n")
	if len(pending) > 0 {
		b.WriteString("\nWaiting for a turn to pick up — the operator is pushing these runs " +
			"forward, so don't park one \"awaiting input\", and don't relay the text: " +
			"the run's own turn receives it in full.\n")
		for _, w := range pending {
			fmt.Fprintf(&b, "- `%s/%s` — %s\n", w.Project, w.Run, w.Entry.FirstLine())
		}
	}
	if len(open) > 0 {
		b.WriteString("\nAsked and unanswered — do not ask these again, and do not ask a second " +
			"question on the same run. An unanswered question stops nothing: if the answer " +
			"has to precede the work, park the thread and name the question in the reason.\n")
		for _, w := range open {
			fmt.Fprintf(&b, "- `%s/%s` — %s\n", w.Project, w.Run, w.Entry.Question)
		}
	}
	b.WriteString("\nTo ask one, write a thread entry as " +
		"`{\"run\": \"<slug>\", \"ask\": \"…?\"}` — an existing run at its own position, one " +
		"prose question, at most one open per run. Ask only where the operator's prose would " +
		"change what the work builds. If what you need is an operator *act* — close the run, " +
		"tag an idea, change the project's mode — write a plain `park` naming that act " +
		"instead: no answer can discharge it.\n")
	return b.String()
}
