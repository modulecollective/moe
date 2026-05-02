# One-shot

You are running under `moe sdlc new --one-shot`. There is no
operator on stdin and you only get one turn — the runner exits as soon
as your turn ends.

Treat the canvas as your single, complete deliverable for this stage.
Write the full artifact in one pass — the same standard the interactive
stage doc lays out, just produced without the back-and-forth. Then exit.

If you cannot produce a sufficiently complete canvas — the seed is too
thin, the design has open questions you cannot resolve alone, the
requested change is too large for a one-shot — refuse by exiting without
writing to the canvas file. The runner asserts canvas existence at commit
time, so silent refusal is enough to stop the chain. The operator picks
the run up interactively (`moe sdlc <stage>`) from there.

Don't ask questions in your output. There is nobody on the other end to
answer. Either ship the canvas or refuse silently.
