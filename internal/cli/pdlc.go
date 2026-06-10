package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// The pdlc workflow is the bureaucracy's robo-PM: it turns a fuzzy
// product goal into a PRD and keeps reconciling that PRD against
// reality for as long as the goal lives. The long-lived noun — the
// *plan* — is just a pdlc run that stays open, journaled and resumable
// like chat and idea runs; nothing in the run machinery expires an
// in_progress run, and stage entry never gates on satisfaction.
//
// Three stages, no push:
//
//   - `frame` — conversational shaping: problem, who it's for, in/out
//     of scope, constraints. The canvas is working notes — scaffolding
//     the prd stage compresses, not the durable artifact.
//   - `prd`   — writes the durable PRD canvas under a fixed heading set
//     (Problem / Scope / Out of scope / Shipped–remaining–changed).
//     Re-entered whenever intent changes; revised in place.
//   - `chunk` — the reconcile stage, re-runnable forever. Diffs the PRD
//     against current reality and emits followups for the delta. It
//     never writes ideas directly.
//
// The forward walk offers frame → prd → chunk once; every later
// sitting is an explicit stage verb, operator-invoked. The one
// mechanical novelty is the per-sitting harvest: chunk is the terminal
// stage, so its chain prompt offers the same harvest gesture close
// uses (followups.md in $EDITOR, surviving entries fan out into
// ideas) instead of the close nudge — a plan that stays open for
// months must not strand every sitting's output behind terminal-only
// harvest, and must not close on a reflex Enter either. See
// promptPdlcHarvest.
//
// All three stages get the read-only sandbox treatment (chat/audit
// style): NeedsSandbox gives the agent a clone to read, and
// EnforceSandboxBoundary refuses the turn's cascade if any tracked
// file in the clone changed.

// pdlcWorkflow is the workflow name written to run.json.
const pdlcWorkflow = "pdlc"

// pdlcFrameDoc is the document id for the frame stage. Canvas lives at
// projects/<p>/runs/<r>/documents/frame/content.md. A promoted idea
// (`moe pdlc new --from-idea`) seeds this canvas — the fuzzy body
// lands on frame's working notes rather than fighting the PRD
// skeleton for the same doc.
const pdlcFrameDoc = "frame"

// pdlcPrdDoc is the document id for the prd stage. Canvas lives at
// projects/<p>/runs/<r>/documents/prd/content.md.
const pdlcPrdDoc = "prd"

// pdlcChunkDoc is the document id for the chunk stage. Canvas lives at
// projects/<p>/runs/<r>/documents/chunk/content.md.
const pdlcChunkDoc = "chunk"

func init() {
	g := NewCommandGroup(pdlcWorkflow, "pdlc workflow: new, frame, prd, chunk, close")
	g.Register(newRunCommand(pdlcWorkflow))
	g.Register(&Command{
		Name:    pdlcFrameDoc,
		Summary: "open a session on the plan's framing notes — shape the product goal",
		Run:     runPdlcFrame,
	})
	g.Register(&Command{
		Name:    pdlcPrdDoc,
		Summary: "open a session on the plan's PRD — the durable product artifact",
		Run:     runPdlcPrd,
	})
	g.Register(&Command{
		Name:    pdlcChunkDoc,
		Summary: "open a reconcile session — diff the PRD against reality, emit followups for the delta",
		Run:     runPdlcChunk,
	})
	// pdlc has no workspace, no push, and no moe/<run> branch — its
	// read-only clone is reaped by `moe clone gc` at terminal status,
	// same as chat/audit — so the shared close skeleton rides the
	// standard harvest / state-guard / status-flip path with a nil
	// cleanup. Close means the product goal shipped or died, not that
	// a sitting ended; it harvests stragglers exactly as every other
	// workflow does.
	g.Register(closeCommand(pdlcWorkflow, "Close pdlc run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (pdlc cat <project>/<run> <stage>)",
		Run:     runCat(pdlcWorkflow, ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (pdlc log <project>/<run> <stage>)",
		Run:     runLog(pdlcWorkflow, ""),
	})
	RegisterGroup(g)

	w := NewWorkflow(pdlcWorkflow)
	w.RegisterStage(pdlcFrameDoc)
	w.RegisterStage(pdlcPrdDoc, pdlcFrameDoc)
	w.RegisterStage(pdlcChunkDoc, pdlcPrdDoc)
	RegisterWorkflow(w)
}

// pdlcStageVerb is the shared body behind the three pdlc stage verbs:
// parse the --agent flag, print the per-stage usage, split the run
// reference, and hand to the stage opener. Same job sdlc's
// runSDLCStage does one workflow over, minus the cascade-mode flags —
// pdlc sittings are operator-invoked, and the cascade entry (when an
// operator types `!` at a chain prompt) reaches the openers through
// openPdlcStage instead.
func pdlcStageVerb(verb string, usage []string, open func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s [--agent <name>] <project>/<run>\n", verb)
		moePrintln(stderr, "")
		for _, line := range usage {
			moePrintln(stderr, line)
		}
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
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 2
	}
	return open(projectID, runID, false, *agentOverride, stdout, stderr)
}

