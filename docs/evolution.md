# Evolution

A memo on how MoE got from one supervised agent to work that finds itself,
orders itself, and starts itself — and on what we kept human along the way.

MoE's first commit is dated 2026-04-12. Its bureaucracy — the journal that
holds every run, canvas, and recorded decision — starts nine days later. Three
months on, the `moe` project alone carries north of a thousand runs of one kind
or another. This is the story of what happened in between, told against a
ladder we only noticed we were on in July.

## The ladder we found halfway up

In mid-July a table called "Steps of AI Adoption" landed on the desk. Five
rungs, numbered 0 through 4, each pinned to how many agents are in flight and,
more usefully, to what the bottleneck is:

- **0 — Gated** (0 agents). Access is process-heavy, only older models are
  approved, nothing an agent produces has a path to production. The bottleneck
  is legacy approval and cost-per-token thinking.
- **1 — Assisted** (~1 agent). You and one agent as a supervised pair. You
  review everything. The bottleneck is your attention, and the work is
  synchronous.
- **2 — Parallel** (~10 agents). One engineer orchestrates five to ten agents
  on separate worktrees; the agent checks its own work; you review final diffs.
  The bottleneck is reviewing six streams of output and steering them.
- **3 — Supervised autonomy** (~100 agents). The agent writes nearly all the
  code, "did you read the code?" becomes "what context was the model missing?",
  and maintenance runs continuously in the background. The bottleneck is trust
  in the loop and decision throughput.
- **4 — AI-native** (~1,000+ agents). The loop is closed, most agents are
  kicked off by other agents, you steer by intent and monitor by exception.

We keep the source's numbering here, step 0 included, even though MoE was never
on that rung — inventing a parallel scheme would have cost the table's one real
gift, which is not the aspiration but the bottleneck column. Every rung names
the thing that stops working when you try to climb. Read backwards, that column
is a to-do list.

At the time we read it, MoE sat squarely at step 2. What follows is how it got
there, and what it cost to leave.

## Step 1: the session was the unit

The original problem was not code quality. Agents were already good at code.
The problem was that running several agent threads meant chat-history
archaeology: the design lived in one scrollback, the test evidence in another,
and all of it died when the session did.

So MoE's first bet was that the *artifact*, not the conversation, is the unit
of work. Every stage of a run writes a short canvas — a compressed statement of
what was decided, readable by the next stage without replaying the chat. Every
turn lands as one commit on the bureaucracy's `main` branch, with trailers that
scope the journal by run, document, and workflow. Git is the checkpoint:
rewinding a bad turn is `git reset --soft`, undoing a landed one is
`git revert`, and there is no database that knows the history better than the
log does.

Everything MoE became later is downstream of that. A machine can only act on
its own history if the history is legible, and a canvas is legible in a way a
transcript is not.

The second bet, made at the same time, was that agent behavior belongs in
markdown rather than in Go. `soul.md` sets the operating philosophy; a stage
fragment sets the lens for the current stage; the twin records project intent.
When an agent gets something wrong, the fix is an edit to the file it already
reads. That rule is why the later steps were cheap: teaching the harness a new
kind of judgment mostly meant writing better prose.

## Step 2: the bang

Step 1's bottleneck is your attention, and the path off it is running several
agents at once with a self-verification loop you trust.

Parallelism came first, and structurally: every stage that touches source runs
inside a per-run sandbox clone — `git clone --local --shared --no-checkout`
against the project submodule, one per run. Two runs on the same project never
share a working tree, index, or refs. That includes stages that only *read*;
a stage asked to check a claim against the source gets a clone rather than
reasoning from memory. Named workspaces cover the case where build cache or a
warm dev server needs to outlive a run.

Self-verification came as the sdlc ladder: design → code → review → test →
push. `review` is a senior-engineer pass with bounded latitude to fix in place;
`test` exercises the change; `push` is fast-forward-only behind the project's
own hook scripts. Nothing merges that didn't walk all five.

