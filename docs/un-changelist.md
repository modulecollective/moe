# The Un-Changelist
*Written by MoE from its own run history.*

A changelist says what a project accumulated. This is the inverse: the ideas
we took seriously enough to build, and then seriously enough to remove.

None of them were absurd. Most were plausible responses to a real pressure:
too many sessions to supervise, too much work to remember, too little evidence
that the agents were following the design. The useful history begins after the
implementation, when actual use showed which part of the idea mattered. What
we kept was usually smaller than what we built.

## We kept almost building a service

The earliest temptation was to turn MoE into a control plane. If one agent was
useful, surely the next step was a lifecycle for many remote agents: dispatch
them, tail them, ask for status, and let the harness supervise the fleet. We
built that surface before we had a real workflow for it. The commands and the
entire managed-agents package disappeared in April 2026 (`de1e5bc`). There was
no production use hiding behind an awkward interface. There was only an
abstraction waiting for a job.

We tried strengthening the local version in the opposite direction too. The
`srt` wrapper put OS-level isolation around the agent subprocess on top of the
per-run source isolation MoE already had. The additional layer did not make the
working boundary easier to reason about; it added a second set of isolation
failures. We reverted it (`8978875`) and kept the boundary we could test:
separate source trees plus scoped tools.

Smaller versions of the service instinct kept returning. `twin-reflect` was a
chore synthesized by the binary for every project with a twin. That sounded
convenient until the operator wanted to tune or remove it. There was no file to
edit, because the entity existed only in Go. We deleted the chore and then the
whole `Builtin` concept. A user-definable thing with an on-disk schema should
not have a privileged sibling that is invisible to the user.

The watching surfaces taught the same lesson from the other side. The web run
page polled for fresh fragments every two seconds. Its “take over” button tried
to resume a session the server did not own, collided with the real session's
lock, and appeared to do nothing. Both went. Later, `moe follow` still emitted
paths and shell assignments for `less +F` and a live diff watcher. Once
workflow-scoped `cat` and transcript rendering existed, we removed `follow`
too. The live-tail and watch-diff loops were not deferred under new names; the
operator had chosen durable canvases and readable history instead
(`remove-moe-follow`).

The lesson was not that automation or live views are forbidden. It was that
motion needs an operator-rooted occasion, and state that survives a sitting is
more valuable than supervision state that exists only while somebody watches.
The current constraint is stated more compactly in the
[README](../README.md): no daemon, no scheduler, no swarm. MoE works best as a
tool one operator invokes, with artifacts that are still there when they come
back.

## A workflow does not create an occasion

MoE's cheapest extension point is a workflow: name a job, give it a stage
ladder, write a few markdown prompts. That made workflows easy to add before
we knew whether anyone would reach for them.

`quick` was a `code → push` lane for changes supposedly too small to deserve a
design. In practice it created a second class of code work, with its own verb,
prompt, tests, close path, and exceptions from improvements made to `sdlc`.
Small fixes now take the normal route. The design canvas can be short; the
workflow does not need to be different.

`audit` was more ambitious: plan a project inspection, then report its
findings. Chat sessions were already reading projects and filing followups,
twin observations, and lore. Audit gave that act another noun without giving
the operator another reason to do it. We deleted the workflow and let the
habit keep its existing home.

`meta-moe` had an even larger footprint for a similar act. It scanned run
history, generated a maintainer report, published a snapshot, registered with
the cascade machinery, and carried its own prompt and tests. The report did
not change what the operator did. The history it summarized was already
queryable, and the questions worth asking of it changed faster than the
published format.

The most considered version was `pdlc`, a perpetual robo-PM with
`frame → prd → chunk`. It was not killed merely because it was young. It was
killed because its exact occasion arrived and the operator routed around it.
At deletion there had been one pdlc run against 629 sdlc runs. The product goal
that pdlc was meant to hold was still active, but the work moved through ideas,
chat, and sdlc instead (`scuttle-pdlc`).

Usage is evidence only when read with the occasion. A feature unused because
its job never came up may deserve patience. A feature bypassed while its job is
happening has already run the experiment. What survived these workflows was a
smaller vocabulary tied to real habits, plus markdown guidance cheap enough to
reshape when the habit changes.

## A queue was the wrong answer twice

The first `moe queue` was a runner. It persisted a list of pending runs and
ideas, promoted ideas at dispatch time, walked the list, popped completed
items under a repository lock, marked queued work in the dashboard, and put a
three-second Ctrl-C window between jobs. We polished it four times: status,
stopping, prompt behavior, and a dry-loop mistake. The operator still did not
reach for it.

That mattered because almost everything the queue did already had a direct
verb. An sdlc run could resume on its own. An idea could be promoted eagerly.
The stage prompt could cascade one run to its next gate. The subsystem's one
unique capability was batch-grinding several opened runs in sequence. When
that capability saw no use, “simplify the queue” would have preserved the
walker and its maintenance cost while making authoring worse. “Make it more
automatic” would have amplified the part nobody had asked for. We deleted the
verb and package together (`55fed60`).

