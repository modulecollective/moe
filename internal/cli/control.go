package cli

// `moe control` is the terminal door to a run's control record
// (internal/runcontrol). pause/handoff/park/finish record intent the
// run's owner honours at its next boundary; resume releases a hold for
// a later eligible sweep. None of them starts an agent: the stage verb
// `show` prints is how the operator picks the conversation up now.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
)

func init() {
	g := NewCommandGroup("control", "hold, hand off, park or finish a run at its next boundary")
	for _, c := range []struct {
		kind    runcontrol.Kind
		summary string
	}{
		{runcontrol.KindPause, "hold a run once its current work settles, until `moe control resume`"},
		{runcontrol.KindHandoff, "hold a run for a human to pick its conversation up"},
		{runcontrol.KindPark, "have a run's current owner yield once, holding nothing"},
		{runcontrol.KindFinish, "ship this run (!! scope) from its next boundary, riding no chain"},
	} {
		kind := c.kind
		g.Register(&Command{
			Name:    string(kind),
			Summary: c.summary,
			Run: func(args []string, stdout, stderr io.Writer) int {
				return runControlSubmit(kind, args, stdout, stderr)
			},
			argKind: argProjectRun,
		})
	}
	g.Register(&Command{
		Name:    "resume",
		Summary: "release a run's hold so a later sweep may continue it; starts nothing",
		Run:     runControlResume,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "show",
		Summary: "show a run's owner, hold, requests and conversation command",
		Run:     runControlShow,
		argKind: argProjectRun,
	})
	RegisterGroup(g)
}

func runControlSubmit(kind runcontrol.Kind, args []string, stdout, stderr io.Writer) int {
	verb := "control " + string(kind)
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "request id; a retry with the same id reads back the recorded request")
	fs.Usage = func() { moePrintf(stderr, "usage: moe %s [--id <id>] <project>/<run>\n", verb) }
	root, md, code := controlArgs(fs, args, stderr, verb)
	if code != 0 {
		return code
	}
	if err := controlPolicy(md); err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	req := runcontrol.Request{ID: *id, Kind: kind}
	if req.ID == "" {
		req.ID = runcontrol.NewID()
	}
	r, note, err := deliverControl(root, md, req)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	if note != "" {
		moePrintf(stderr, "%s: %s\n", verb, note)
	}
	key := md.Project + "/" + md.ID
	if r.ID != req.ID {
		moePrintf(stdout, "%s: already satisfied by %s %s — %s\n", key, r.Kind, r.ID, describeRequest(r))
	} else {
		moePrintf(stdout, "%s %s on %s: %s\n", r.Kind, r.ID, key, describeRequest(r))
	}
	if kind == runcontrol.KindHandoff {
		if cmd := conversationCommand(md, r.Stage, r.Agent); cmd != "" {
			moePrintf(stdout, "continue the conversation: %s\n", cmd)
		}
	}
	return 0
}

func runControlResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("control resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "request id; a retry with the same id reads back the recorded resume")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe control resume [--id <id>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Takes the run's owner guard, then clears its hold (or retires an")
		moePrintln(stderr, "unknown-outcome recovery) so a later eligible sweep may continue it.")
		moePrintln(stderr, "Starts nothing. While another invocation owns the run or a session")
		moePrintln(stderr, "is unresolved, the hold stays. To continue the conversation now, run")
		moePrintln(stderr, "the stage command it prints instead — that also clears the hold.")
	}
	root, md, code := controlArgs(fs, args, stderr, "control resume")
	if code != 0 {
		return code
	}
	release, ok := ownRunForMutation(root, md.Project, md.ID, stderr)
	if !ok {
		return 1
	}
	defer release()
	r, released, err := runcontrol.Resume(root, md.Project, md.ID, heldRun(root, md.Project, md.ID),
		runcontrol.Request{ID: *id})
	if err != nil {
		moePrintf(stderr, "control resume: %v\n", err)
		return 1
	}
	key := md.Project + "/" + md.ID
	switch {
	case released:
		moePrintf(stdout, "%s: %s; a later sweep may continue it\n", key, r.Outcome)
	case r.ID != "":
		moePrintf(stdout, "%s: resume %s already recorded — %s\n", key, r.ID, r.Outcome)
	default:
		moePrintf(stdout, "%s was not held\n", key)
	}
	if cmd := conversationCommand(md, "", ""); cmd != "" {
		moePrintf(stdout, "continue now: %s\n", cmd)
	}
	return 0
}

func runControlShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("control show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe control show <project>/<run>") }
	root, md, code := controlArgs(fs, args, stderr, "control show")
	if code != 0 {
		return code
	}
	key := md.Project + "/" + md.ID
	moePrintf(stdout, "%s (%s, %s)\n", key, md.Workflow, md.Status)
	now := time.Now()
	o := ObserveRuns(root, []*run.Metadata{md}, now)[key]
	for _, line := range o.Details(now) {
		moePrintf(stdout, "  %s\n", line)
	}
	if o.ControlErr != nil {
		// Never "not held": an unreadable record refuses motion.
		moePrintf(stdout, "  control: unreadable — %v\n", o.ControlErr)
		return 1
	}
	f := o.Control
	stage, agent := conversationAddress(md, o.Live)
	switch {
	case f.Hold != nil:
		r, _ := f.Find(f.Hold.Request)
		moePrintf(stdout, "  hold: %s %s — %s\n", f.Hold.Kind, f.Hold.Request, holdPhase(r))
		if f.Hold.Stage != "" {
			stage, agent = f.Hold.Stage, f.Hold.Agent
		}
	default:
		moePrintln(stdout, "  hold: none")
	}
	if f.Recovery != nil {
		moePrintf(stdout, "  recovery required: %s (%s)\n", f.Recovery.Reason, f.Recovery.Request)
	}
	if f.Hold != nil || f.Recovery != nil {
		moePrintf(stdout, "  release: moe control resume %s\n", key)
	}
	if cmd := conversationCommand(md, stage, agent); cmd != "" {
		moePrintf(stdout, "  conversation: %s\n", cmd)
	}
	for _, r := range f.Requests {
		moePrintf(stdout, "  %s %s %s — %s\n", r.AcceptedAt.Format("2006-01-02 15:04"), r.Kind, r.ID, describeRequest(r))
	}
	return 0
}

func controlArgs(fs *flag.FlagSet, args []string, stderr io.Writer, verb string) (string, *run.Metadata, int) {
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return "", nil, 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return "", nil, 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", nil, 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return "", nil, 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", nil, 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", nil, 1
	}
	return root, md, 0
}

// describeRequest is one line of what a request is waiting for or what
// became of it.
func describeRequest(r runcontrol.Request) string {
	var s string
	switch r.State {
	case runcontrol.StateAccepted:
		s = "accepted, waiting for the owner's next boundary"
	case runcontrol.StateApplied:
		s = "applied at " + r.Boundary + ", outcome not yet recorded"
	default:
		s = string(r.State)
		if r.Boundary != "" {
			s += " at " + r.Boundary
		}
	}
	if r.Outcome != "" && r.State != runcontrol.StateApplied {
		s += " — " + r.Outcome
	}
	return s
}

// holdPhase distinguishes a hold whose owner is still settling from one
// that is ready to resume. The receipt alone does not prove ownership
// was released; `show` prints the guard alongside it.
func holdPhase(r runcontrol.Request) string {
	if r.Settled() {
		return "held, ready to resume"
	}
	return "held, current turn settling"
}

// conversationAddress is the conversation a human picks up: the stage
// actually running or last run, and its backend — not the next pending
// stage, and not the furthest stage with a session (a review→code
// kickback leaves review's session behind code's newer turn).
//
// In order: the live owner's stage and backend once its agent has
// launched — a stage's first turn has no session in the canonical
// metadata until it settles, but it is the conversation being had; the
// live owner's stage when it already has a conversation (admission or a
// quota wait, before a new agent starts); the run's last settled turn
// (run.Metadata.LastTurn); and, for runs that settled no turn since that
// was recorded, the furthest stage in workflow order with a session.
// When the backend is not known it prefers the run's sticky agent among
// the stage's sessions, and otherwise names none, leaving the stage
// verb's normal resolution.
func conversationAddress(md *run.Metadata, live *runsock.Status) (stage, agent string) {
	if live != nil && live.Stage != "" && live.Agent != "" {
		return live.Stage, live.Agent
	}
	if live != nil && live.Stage != "" {
		if doc := md.Documents[live.Stage]; doc != nil && doc.HasSession() {
			return live.Stage, stageAgent(md, live.Stage)
		}
	}
	if t := md.LastTurn; t != nil && t.Stage != "" {
		return t.Stage, t.Agent
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", ""
	}
	stages := wf.Stages()
	slices.Reverse(stages)
	for _, s := range stages {
		if doc := md.Documents[s]; doc != nil && doc.HasSession() {
			return s, stageAgent(md, s)
		}
	}
	return "", ""
}

