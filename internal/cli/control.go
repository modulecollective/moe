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
	if kind == runcontrol.KindHandoff {
		req.Stage, req.Agent = conversationAddress(md)
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
		if cmd := conversationCommand(md, req.Stage, req.Agent); cmd != "" {
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
	moePrintf(stdout, "  owner: %s\n", ownerLine(root, md))
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		// Never "not held": an unreadable record refuses motion.
		moePrintf(stdout, "  control: unreadable — %v\n", err)
		return 1
	}
	stage, agent := conversationAddress(md)
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

// ownerLine reports the run guard. Live phase and activity come from
// the owner's endpoint, which this does not consult.
func ownerLine(root string, md *run.Metadata) string {
	busy, err := runowner.Busy(root, md.Project, md.ID)
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case busy:
		return "owned by a running moe invocation"
	}
	return "none"
}

// conversationAddress is the last stage that actually ran and the
// backend that ran it — not the next pending stage. With several
// backends on that stage it prefers the run's sticky agent, and
// otherwise names none, leaving the stage verb's normal resolution.
func conversationAddress(md *run.Metadata) (stage, agent string) {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return "", ""
	}
	stages := wf.Stages()
	slices.Reverse(stages)
	for _, s := range stages {
		doc := md.Documents[s]
		if doc == nil || !doc.HasSession() {
			continue
		}
		var agents []string
		for a, id := range doc.Sessions {
			if id != "" {
				agents = append(agents, a)
			}
		}
		sort.Strings(agents)
		switch {
		case len(agents) == 1:
			return s, agents[0]
		case slices.Contains(agents, md.Agent):
			return s, md.Agent
		}
		return s, ""
	}
	return "", ""
}

// conversationCommand is the stage verb that resumes a conversation
// interactively, or "" when the run has none to resume. stage == ""
// derives the address from the run.
func conversationCommand(md *run.Metadata, stage, agent string) string {
	if md.Status != run.StatusInProgress {
		return ""
	}
	if stage == "" {
		stage, agent = conversationAddress(md)
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
// owner's transitions; a park is bound to that owner's generation. The
// owner's refusal is definite. A send whose answer was lost is read back
// by id, and if it never landed is committed directly under the same id
// — never resent under a new one. With no owner answering, the request
// is committed directly and the owner (if one exists) reads it at its
// next boundary. note says which happened when it is not the plain case.
func deliverControl(root string, md *run.Metadata, req runcontrol.Request) (runcontrol.Request, string, error) {
	id := runsock.Identity{Root: root, Project: md.Project, Run: md.ID}
	ctx, cancel := context.WithTimeout(context.Background(), controlDialTimeout)
	defer cancel()
	st, err := runsock.ReadStatus(ctx, runsock.RuntimeDir(), id)
	if err == nil {
		gen := ""
		if req.Kind == runcontrol.KindPark {
			gen, req.Owner = st.Generation, st.Generation
		}
		r, _, err := runsock.SendControl(ctx, runsock.RuntimeDir(), id, gen, req)
		if err == nil {
			return r, "", nil
		}
		if _, refused := errors.AsType[*runsock.RefusedError](err); refused {
			return runcontrol.Request{}, "", err
		}
		f, lerr := runcontrol.Load(root, md.Project, md.ID)
		if lerr != nil {
			return runcontrol.Request{}, "", fmt.Errorf("owner acknowledgement lost (%v) and readback failed (%v); retry with --id %s", err, lerr, req.ID)
		}
		if r, ok := f.Find(req.ID); ok {
			return r, "saved; the owner's acknowledgement was lost", nil
		}
	}
	r, err := runcontrol.Submit(root, md.Project, md.ID, req)
	if err != nil {
		return runcontrol.Request{}, "", err
	}
	if busy, _ := runowner.Busy(root, md.Project, md.ID); busy && !r.Settled() {
		return r, "saved; owner acknowledgement unavailable — it applies at the owner's next boundary", nil
	}
	return r, "", nil
}
