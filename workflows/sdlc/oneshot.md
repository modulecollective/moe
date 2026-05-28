# One-shot

You are running headless: the `!` / `!<stage>` / `!!!` cascade driver
invoked this stage. There is no operator on stdin and you only get
one turn — the runner exits as soon as your turn ends.

Treat the canvas as your single, complete deliverable for this stage.
Write the full artifact in one pass — the same standard the interactive
stage doc lays out, just produced without the back-and-forth. Then exit.

If you cannot produce a sufficiently complete canvas — the seed is too
thin, the design has open questions you cannot resolve alone, the
requested change is too large for one headless turn, or verification
requires a human surface you cannot drive from tools (a rendered UI,
agent behaviour against real Claude, a prompt change whose only signal
is human-shaped, anything that needs an operator's eyes and nothing
else covers it) — refuse by leaving the canvas at its seeded
placeholders (test stage) or by exiting without writing to the canvas
file (everywhere else). For design and code, the runner asserts canvas
existence at commit time; for test, the stage gate catches the unfilled
skeleton at advance time. Either way, refusal stops the chain. The
operator picks the run up interactively (`moe sdlc <stage>`) from there.

Don't ask questions in your output. There is nobody on the other end to
answer. Either ship the canvas or refuse silently.

A note for the design stage in particular: a baked canvas (a promoted
idea, a reopened run's prior design, an upstream seed) is still a
design turn that needs a canvas edit on success. If the design is
already code-ready, the edit is the `## Design review` note the stage
fragment describes — not a no-op exit. If you can't tell whether it's
code-ready, refuse silently as above; the unchanged-canvas gate stops
the chain so an operator can pick the run up interactively.