// stageAgent is the backend of stage's conversation when the run says
// which: its only session, or the sticky agent among several.
func stageAgent(md *run.Metadata, stage string) string {
	var agents []string
	for a, id := range md.Documents[stage].Sessions {
		if id != "" {
			agents = append(agents, a)
		}
	}
	sort.Strings(agents)
	switch {
	case len(agents) == 1:
		return agents[0]
	case slices.Contains(agents, md.Agent):
		return md.Agent
	}
	return ""
}

// conversationCommand is the stage verb that resumes a conversation
// interactively, or "" when the run has none to resume. stage == ""
// derives the address from the run.
func conversationCommand(md *run.Metadata, stage, agent string) string {
	if md.Status != run.StatusInProgress {
		return ""
	}
	if stage == "" {
		stage, agent = conversationAddress(md, nil)
	}
	if stage == "" {
		return ""
	}
	cmd := fmt.Sprintf("moe %s %s %s/%s", md.Workflow, stage, md.Project, md.ID)
	if agent != "" {
		cmd += " --agent " + agent
	}
	return cmd
}

// controlDialTimeout bounds each exchange with a live owner.
var controlDialTimeout = 3 * time.Second

// deliverControl records req. With a live owner answering, the request
// goes through its endpoint so the receipt is serialised with the
// owner's transitions; a park is bound to that owner's generation, and
// a handoff to the conversation that owner is actually running. The
// owner's refusal is definite. A send whose answer was lost is retried
// directly under the same id: Submit reads the record under the
// repository lock, so it returns the request only once the owner's
// commit has landed, records it if that commit failed, and reports an
// error — never a success — while the outcome is still unknown. With no
// owner answering, a pause or handoff is committed directly and the
// owner (if one exists) reads it at its next boundary; a park, which
// only means something to the owner it addresses, is refused. note
// says which happened when it is not the plain case.
func deliverControl(root string, md *run.Metadata, req runcontrol.Request) (runcontrol.Request, string, error) {
	id := runsock.Identity{Root: root, Project: md.Project, Run: md.ID}
	ctx, cancel := context.WithTimeout(context.Background(), controlDialTimeout)
	defer cancel()
	st, err := runsock.ReadStatus(ctx, runsock.RuntimeDir(), id)
	var live *runsock.Status
	if err == nil {
		live = &st
	}
	if req.Kind == runcontrol.KindHandoff && req.Stage == "" {
		req.Stage, req.Agent = conversationAddress(md, live)
	}
	if live == nil {
		// A park with no generation is refused by Submit unless its id
		// is already recorded, which reads that request back.
		r, serr := runcontrol.Submit(root, md.Project, md.ID, req)
		if serr != nil && req.Kind == runcontrol.KindPark && errors.Is(serr, runcontrol.ErrNoOwner) {
			return runcontrol.Request{}, "", fmt.Errorf("%w; no owner of %s/%s answered (%v)", serr, md.Project, md.ID, err)
		}
		if err := serr; err != nil {
			return runcontrol.Request{}, "", err
		}
		if busy, _ := runowner.Busy(root, md.Project, md.ID); busy && !r.Settled() {
			return r, "saved; owner acknowledgement unavailable — it applies at the owner's next boundary", nil
		}
		return r, "", nil
	}
	gen := ""
	if req.Kind == runcontrol.KindPark && req.Owner == "" {
		// A park already recorded under this id reads back under the
		// lock, whichever owner answers now: it addressed the owner that
		// received it and must neither conflict with nor retarget the
		// successor. Only a genuinely new park is bound to the answering
		// generation.
		r, serr := runcontrol.Submit(root, md.Project, md.ID, req)
		if serr == nil {
			return r, "", nil
		}
		if !errors.Is(serr, runcontrol.ErrNoOwner) {
			return runcontrol.Request{}, "", serr
		}
		gen, req.Owner = st.Generation, st.Generation
	}
	r, _, err := runsock.SendControl(ctx, runsock.RuntimeDir(), id, gen, req)
	if err == nil {
		return r, "", nil
	}
	if _, refused := errors.AsType[*runsock.RefusedError](err); refused {
		return runcontrol.Request{}, "", err
	}
	r, serr := runcontrol.Submit(root, md.Project, md.ID, req)
	if serr != nil {
		return runcontrol.Request{}, "", fmt.Errorf("owner acknowledgement lost (%v) and it could not be confirmed (%v); retry with --id %s", err, serr, req.ID)
	}
	return r, "saved; the owner's acknowledgement was lost — it applies at the owner's next boundary", nil
}
