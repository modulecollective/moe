package cli

import (
	"flag"
	"io"

	"github.com/modulecollective/moe/internal/agent"
)

// The audit workflow is the bureaucracy's "checkup" verb. A run reads
// one project — its code, canvases, and digital twin — and produces
// structured feedback in the channels every other run uses: a canvas
// report, followups for the next sdlc run, twin observations for the
// next reflect, and lore entries for portable facts. No push: the
// artifact is the notes themselves.
//
// Two stages with no push:
//
//   - `plan`   — name what this pass covers (Scope / Themes / Out of scope)
//   - `report` — do the actual review, writing the canvas and filing
//     entries to the trace channels
//
// The harvest path at `close` matches every other workflow: the shared
// closeCommand harvester opens followups.md in $EDITOR and promotes
// surviving entries to ideas; feedback/lore.md and feedback/twin.md
// ride the standard close paths (lore promotion, next-twin-reflect
// pickup). Both stages run in a sandbox clone with the design-stage's
// tracked-change guard re-used to enforce the read-only character —
// the report stage's job is to write notes, not edit project code.

// auditWorkflow is the workflow name written to run.json.
const auditWorkflow = "audit"

// auditPlanDoc is the document id for the plan stage. Canvas lives
// at projects/<p>/runs/<r>/documents/plan/content.md.
const auditPlanDoc = "plan"

// auditReportDoc is the document id for the report stage. Canvas
// lives at projects/<p>/runs/<r>/documents/report/content.md.
const auditReportDoc = "report"

func init() {
	g := NewCommandGroup(auditWorkflow, "audit workflow: new, plan, report")
	g.Register(newRunCommand(auditWorkflow))
	g.Register(&Command{
		Name:    auditPlanDoc,
		Summary: "open a Claude Code session on the run's plan canvas — pick what this review pass should cover",
		Run:     runAuditPlan,
	})
	g.Register(&Command{
		Name:    auditReportDoc,
		Summary: "open a Claude Code session on the run's report canvas — do the actual review and file feedback",
		Run:     runAuditReport,
	})
	// Audit has no workspace and no moe/<run> branch, so the shared
	// close skeleton has nothing workflow-specific to clean up — pass
	// nil and ride the standard harvest / state-guard / status-flip
	// path. The followups / lore / twin-feedback channels populated by
	// the report stage all harvest through the same shared paths sdlc
	// and kb runs use.
	g.Register(closeCommand(auditWorkflow, "Close audit run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (audit cat <project>/<run> <stage>)",
		Run:     runCat(auditWorkflow, ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (audit log <project>/<run> <stage>)",
		Run:     runLog(auditWorkflow, ""),
	})
	RegisterGroup(g)

	w := NewWorkflow(auditWorkflow)
	w.RegisterStage(auditPlanDoc)
	w.RegisterStage(auditReportDoc, auditPlanDoc)
	RegisterWorkflow(w)
}

func runAuditPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe audit plan [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the plan canvas.")
		moePrintln(stderr, "The agent asks the operator what this review pass should cover")
		moePrintln(stderr, "(Scope / Themes / Out of scope) and writes the answer; headless")
		moePrintln(stderr, "default falls through to 'Scope: everything'. The report stage")
		moePrintln(stderr, "reads this canvas to know what to read.")
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
		moePrintf(stderr, "audit plan: %v\n", err)
		return 2
	}
	return openAuditPlan(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

// openAuditPlan is the Go-level seam behind `moe audit plan`. The
// chain prompt's cascade driver (`!` / `!<stage>` / `!!` / `!!!`) reaches it
// through openAuditStage. NeedsSandbox is true so the agent can
// verify a path the operator names in passing exists; the
// design-stage's tracked-change guard is re-used to enforce the
// read-only character of the stage.
func openAuditPlan(projectID, runID string, headless, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, auditPlanDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			SkipNextStage:          suppressNextStage,
			Agent:                  agentOverride,
			CanvasSkeleton:         auditPlanCanvasSkeleton,
		}, stdout, stderr)
}

func runAuditReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe audit report [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the report canvas.")
		moePrintln(stderr, "The agent reads the project — code, canvases, digital twin — under")
		moePrintln(stderr, "the scope named in the plan canvas, files concerns to followups.md /")
		moePrintln(stderr, "feedback/twin.md / feedback/lore.md, and writes a ranked summary on")
		moePrintln(stderr, "the canvas. No push: the artifact is the filed feedback.")
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
		moePrintf(stderr, "audit report: %v\n", err)
		return 2
	}
	return openAuditReport(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

// openAuditReport is the Go-level seam behind `moe audit report`.
// Same shape as openAuditPlan one stage downstream. The report stage
// has no requirePriorCanvas check: the plan stage is optional by design
// (an empty `## Scope` falls through to "everything" at read-time),
// so the report stage opens against whatever the plan left — even a
// fresh run that skipped plan entirely.
func openAuditReport(projectID, runID string, headless, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, auditReportDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			SkipNextStage:          suppressNextStage,
			Agent:                  agentOverride,
			CanvasSkeleton:         auditReportCanvasSkeleton,
		}, stdout, stderr)
}

// auditPlanCanvasSkeleton is the fixed structural shape the plan
// canvas opens with. The agent fills the sections in place; an
// untouched skeleton on commit refuses through the standard canvas-
// unchanged-from-kickoff gate. Same shape as testCanvasSkeleton in
// sdlc.go.
const auditPlanCanvasSkeleton = `# Plan

## Scope

(agent fills: one paragraph naming what the report stage will read. "everything" is a valid answer.)

## Themes

(optional — concerns to weight extra. Empty is fine.)

## Out of scope

(optional — things this pass deliberately ignores. Empty is fine.)
`

// auditReportCanvasSkeleton is the fixed structural shape the report
// canvas opens with. The `## Counts` self-tally is the budget surface
// in `moe audit cat` — fixed section so the operator sees the numbers
// at a glance. See workflows/audit/report.md for the framing.
const auditReportCanvasSkeleton = `# Audit

## Summary

(agent fills: 2–3 sentences — what was reviewed and the bottom line)

## What's working

(agent fills: 1–5 bullets. Absence here usually means the reviewer didn't read enough.)

## Concerns

(agent fills: ranked by load-bearing-ness. Each entry: one paragraph + a pointer to the followup / twin / lore entry it filed, if any.)

## Counts

followups: 0
lore: 0
twin: 0
`