And then the lever that made the whole thing pay: the bang. At the end of every
stage MoE prints a chain prompt, and what you type there decides how far the
run travels without you. `!` runs the next stage and parks. `!<stage>` runs to
a named gate. `!!` ships this run. `!!!` ships it and rides on into the next
chained run. Same vocabulary as flags on the stage verbs, for the same reason.

That is where the economics turned. Shape a few runs during the day, chain them
into a sequence, fire `!!!` once on your way out. The chain codes, reviews,
tests, and ships overnight on capacity a flat-rate subscription already pays
for, and each run is still gated, journaled, and revertible in the morning.

Which brings you to step 2's bottleneck, exactly as advertised: reviewing the
output, and — the part the table doesn't say out loud — deciding what should
have been in the chain at all. MoE could execute a queue faster than one human
could fill it thoughtfully.

## Step 3: the pulse

All of MoE's backlog inflow was operator-authored (ideas, chat sessions) or
incidental (followups captured while a run was busy doing something else).
Nothing in the system went *looking* for work. And as inflow grew, the backlog
itself became the bottleneck: nothing helped decide what to pull next.

We had tried to fix that twice before. `audit` was a plan-then-report workflow.
`pdlc` was a robo-PM. Both died to "chat sittings filing followups", and both
post-mortems named the same two gaps: no *occasion* — nothing fired them, so
you had to remember — and no *headless* mode, so a survey cost a synchronous
sitting. A chore you have to remember to do is not a solution to having too
many chores.

The pulse solved the occasion problem by not being a thing you invoke. It fires
at the *tail* of run traffic — sdlc close, sdlc push, twin close. The occasion
is "work just landed," which is exactly when there is something new to notice.
Nothing fires on a clock, then or now.

A pulse is a read-only headless sweep of one project: recent journal, drift
between twin and code, the open backlog, plus context the harness computes and
hands over because the agent can't cheaply derive it — pending twin
observations, recently settled runs, live chain state, and a GitHub block
covering merged PRs and default-branch CI, since work that lands outside moe is
invisible to a journal-only sweep. The sandbox is read-only on both legs. The
agent writes a report and a fenced-JSON gate; the harness reads the gate and
acts.

The gate is where the interesting part accumulated. It started as one channel —
`reflect.due`, minting a twin-reflect run when the recorded canon had drifted
far enough behind. Then it grew `spawn`: a list of proposed fix runs, each of
which the harness mints as a *parked* sdlc run with its design canvas seeded.
The bar for a spawn entry is taught in the stage fragment rather than enforced
in code — mechanical, bounded, verifiable — because the harness has no basis
for judging which entries to trim, and the parked queue is itself the review
gate.

That crossed a line vision.md had drawn, and the design said so rather than
smuggling it. The line was redrawn as: **the pulse makes runs, but never runs
them.** Execution stayed operator-rooted.

The rhythm this produced is worth stating plainly, because it's the whole
argument for the design: across 2026-07-17 to 07-19, sixty-six pulse sweeps
fired on the `moe` project. None of them were scheduled. Each one rode the tail
of work a human had already chosen to land.

The gate's two channels have since collapsed into one. `reflect.due` was
deleted and a spawn entry grew a `workflow` field (`sdlc` by default, `twin`
for a reflect), which took the reflect's harness-fixed placement rule with it:
where a reflect lands is now an ordering claim the agent makes in `chain`,
priced against the same bar as everything else it queues. One grammar, one
placement mechanism, one consent gate — and the guards that matter (one twin
run in flight, no reflect over unrecorded twin edits) stayed exactly where they
were, in the mint. What changed later is only what the pulse does when the
first guard fires: a pulse-side ask for a reflect is a nomination, so an
already-open pass is mapped onto rather than refused, and the gate never has to
know which case it is.

## Step 4: the fourth bang

Which left the last gap. The pulse could find work and open it, but the runs it
opened sat parked, and it wrote a prose paragraph called `## Pull next` ranking
the backlog for a human to read.