func runPdlcFrame(args []string, stdout, stderr io.Writer) int {
	return pdlcStageVerb("pdlc frame", []string{
		"Opens an interactive session on the plan's framing notes: the problem,",
		"who it's for, what's in and out of scope, constraints. Conversational",
		"and resumable across sittings — the canvas is scaffolding the prd",
		"stage compresses, not the durable artifact.",
	}, openPdlcFrame, args, stdout, stderr)
}

func runPdlcPrd(args []string, stdout, stderr io.Writer) int {
	return pdlcStageVerb("pdlc prd", []string{
		"Opens an interactive session on the plan's PRD — the reviewable",
		"product artifact under a fixed heading set (Problem / Scope / Out of",
		"scope / Shipped–remaining–changed). Re-run whenever intent changes;",
		"the canvas is revised in place and the log accumulates across sittings.",
	}, openPdlcPrd, args, stdout, stderr)
}

func runPdlcChunk(args []string, stdout, stderr io.Writer) int {
	return pdlcStageVerb("pdlc chunk", []string{
		"Opens a reconcile session: diff the PRD (stable intent) against",
		"current reality — prior followups, the journal's harvested-idea",
		"lineage, and the project source — and emit followups for the work the",
		"PRD implies that isn't done yet. Re-run after work lands; the first",
		"sitting (everything is delta) is the initial decomposition.",
	}, openPdlcChunk, args, stdout, stderr)
}

// openPdlcFrame is the Go-level seam behind `moe pdlc frame`. The chain
// prompt's cascade driver reaches it through openPdlcStage. Same
// read-only sandbox shape as openAuditPlan: NeedsSandbox so the agent
// can read source while shaping, EnforceSandboxBoundary as the hard
// gate against edits leaking out of a thinking stage.
func openPdlcFrame(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	const kickoff = "The operator just opened a framing session on this plan. Read the " +
		"frame canvas first — it may hold a promoted idea's seed or notes from " +
		"prior sittings. In one or two sentences, acknowledge where the framing " +
		"stands (fresh goal vs. resumed) and ask what they'd like to shape. " +
		"Then wait for their reply."
	return runStageSession(projectID, runID, pdlcFrameDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			InitialPrompt:          kickoff,
			Headless:               headless,
			Agent:                  agentOverride,
		}, stdout, stderr)
}

// openPdlcPrd is the Go-level seam behind `moe pdlc prd`. Same shape as
// openPdlcFrame one stage downstream, plus the canvas skeleton: the
// chunk stage *consumes* the PRD, so a stable heading set is what
// makes the reconcile read mechanical instead of fuzzy.
func openPdlcPrd(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, pdlcPrdDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			Agent:                  agentOverride,
			CanvasSkeleton:         pdlcPrdCanvasSkeleton,
		}, stdout, stderr)
}

// openPdlcChunk is the Go-level seam behind `moe pdlc chunk`. Same
// shape as openPdlcPrd one stage downstream. No skeleton: the chunk
// canvas is the sitting's reconcile narrative, and its real output —
// followups — rides the shared followups.md channel.
func openPdlcChunk(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, pdlcChunkDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			Agent:                  agentOverride,
		}, stdout, stderr)
}

