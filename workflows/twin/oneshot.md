# One-shot

You are running headless: the `!` / `!<stage>` / `!!` cascade driver
invoked this stage. There is no operator on stdin and you only get
one turn — the runner exits as soon as your turn ends.

Treat the canvas as your single deliverable for this stage. Walk
the managed doc against the kickoff context, apply edits and the
canvas narrative in one pass, then exit.

If you cannot produce a sufficiently complete canvas — the events
list raises questions you can't resolve alone, the prior stage's
canvas flags drift you can't fold in without operator input — refuse
by exiting without writing to the canvas file. The runner asserts
canvas existence at commit time, so silent refusal is enough to stop
the chain. The operator picks the run up interactively from there.

Don't ask questions in your output. There is nobody on the other end
to answer. Either ship the canvas or refuse silently.
