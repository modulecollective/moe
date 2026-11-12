package cli

// `moe ui`: the terminal console. internal/ui owns the screen; this file
// supplies what is on it (uisource.go) and does what the operator asks
// through the same writers the CLI and web use — input.Add and Answer,
// deliverControl and runcontrol.Resume, runopen's capture writers, and
// the optional Rex reveal (uirex.go). Nothing here launches an agent.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/ui"
)

func init() {
	Register(&Command{
		Name:    "ui",
		Summary: "terminal console: follow runs live, send notes and controls, edit ideas and intents",
		Run:     runUI,
	})
}

func runUI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "show only rows whose run belongs to this project")
	workflow := fs.String("workflow", "", "show only rows whose run uses this workflow")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe ui [--project <id>] [--workflow <name>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "The dashboard, every completed run included, with the selected item's")
		moePrintln(stderr, "details below it. Notes, answers and run controls go through the same")
		moePrintln(stderr, "writers as `moe input` and `moe control`; ideas and intents are edited")
		moePrintln(stderr, "as plain text. Press ? inside for keys and commands.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	out, ok := stdout.(*os.File)
	if !ok || !cliout.IsTTY(stdout) {
		moePrintln(stderr, "ui: needs a terminal on stdout")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	filter := DashFilter{ProjectFilter: *project, WorkflowFilter: *workflow}

	src := newUISource(root, filter, out)
	// The first gather fails fast, as dash does: a typo'd filter or a
	// broken bureaucracy is an error, not an empty console.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := src.refresh(ctx); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	srcDone := make(chan struct{})
	go func() { src.run(ctx); close(srcDone) }()
	defer func() { stop(); <-srcDone }()

	status := dashStatus{}
	if cliout.RexStatusEnabled(out) {
		status.w = out
	}
	cfg := ui.Config{
		Boards: src.boards,
		Do:     func(a ui.Action) ui.Result { return uiDo(ctx, root, a) },
		Report: func(w io.Writer, _ ui.Board) error { return status.update(src.currentReports()) },
		Unreport: func(io.Writer) {
			status.close()
		},
	}
	if err := ui.Run(ctx, cfg, out, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// uiDo makes one mutation through the existing writer for it. It runs on
// the console's single worker.
func uiDo(ctx context.Context, root string, a ui.Action) ui.Result {
	switch a.Kind {
	case ui.ActNote:
		return uiNote(root, a)
	case ui.ActAnswer:
		return uiAnswer(root, a)
	case ui.ActControl:
		if a.Control == "resume" {
			return uiResume(root, a)
		}
		return uiControl(root, a)
	case ui.ActCreate:
		return uiCreate(root, a)
	case ui.ActEdit:
		return uiEdit(root, a)
	case ui.ActClose:
		return uiClose(root, a)
	case ui.ActTag:
		return uiTag(root, a)
	case ui.ActReveal:
		return revealTerminal(ctx, root, a)
	}
	return ui.Result{Message: "unknown action"}
}

// definite reports an error that means nothing was written: a refusal
// before any write, or a failed commit that was rolled back. Anything
// else leaves the outcome unknown.
func definite(err error) bool {
	_, timeout := errors.AsType[*repolock.TimeoutError](err)
	_, nested := errors.AsType[*repolock.NestedError](err)
	_, reserved := errors.AsType[*session.UnresolvedError](err)
	_, refused := errors.AsType[*runsock.RefusedError](err)
	if timeout || nested || reserved || refused {
		return true
	}
	for _, e := range []error{
		run.ErrRunNotFound, run.ErrSlugTaken,
		input.ErrEmpty, input.ErrNotLive, input.ErrStalePing, input.ErrNoOpenPing, input.ErrMalformed,
		runopen.ErrNotCapture, runopen.ErrCaptureChanged, runopen.ErrNotTaggableIdea,
		runcontrol.ErrMalformed, runcontrol.ErrNotLive, runcontrol.ErrBadKind, runcontrol.ErrConflict,
		runcontrol.ErrHeld, runcontrol.ErrRecovery, runcontrol.ErrNoOwner, runcontrol.ErrNotOwner, runcontrol.ErrStale,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return strings.Contains(err.Error(), "nothing recorded")
}

func failure(err error, readback func() []string) ui.Result {
	if definite(err) {
		return ui.Result{Message: oneLine(err.Error())}
	}
	r := ui.Result{Uncertain: true, Message: "outcome unknown: " + oneLine(err.Error())}
	if readback != nil {
		r.Lines = readback()
	}
	return r
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// inputReadback is the evidence for an input write whose outcome is
// unknown: whether the record now holds this text.
func inputReadback(root string, a ui.Action) func() []string {
	return func() []string {
		f, err := input.Load(root, a.Project, a.Run)
		if err != nil {
			return []string{"readback: input record unreadable: " + err.Error()}
		}
		for _, e := range slices.Backward(f.Notes) {
			if e.Text == strings.TrimSpace(a.Text) || e.Text == a.Text {
				return []string{fmt.Sprintf("readback: the record holds #%d with this text — check `git log` before sending it again", e.ID)}
			}
		}
		return []string{"readback: the record holds no entry with this text"}
	}
}

func uiNote(root string, a ui.Action) ui.Result {
	e, err := input.Add(root, a.Project, a.Run, a.Text)
	if err != nil {
		return failure(err, inputReadback(root, a))
	}
	return ui.Result{OK: true, Message: fmt.Sprintf("note #%d saved for %s/%s — it reaches the run's next prompt", e.ID, a.Project, a.Run)}
}

func uiAnswer(root string, a ui.Action) ui.Result {
	e, err := input.Answer(root, a.Project, a.Run, a.QuestionID, a.Text)
	if err != nil {
		return failure(err, inputReadback(root, a))
	}
	return ui.Result{OK: true, Message: fmt.Sprintf("answered #%d on %s/%s", e.ID, a.Project, a.Run)}
}

// uiControl is `moe control <kind>` for one run: delivered through the
// owner's endpoint when it answers, else recorded directly, then read
// back. A park is bound to the owner generation the console showed. The
// request id survives an unknown outcome, so a retry reads back.
func uiControl(root string, a ui.Action) ui.Result {
	md, err := run.Load(root, a.Project, a.Run)
	if err != nil {
		return failure(err, nil)
	}
	if err := controlPolicy(md); err != nil {
		return ui.Result{Message: err.Error()}
	}
	req := runcontrol.Request{ID: a.RequestID, Kind: runcontrol.Kind(a.Control)}
	if req.ID == "" {
		req.ID = runcontrol.NewID()
	}
	if req.Kind == runcontrol.KindPark {
		req.Owner = a.Generation
	}
	r, note, err := deliverControl(root, md, req)
	if err != nil {
		res := failure(err, nil)
		res.RequestID = req.ID
		if res.Uncertain {
			res.Lines = append(res.Lines, "the same command retries request "+req.ID+" and reads it back")
		}
		return res
	}
	rb, err := readBack(root, md, r, note)
	if err != nil {
		return ui.Result{Uncertain: true, Message: oneLine(err.Error()), RequestID: req.ID}
	}
	lines := []string{fmt.Sprintf("%s %s: %s", rb.req.Kind, rb.req.ID, rb.desc)}
	if rb.note != "" {
		lines = append(lines, rb.note)
	}
	if req.Kind == runcontrol.KindHandoff {
		if cmd := conversationCommand(md, rb.req.Stage, rb.req.Agent); cmd != "" {
			lines = append(lines, "continue the conversation: "+cmd)
		}
	}
	msg := fmt.Sprintf("%s %s/%s: %s", rb.req.Kind, a.Project, a.Run, rb.req.State)
	if rb.req.ID != req.ID {
		msg = fmt.Sprintf("%s/%s: already satisfied by %s %s", a.Project, a.Run, rb.req.Kind, rb.req.ID)
	}
	return ui.Result{OK: true, Message: msg, Lines: lines, RequestID: req.ID}
}

// uiResume is `moe control resume`: take the guard, release the hold
// shown (its revision), start nothing.
func uiResume(root string, a ui.Action) ui.Result {
	md, err := run.Load(root, a.Project, a.Run)
	if err != nil {
		return failure(err, nil)
	}
	if err := controlPolicy(md); err != nil {
		return ui.Result{Message: err.Error()}
	}
	var guard bytes.Buffer
	release, ok := ownRunForMutation(root, md.Project, md.ID, &guard)
	if !ok {
		return ui.Result{Message: "resume: " + oneLine(guard.String())}
	}
	defer release()
	id := a.RequestID
	if id == "" {
		id = runcontrol.NewID()
	}
	r, released, err := runcontrol.Resume(root, md.Project, md.ID, heldRun(root, md.Project, md.ID),
		runcontrol.Request{ID: id, Revision: a.HoldRevision})
	if err != nil {
		res := failure(err, nil)
		res.RequestID = id
		return res
	}
	var lines []string
	if cmd := conversationCommand(md, "", ""); cmd != "" {
		lines = append(lines, "continue now: "+cmd)
	}
	switch {
	case released:
		return ui.Result{OK: true, RequestID: id, Message: fmt.Sprintf("%s/%s: %s; a later sweep may continue it", a.Project, a.Run, r.Outcome), Lines: lines}
	case r.ID != "":
		return ui.Result{OK: true, RequestID: id, Message: fmt.Sprintf("%s/%s: resume %s already recorded — %s", a.Project, a.Run, r.ID, r.Outcome), Lines: lines}
	}
	return ui.Result{OK: true, RequestID: id, Message: a.Project + "/" + a.Run + " was not held", Lines: lines}
}

// uiCreate opens a new idea or intent with the operator's text as its
// canvas. Ideas start untagged.
func uiCreate(root string, a ui.Action) ui.Result {
	if a.Workflow != dash.IdeaWorkflow && a.Workflow != dash.IntentWorkflow {
		return ui.Result{Message: "create: not a capture workflow: " + a.Workflow}
	}
	if err := requireProject(root, a.Project); err != nil {
		return ui.Result{Message: oneLine(err.Error())}
	}
	doc, _ := dash.CaptureDocID(a.Workflow)
	_, err := runopen.Open(root, a.Project, run.Options{
		ID:       a.Run,
		Workflow: a.Workflow,
		SeedDocs: map[string]string{doc: a.Text},
	})
	if err != nil {
		return failure(err, func() []string { return createReadback(root, a) })
	}
	return ui.Result{OK: true, Message: fmt.Sprintf("opened %s %s/%s", a.Workflow, a.Project, a.Run)}
}

// createReadback says whether the run exists on disk and in the journal.
func createReadback(root string, a ui.Action) []string {
	rel := filepath.Join(run.Dir(a.Project, a.Run), "run.json")
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		return []string{"readback: no run.json was written"}
	}
	if out, err := git.Output(root, "log", "-1", "--format=%h", "--", rel); err == nil && strings.TrimSpace(out) != "" {
		return []string{"readback: the run is committed (" + strings.TrimSpace(out) + ")"}
	}
	return []string{"readback: " + rel + " exists but is not committed; inspect the tree before creating it again"}
}

func uiEdit(root string, a ui.Action) ui.Result {
	err := runopen.EditCaptureChecked(root, a.Project, a.Run, a.Expected, a.Text)
	switch {
	case err == nil:
		return ui.Result{OK: true, Message: "saved " + a.Project + "/" + a.Run}
	case errors.Is(err, run.ErrNothingToCommit):
		return ui.Result{OK: true, Message: "unchanged; nothing committed"}
	case errors.Is(err, runopen.ErrCaptureChanged):
		return ui.Result{Changed: true, Message: "the canvas changed since you opened it; nothing saved — Ctrl-X discards your draft, then e opens the current text"}
	}
	return failure(err, nil)
}

func uiClose(root string, a ui.Action) ui.Result {
	if err := runopen.CloseCapture(root, a.Project, a.Run); err != nil {
		return failure(err, nil)
	}
	return ui.Result{OK: true, Message: "closed " + a.Project + "/" + a.Run + "; its history stays"}
}

// uiTag sets or clears an idea's workflow licence. It permits a later
// sweep to pick the idea up; it starts nothing.
func uiTag(root string, a ui.Action) ui.Result {
	if a.Workflow != "" {
		if err := validatePromoteTag(a.Workflow); err != nil {
			return ui.Result{Message: "tag: " + err.Error()}
		}
	}
	err := runopen.TagIdea(root, a.Project, a.Run, a.Workflow, a.DesignOnly)
	if err != nil && !errors.Is(err, run.ErrNothingToCommit) {
		return failure(err, nil)
	}
	what := "untagged " + a.Project + "/" + a.Run
	if a.Workflow != "" {
		what = "tagged " + a.Project + "/" + a.Run + " " + promoteTagLabel(a.Workflow, a.DesignOnly) + "; a later sweep may pick it up"
	}
	if errors.Is(err, run.ErrNothingToCommit) {
		what += " (already so)"
	}
	return ui.Result{OK: true, Message: what}
}
