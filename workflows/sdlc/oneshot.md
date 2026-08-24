# One-shot

You are running headless: the `!` / `!<stage>` / `!!!` cascade driver
invoked this stage. There is no operator on stdin and you only get
one turn — the runner exits as soon as your turn ends.

Treat the canvas as your single, complete deliverable for this stage.
Write the full artifact in one pass — the same standard the interactive
stage doc lays out, just produced without the back-and-forth. Then exit.

There are two ways a turn can end without the artifact, and they are
not the same judgement. Pick the one that matches.

**"This run shouldn't proceed at all."** The idea is already done, the
premise is falsified by the code, the smallest correct diff is no diff.
That's a conclusion, not a shortfall — so write it down. Write a real
canvas: what you checked, what you found, why the run is moot. End it
with the gate section:

    ## Gate

    ```json
    {"status":"close"}
    ```

Then exit normally. The turn commits like any other stage turn, and the
harness closes the run — your reasoning is the durable record, and the
run stops being re-offered on every sweep. Write a `## Gate` section
only when you are nominating the close; don't quote the fence as an
example elsewhere in the canvas. If the operator disagrees, `moe sdlc
reopen` mints a successor seeded from your canvas — so a close is
recoverable, but it is still terminal until someone acts. Nominate it
only when you are sure.

**"I can't do this alone."** The seed is too thin, the design has open
questions you cannot resolve, the change is too large for one headless
turn, or verification requires a human surface you cannot drive from
tools (a rendered UI, agent behaviour against real Claude, a prompt
change whose only signal is human-shaped, anything that needs an
operator's eyes and nothing else covers it). Refuse silently: leave the
canvas at its seeded placeholders (test stage) or exit without writing
to the canvas file (everywhere else). For design and code, the runner
asserts canvas existence at commit time; for test, the stage gate
catches the unfilled skeleton at advance time. Either way, refusal
stops the chain and the run parks. The operator picks it up
interactively (`moe sdlc <stage>`) from there.

Don't ask questions in your output. There is nobody on the other end to
answer. Either ship the canvas, nominate the close, or refuse silently.

A note for the design stage in particular: a baked canvas (a promoted
idea, a reopened run's prior design, an upstream seed) is still a
design turn that needs a canvas edit on success. If the design is
already code-ready, the edit is the `## Design review` note the stage
fragment describes — not a no-op exit. If you can't tell whether it's
code-ready, refuse silently as above; the unchanged-canvas gate stops
the chain so an operator can pick the run up interactively.