// pdlcPrdCanvasSkeleton is the fixed structural shape the PRD canvas
// opens with. The chunk stage's reconcile read (and any future
// cross-stage consistency judge) keys off these headings, so they are
// load-bearing — the agent fills the sections in place and must not
// rename them. Same mechanism as testCanvasSkeleton in sdlc.go.
const pdlcPrdCanvasSkeleton = `# PRD

## Problem

(agent fills: the product goal — what's broken or missing, for whom, why it matters)

## Scope

(agent fills: what shipping this means — the capabilities the plan commits to)

## Out of scope

(agent fills: what this plan deliberately won't do)

## Shipped / remaining / changed

(agent appends one dated entry per sitting: what landed, what's still open, what changed in intent)
`

// promptPdlcHarvest is the chunk stage's chain prompt. chunk sits at
// the end of the pdlc ladder, so the generic terminal-stage prompt
// would offer close with a Y default — exactly wrong for a plan meant
// to stay open across months of sittings. Instead the natural
// follow-on is harvest: the same gesture close uses (followups.md in
// $EDITOR, surviving unchecked entries fan out into ideas), offered
// per sitting so a never-closing plan doesn't strand every chunk
// sitting's output behind terminal-only harvest.
//
// Prompted, not automatic: declining costs nothing — unchecked entries
// wait for the next sitting or for close — and the prompt is skipped
// silently when there are no unchecked entries, so a sitting that
// emitted nothing exits without ceremony. This per-sitting tailor step
// is also the backlog throttle: nothing reaches the backlog until the
// operator edits the followup text and lets harvest run.
//
// Non-TTY callers get a print-only nudge, same anti-silent-action rule
// the close nudge honours — harvest opens $EDITOR, which a headless
// turn can't host.
func promptPdlcHarvest(root string, md *run.Metadata, stdout, stderr io.Writer) int {
	followupsRel := run.FollowupsPath(md.Project, md.ID)
	if !hasUncheckedEntry(filepath.Join(root, followupsRel)) {
		return 0
	}
	if !stdinIsTerminal() {
		moePrintf(stdout, "follow-ups pending in %s — re-run the chunk stage interactively to harvest, or `moe pdlc close %s/%s` harvests at close.\n",
			followupsRel, md.Project, md.ID)
		return 0
	}
	opts := []promptOption{
		{key: 'Y', hint: "harvest (tailor in $EDITOR, fan out into ideas)"},
		{key: 'n', hint: "decline (entries wait for the next sitting or close)"},
	}
	moePrintf(stdout, "chunk sealed — harvest follow-ups now? %s\n", renderPromptLabel(opts))
	moePrintln(stdout, renderPromptLegend(opts))
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "" && !strings.HasPrefix(answer, "y") {
		return 0
	}
	if err := harvestPdlcSitting(root, md, stdout, stderr); err != nil {
		moePrintf(stderr, "pdlc harvest: %v\n", err)
		return 1
	}
	return 0
}

// harvestPdlcSitting runs the per-sitting harvest under the repo lock:
// the shared harvestFollowups loop (editor pop, idea creation per
// unchecked entry, in-place [x] rewrite) followed by a commit of the
// rewritten followups.md. The status flip stays out — this is a
// harvest call site, not a terminal transition, so the terminal.go
// invariant is untouched and close keeps its own harvest for
// stragglers.
//
// Idempotent the same way close's harvest is: checked entries are
// skipped, and a partial failure mid-batch commits its own progress
// inside harvestFollowups, so a retry picks up where it left off.
// ErrNothingToCommit is tolerated — the operator may delete every
// pending line in the editor, leaving nothing to record.
func harvestPdlcSitting(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	followupsRel := run.FollowupsPath(md.Project, md.ID)
	msg := fmt.Sprintf("harvest: capture follow-ups for %s/%s\n\n", md.Project, md.ID) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
			Document: pdlcChunkDoc,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: "pdlc-harvest",
		Run:     md.Project + "/" + md.ID,
	}, stdout, stderr, func() error {
		if err := harvestFollowups(root, md.Project, md.ID, md.Workflow, false); err != nil {
			return err
		}
		err := run.StageAndCommit(root, msg, followupsRel)
		if errors.Is(err, run.ErrNothingToCommit) {
			return nil
		}
		return err
	})
}