Nobody read it. That paragraph is now deleted, and the replacement is the
sharpest lesson in this memo: **a ranking nothing consumes is not a ranking.**
It was replaced by lane order the pulse actually stamps — real
`MoE-Chained-To` edges on real runs, which the dash, the chain editor, and the
ride machinery all read from one source.

So the pulse learned to groom. A gate channel names where spawned runs should
*go*: chained onto an existing run, spliced mid-thread, gathered under a named
head, or self-rooted. The primitive is not "make a chain" but "chain after an
existing item" — heads became an optional naming convenience rather than a
container. Sprawl is not a problem to prevent, because the groomer is the
merge: stray threads are exactly what a later pulse consolidates by moving one.

And then the last door: a sweep's final act can be to kick the thread it just
groomed. Work can now find itself, order itself, and start itself.

That is a real change in kind, and it needed a consent surface that wasn't a
review screen — a gate that needs a human doesn't survive contact with a
cascade nobody is watching. The answer was a fourth bang. `!!!` rides a chain
**statically**: the machine may not grow the ride, and groom placements landing
inside the ridden unit get redirected out. `!!!!` rides **dynamically**, and
that is the sole license for the machine to extend a ride mid-flight and for a
tail pulse to kick its own thread.

It is a consent notch, not a distance notch — the two set the same ride flag
and differ only in the mode the invocation carries. The reasoning was blunt:
an operator who kicks a three-run chain expecting three runs should not wake up
to twelve merges. The grant is typed, process-wide, and dies with the process.

## What carries the weight

Four structural guards sit on the self-kick, and each is
impossible-by-construction rather than restraint-by-good-manners:

The ride must be **dynamic** — a fourth bang a human typed upstream. A plain
push, a `!!`, a `!!!` all groom and park. The surprise ride is not a thing the
machine promises to avoid; it's a thing it cannot do.

The spawner must be **unchained**, which is a re-entrancy guard: a ride already
picks up its own tail growth, so nested rides can't stack.

And the thread must be **machine-rooted**. The pulse curates operator chains
but never starts them. That trigger stays where it was.

The first three are per-hop, and a kicked ride's own tail push satisfies them
afresh — it grooms, spawns, and kicks again, without bound. So the fourth is a
depth bound: the run whose push fired the pulse must itself be
**operator-opened**. One machine generation per fourth bang. Each `!!!!`
licenses the machine to start something once; letting generation N license
generation N+1 is the runaway, and a fix whose push flags more fixes is worth
an operator glance anyway. The deferred work parks rather than dying — a parked
thread is exactly what the next consented pulse consolidates.

Underneath those, nothing that made step 2 safe was traded away. Nothing fires
on a clock. The survey sandbox is read-only; the agent proposes JSON and the
harness stamps it. Every ridden run still walks review and test. Every merge is
still fast-forward behind the push hooks. Parked chains never move on their
own — appending to a parked chain is curation, not execution. And the retreat
is built in: stop typing the fourth bang.

## What we're still finding out

There is deliberately no source filter on grooming. An operator-parked run can
be groomed into a machine-rooted thread and ride a confident self-kick with no
human look in between. That was argued both ways inside a single session and
settled toward empowering the agent, and the design canvas records the
supersession rather than quietly rewriting itself, because that consequence
*is* the bet.

The bars that decide all of this — is this work mechanical and bounded enough
to spawn, is this order settled enough to kick — are prose in a stage fragment,
not thresholds in Go. That is on purpose, and it is also the open question. The
only way to learn whether those bars are trustworthy is to let them root real
rides and watch what merges.

So that's where MoE is: somewhere on step 4's lower slopes, with the loop
closed and the trigger still typed. The standing direction from here is that
over time the pulses chain work we kick off nearly unchanged — and that the
agent decides more and more of its own work. Every unchanged kick is evidence.
Every one that needed editing first is a fragment that needs a better sentence.

The bottleneck, as the table promised, has moved again. It is no longer
attention, or review, or the backlog. It's judgment about what's worth doing —
and that is a much better problem to have.
