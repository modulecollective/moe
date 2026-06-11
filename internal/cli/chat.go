package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

// The chat workflow is the bureaucracy's "just think about a project"
// surface. A run opens an interactive session prepped with the same
// project context the work stages get — the per-run sandbox clone for
// reading source, the digital twin, prior canvases — but framed as a
// thinking partner rather than an implementer. The agent answers
// questions, reasons about decisions, and grooms the backlog (opening
// ideas on the operator's behalf); it never edits project code and
// never drives the coding ladder.
//
// One stage, no push:
//
//   - `chat` — the interactive session itself.
//
// The artifact is the transcript, read back with `moe chat log`. There
// is no agent-written summary (the operator has historically backed
// those out). The canvas is a moe-owned session log: a header plus one
// `Session N — opened …` marker moe appends on each open. That marker
// is also what makes the canvas differ from main every turn, so
// session.Close's canvas-unchanged guard passes without an opt-out —
// the chat agent writes nothing to the canvas itself (see
// chatCanvasOnOpen and stageSessionOpts.CanvasOnOpen).
//
// Read-only by contract, the same way audit is: NeedsSandbox gives the
// agent a clone to read, and EnforceSandboxBoundary refuses the turn's
// cascade if any tracked file in the clone changed — the hard gate
// against source edits leaking out of a "just thinking" session.

// chatWorkflow is the workflow name written to run.json. Aliased from
// dash so the string lives in one place — dash also needs it to
// recognise chat runs on the home screen (see dash.ChatWorkflow).
const chatWorkflow = dash.ChatWorkflow

// chatDoc is the document id for the single chat stage. Canvas lives at
// projects/<p>/runs/<r>/documents/chat/content.md. The stage verb and
// the workflow share the name, so the resume invocation reads
// `moe chat chat <project>/<run>`. Aliased from dash.ChatDocID.
const chatDoc = dash.ChatDocID

// chatCanvasHeader is the fixed preamble moe writes the first time a
// chat canvas is opened. The session markers append below the
// `## Sessions` heading. One %s — the project id. No line in the
// preamble may start with "Session " or nextChatSessionNum miscounts.
const chatCanvasHeader = `# Chat: %s

Thinking-partner session — the conversation is the record, read it back
with the workflow's log verb. moe writes one marker per session below;
the chat agent does not edit this canvas.

## Sessions

`

func init() {
	g := NewCommandGroup(chatWorkflow, "chat workflow")
	g.Register(newRunCommand(chatWorkflow))
	g.Register(&Command{
		Name:    chatDoc,
		Summary: "open (or resume) an interactive thinking-partner session on the project",
		Run:     runChat,
	})
	// chat has no workspace, no push, and no moe/<run> branch worth a
	// bespoke teardown (its read-only clone is reaped by `moe clone gc`
	// at terminal status, same as audit), so the shared close skeleton
	// rides the standard harvest / state-guard / status-flip path with a
	// nil cleanup.
	g.Register(closeCommand(chatWorkflow, "Close chat run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump the chat canvas (moe-written session log) to stdout",
		Run:     runCat(chatWorkflow, chatDoc),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render the chat session transcript (chat log <project>/<run>)",
		Run:     runLog(chatWorkflow, chatDoc),
	})
	RegisterGroup(g)

	w := NewWorkflow(chatWorkflow)
	w.RegisterStage(chatDoc)
	w.SetPerpetual()
	RegisterWorkflow(w)
}

func runChat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chat chat [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens (or resumes) an interactive thinking-partner session on the")
		moePrintln(stderr, "project: ask questions, reason about decisions, groom the backlog.")
		moePrintln(stderr, "The agent reads source through a per-run sandbox clone but never")
		moePrintln(stderr, "edits it and never drives coding. The run stays open across")
		moePrintln(stderr, "sessions — re-run to continue the same thread, or `moe chat close`")
		moePrintln(stderr, "when the thread is done.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chat: %v\n", err)
		return 2
	}
	return openChat(projectID, runID, *agentOverride, stdout, stderr)
}

// openChat is the Go-level seam behind `moe chat chat`. Same shape as
// openAuditPlan — NeedsSandbox + EnforceSandboxBoundary make it a
// read-only sandbox session — with two chat-specific knobs:
//
//   - CanvasOnOpen appends the per-session marker so the moe-owned
//     canvas moves every turn (the agent never writes it).
//   - SkipNextStage is always on. chat is a single terminal stage, so
//     the only post-turn prompt available is the close nudge, whose
//     Y-default would close-on-Enter — exactly wrong for a run meant to
//     be reopened. Suppressing it drops the operator back to the shell;
//     they reopen to continue or `moe chat close` to finish.
//
// chat is interactive-only: there is no headless parameter and no
// oneshot.md fragment. The cascade dispatcher is still registered (see
// chat_stages.go) for surface uniformity, but it routes here and always
// opens an interactive REPL.
//
// A closed run is reopened-and-continued in one step before the session
// opens (reopenClosedChat): close is a soft archive for chat, and
// re-entering is the reopen — there is no separate `moe chat reopen`
// verb.
func openChat(projectID, runID, agentOverride string, stdout, stderr io.Writer) int {
	if code := reopenClosedChat(projectID, runID, stdout, stderr); code != 0 {
		return code
	}
	const kickoff = "The operator just opened a chat session about this project — you are " +
		"their thinking partner here, not an implementer. Read the chat canvas first " +
		"(it's a moe-written session log; you don't edit it), then orient yourself with " +
		"the digital twin and source as needed. You answer questions about the project, " +
		"reason through decisions, and groom the backlog (open or refine ideas via the " +
		"moe-howto skill) when asked — you do not write or drive code. Greet the operator " +
		"in one line and ask what they'd like to think through. Then wait for their reply."
	return runStageSession(projectID, runID, chatDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			InitialPrompt:          kickoff,
			SkipNextStage:          true,
			Agent:                  agentOverride,
			CanvasOnOpen: func(workRoot string, md *run.Metadata) error {
				return chatCanvasOnOpen(workRoot, md, resolveAgentName(agentOverride, md.Agent))
			},
		}, stdout, stderr)
}