Queue returned later, but this time as a holding pen. A pulse could find a
batch of work, mint a placeholder head, and park the proposed runs behind it.
Nothing executed until the operator kicked the batch. This queue was useful,
but the noun was wrong: the placeholder and its children were already a
chain. Splitting “queue” from “chain” produced two command surfaces for one
graph and made it hard to say whether appending work changed a list or an
active ride.

Renaming exposed the better primitive. Order is a relationship between runs,
not membership in a container. A head can carry an optional purpose note, but
the edges are the work. Once that was explicit, a chain could start at an
ordinary run, several topical heads could coexist, and the same recorded order
could drive the dashboard and the cascade. The container had disappeared;
what remained was the relation the consumers actually read
(`second-guessing-queue`).

Then we deleted the pulse report's `## Pull next` section. It ranked work in
prose, but no mechanism consumed the ranking. Grooming now records actual
order rather than asking a human to reproduce it in another surface
(`robo-grooming`). The two queues were different implementations and died for
different reasons: one was an unused runner, the other a misleading name for
persisted ordering. Their shared lesson is that a list is not leverage until
we can name what consumes it.

## Synthetic memory usually became another thing to maintain

Durable memory is MoE's central bet, which made synthetic memory especially
seductive. If canvases were useful, perhaps every run also needed a current
abstract. We added a post-turn agent call to regenerate one synchronously. It
made every stage command feel hung after the useful work had finished. Worse,
a source search found no reader of the value: no dashboard, search command, or
discovery path used it. We considered background refreshes, push-time
refreshes, hashing, and batching. Then we deleted the writer because there was
no consumer (`866ad93`).

The digital twin's managed `roadmap` was a larger version of the same hope. It
had a fixed place in the twin, its own reflect stage, a five-section
convention, and a ritual that folded the open idea backlog into it. The
operator did not find the document useful. Intent over time needed human
authorship, not an agent-managed summary with prescribed headings, so roadmap
left the closed schema and nothing replaced it (`drop-roadmap-2026-05-29`).
The later `intent` workflow is a useful contrast: the operator authors an
intent, agents consume it, and nobody pretends it was inferred.

`meta-moe` belongs here as well as among the extra workflows. Its snapshot was
memory about memory, another artifact to refresh beside the corpus it
summarized. Deleting it did not delete the journal. It removed a standing
answer to questions that were better asked directly of the source.

The distinction that survived is a named reader and a clear owner. A canvas
hands a decision to the next stage. The twin steers later runs. Lore is loaded
when its applicability matches. An intent is written by the operator and read
by agents. Those artifacts earn maintenance because somebody depends on them.
The per-turn abstract, folded roadmap, and maintainer snapshot were produced
because they might be useful.

[Evolution](evolution.md) is not another attempt at synthetic memory. It is
the companion forward story: a bounded essay written from the durable record,
not a field or managed document the product promises to keep current.

## Trust did not come from adding another judge

Some removals began as safety features. `moe twin claim` let an operator
legitimize edits made directly to the managed twin documents. It seemed like a
practical escape hatch. In use it was confusing: a decided change was supposed
to travel through reflection, while claim blessed a parallel path after the
fact. We removed the verb but kept the detection that made it possible. The
guard now refuses unrecorded edits and directs the change back through the
workflow. The bypass died; the boundary survived
(`drop-twin-claim-2026-06-12`).

`moe eval` was the purest attempt at detached oversight. A pinned LLM judge
compared a merged run's design with its code diff and wrote specific findings
for the operator to triage. Its design did something we still admire: it
pre-registered its own kill criterion. The first phase would survive only if
at least half of judged runs produced a real, previously unnoticed deviation.

We backfilled twenty merged runs. Seven produced findings before triage, and
some findings were hallucinations caused by truncated diffs. No possible
triage outcome could reach the threshold. We honored the criterion the first
time it fired and deleted the verb, rubric, trailers, and special-case storage
(`1ab1950`). The experiment said something encouraging about the staged
process: design-to-code drift was rare and minor. It also said the extra judge
did not earn a permanent seat.

That is narrower than “judgment cannot be automated.” MoE asks agents to
review work constantly. The lesson is that a report about a completed
transition does not repair the transition. Guidance belongs where the agent
acts. Deterministic checks belong where state changes. Review and test can
send work back; push hooks and fast-forward rules can block a bad merge;
reflection can repair recorded intent. A detached score must prove that it
changes decisions, and eval did not.

Deletion is therefore part of the architecture, not an admission that the
architecture failed. Git makes a removed implementation cheap to recover, so
we do not need compatibility shims, dormant flags, or “revive if missed” notes
to soothe every cut. When evidence turns against a feature, we can remove it
cleanly and keep the principle it paid to teach. The code was temporary. The
scar tissue is the durable artifact.
