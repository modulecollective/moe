package cli

import (
	"flag"
	"io"
)

// The review workflow is the bureaucracy's "checkup" verb. A run reads
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

// reviewWorkflow is the workflow name written to run.json.
const reviewWorkflow = "review"

// reviewPlanDoc is the document id for the plan stage. Canvas lives
// at projects/<p>/runs/<r>/documents/plan/content.md.
const reviewPlanDoc = "plan"

// reviewReportDoc is the document id for the report stage. Canvas
// lives at projects/<p>/runs/<r>/documents/report/content.md.
const reviewReportDoc = "report"

func init() {
	g := NewCommandGroup(reviewWorkflow, "review workflow: new, plan, report")
	g.Register(newRunCommand(reviewWorkflow))
	g.Register(&Command{
		Name:    reviewPlanDoc,
		Summary: "open a Claude Code session on the run's plan canvas — pick what this review pass should cover",
		Run:     runReviewPlan,
	})
	g.Register(&Command{
		Name:    reviewReportDoc,
		Summary: "open a Claude Code session on the run's report canvas — do the actual review and file feedback",
		Run:     runReviewReport,
	})
	// Review has no workspace and no moe/<run> branch, so the shared
	// close skeleton has nothing workflow-specific to clean up — pass
	// nil and ride the standard harvest / state-guard / status-flip
	// path. The followups / lore / twin-feedback channels populated by
	// the report stage all harvest through the same shared paths sdlc
	// and kb runs use.
	g.Register(closeCommand(reviewWorkflow, "Close review run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (review cat <project>/<run> <stage>)",
		Run:     runCat(reviewWorkflow, ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (review log <project>/<run> <stage>)",
		Run:     runLog(reviewWorkflow, ""),
	})
	RegisterGroup(g)

	w := NewWorkflow(reviewWorkflow)
	w.RegisterStage(reviewPlanDoc)
	w.RegisterStage(reviewReportDoc, reviewPlanDoc)
	RegisterWorkflow(w)
}

func runReviewPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe review plan <project>/<run>")
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
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "review plan: %v\n", err)
		return 2
	}
	return openReviewPlan(projectID, runID, false, false, stdout, stderr)
}

// openReviewPlan is the Go-level seam behind `moe review plan`. The
// chain prompt's cascade driver (`!` / `!<stage>` / `!!`) reaches it
// through openReviewStage. NeedsSandbox is true so the agent can
// verify a path the operator names in passing exists; the
// design-stage's tracked-change guard is re-used to enforce the
// read-only character of the stage.
func openReviewPlan(projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
	if headless {
		return runStageSession(projectID, runID, reviewPlanDoc,
			stageSessionOpts{
				NeedsSandbox:           true,
				EnforceSandboxBoundary: true,
				Headless:               true,
				SkipNextStage:          suppressNextStage,
				CanvasSkeleton:         reviewPlanCanvasSkeleton,
			}, stdout, stderr)
	}
	const kickoff = "The operator just opened this review plan session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one sentence, acknowledge where the plan stands " +
		"(fresh vs. resumed). Then ask the one question: what should this review " +
		"cover? Wait for their reply, then write the answer to the Scope section."
	return runStageSession(projectID, runID, reviewPlanDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			InitialPrompt:          kickoff,
			SkipNextStage:          suppressNextStage,
			CanvasSkeleton:         reviewPlanCanvasSkeleton,
		}, stdout, stderr)
}

func runReviewReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe review report <project>/<run>")
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
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "review report: %v\n", err)
		return 2
	}
	return openReviewReport(projectID, runID, false, false, stdout, stderr)
}

// openReviewReport is the Go-level seam behind `moe review report`.
// Same shape as openReviewPlan one stage downstream. The report stage
// has no requirePriorCanvas check: the plan stage is optional by design
// (an empty `## Scope` falls through to "everything" at read-time),
// so the report stage opens against whatever the plan left — even a
// fresh run that skipped plan entirely.
func openReviewReport(projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
	if headless {
		return runStageSession(projectID, runID, reviewReportDoc,
			stageSessionOpts{
				NeedsSandbox:           true,
				EnforceSandboxBoundary: true,
				Headless:               true,
				SkipNextStage:          suppressNextStage,
				CanvasSkeleton:         reviewReportCanvasSkeleton,
			}, stdout, stderr)
	}
	const kickoff = "The operator just opened this review report session. " +
		"Read the plan canvas at documents/plan/content.md first — it names the " +
		"scope this pass should cover (an empty or missing Scope collapses to " +
		"'everything in this project'). Then read the report canvas to see " +
		"whether this is a fresh start or a resume. In one or two sentences, " +
		"acknowledge the scope you read off the plan and ask the operator " +
		"whether to begin the review or refine the scope first. Then wait for " +
		"their reply."
	return runStageSession(projectID, runID, reviewReportDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			InitialPrompt:          kickoff,
			SkipNextStage:          suppressNextStage,
			CanvasSkeleton:         reviewReportCanvasSkeleton,
		}, stdout, stderr)
}

// reviewPlanCanvasSkeleton is the fixed structural shape the plan
// canvas opens with. The agent fills the sections in place; an
// untouched skeleton on commit refuses through the standard canvas-
// unchanged-from-kickoff gate. Same shape as testCanvasSkeleton in
// sdlc.go.
const reviewPlanCanvasSkeleton = `# Plan

## Scope

(agent fills: one paragraph naming what the report stage will read. "everything" is a valid answer.)

## Themes

(optional — concerns to weight extra. Empty is fine.)

## Out of scope

(optional — things this pass deliberately ignores. Empty is fine.)
`

// reviewReportCanvasSkeleton is the fixed structural shape the report
// canvas opens with. The `## Counts` self-tally is the budget surface
// in `moe review cat` — fixed section so the operator sees the numbers
// at a glance. See workflows/review/report.md for the framing.
const reviewReportCanvasSkeleton = `# Review

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
