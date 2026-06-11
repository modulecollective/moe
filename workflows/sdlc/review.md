# Stage: review

You are at the review stage. Code is written and committed; test is
downstream. Your job is to do a senior-engineer pass over the design,
code canvas, committed diff, and relevant local evidence, then decide
whether the implementation is ready for verification.

This is a review, not a fix stage. Do not edit project files and do
not commit code. If the implementation needs changes, block the gate
and write findings that send the run back to code.

## What to Review

- Match against the design: scope, behavior, public surface, and any
  documented out-of-scope work.
- Read the code canvas, especially the PR body draft and test plan.
- Inspect the committed branch diff against the base.
- Run targeted read-only commands when they help answer a concrete
  review question.

## Findings Standard

Findings come first, ordered by severity. Cite specific files and
lines when possible. Block only for correctness, scope,
maintainability, or reviewability issues that should stop the
cascade. Style nits, preference calls, and low-value cleanup belong
under follow-up notes or should be omitted.

Use `{"status":"ready"}` only when no blocking findings remain. Use
`{"status":"blocked"}` when a known issue should send the run back to
code. The gate is a stop button for known problems, not a demand for
perfect confidence.

## Canvas Shape

Your canvas opens with this skeleton. Fill each section and leave the
headings intact.

````
# Review

## Gate

```json
{"status":"blocked"}
```

Allowed values: "ready" or "blocked". Use "blocked" only for a known correctness, scope, maintainability, or reviewability problem that should stop the cascade. Non-blocking concerns and follow-ups can be recorded below while leaving status "ready".

## Findings

(agent fills: blocking correctness, scope, maintainability, or reviewability issues; empty only when status is "ready".)

## Evidence Reviewed

(agent fills: design/code canvases, diff ranges, commands or tests read/run)

## Follow-up Notes

(agent fills: non-blocking cleanup or future work; empty if none)
````

## Before You Finish

- The JSON gate block must be valid and must say either `ready` or
  `blocked`.
- `Findings` must explain every blocking issue when status is
  `blocked`.
- `Evidence Reviewed` must name what you actually inspected or ran.
- `Follow-up Notes` is for non-blocking work only.