// reopenClosedChat flips a closed chat run back to in_progress before
// the session opens, so re-entering a closed thread reopens-and-continues
// in one step — close is a soft archive, not a one-way door. The flip is
// scoped to the chat verb's own entry (here) rather than runStageSession,
// which must not silently revive other workflows on stage re-entry.
//
// chat never pushes, so closed is the only terminal status reachable: an
// in-progress run falls straight through to a plain resume, a closed run
// is flipped (via the shared runopen.Reopen) and announced, and any other
// status refuses loud rather than guessing. A non-zero return is a hard
// failure (root/load/flip error or unexpected status); 0 means "carry on
// and open the session". The transcript lives in the bureaucracy and
// survives close, and NeedsSandbox re-mints the read-only clone if
// `moe clone gc` reaped it, so the conversation continues either way.
func reopenClosedChat(projectID, runID string, stdout, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "chat: %v\n", err)
		return 1
	}
	switch md.Status {
	case run.StatusInProgress:
		return 0
	case run.StatusClosed:
		if err := runopen.Reopen(root, md, stdout, stderr); err != nil {
			moePrintf(stderr, "chat: reopen %s/%s: %v\n", projectID, runID, err)
			return 1
		}
		moePrintf(stdout, "reopened %s/%s\n", projectID, runID)
		return 0
	default:
		moePrintf(stderr, "chat: %s/%s has unexpected status %q; cannot resume\n", projectID, runID, md.Status)
		return 1
	}
}

// chatGroomingHome points the chat agent's MOE_HOME at the canonical
// bureaucracy root so backlog grooming (`moe idea new` / `edit`) lands
// on the operator's live backlog: committed to real `main`, visible to
// any other window at once, and writable — devEnvWritableDirs keys the
// agent's --add-dir set off MOE_HOME, so this one assignment both aims
// grooming at the real bureaucracy and adds it to the writable scope.
//
// Setting it last also overrides any project dev-env hook that
// redirected MOE_HOME to a scratch bureaucracy — the moe-on-moe
// silent-scratch trap, where an in-session capture used to succeed into
// a throwaway world and vanish on teardown. root is the canonical root
// moe already resolved; the scratch redirect only ever lived in the
// agent subprocess's env, never in moe's own.
//
// Mutates and returns devEnv (creating it when nil); non-chat workflows
// get devEnv back untouched.
func chatGroomingHome(workflow string, devEnv map[string]string, root string) map[string]string {
	if workflow != chatWorkflow {
		return devEnv
	}
	if devEnv == nil {
		devEnv = map[string]string{}
	}
	devEnv[bureaucracy.EnvHome] = root
	return devEnv
}

// chatCanvasOnOpen seeds (turn one) or appends to (every resume) the
// chat canvas with a `Session N — opened <ts>, agent <name>` marker.
// moe owns this canvas wholesale; the chat agent never writes it, so
// this append is the only thing that moves the canvas off main between
// sessions — which is what lets session.Close's canvas-unchanged guard
// pass on a resume turn where the agent only talked.
//
// agentName is the resolved backend for this turn, threaded so the
// marker honestly records which agent drove each session.
func chatCanvasOnOpen(workRoot string, md *run.Metadata, agentName string) error {
	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, chatDoc))
	body, err := os.ReadFile(canvasAbs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("chat: read canvas: %w", err)
	}
	out := string(body)
	if out == "" {
		out = fmt.Sprintf(chatCanvasHeader, md.Project)
	}
	marker := fmt.Sprintf("Session %d — opened %s, agent %s\n",
		nextChatSessionNum(out), time.Now().UTC().Format(time.RFC3339), agentName)
	if err := os.WriteFile(canvasAbs, []byte(out+marker), 0o644); err != nil {
		return fmt.Errorf("chat: write canvas: %w", err)
	}
	return nil
}

// nextChatSessionNum returns the 1-based number for the marker about to
// be appended, by counting existing `Session …` lines. moe owns the
// whole canvas, so a line-prefix count is reliable — the header
// preamble is written to keep any prose off a "Session "-prefixed line.
func nextChatSessionNum(canvas string) int {
	n := 0
	for _, line := range strings.Split(canvas, "\n") {
		if strings.HasPrefix(line, "Session ") {
			n++
		}
	}
	return n + 1
}
