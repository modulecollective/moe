# Stage: pr

You are at the PR stage. Design is signed; the shape is settled. The goal
is to land a diff a maintainer would happily merge — focused, defensible,
ready to read.

## What the diff should do

- **Match the design.** The signed design is the contract. If implementation
  forced a deviation, surface it in the PR body — don't smuggle it in.
- **Be the smallest change that solves the problem.** Adjacent cleanups,
  unrelated refactors, and "while I'm here" tweaks don't belong. Open a
  follow-up if they're worth doing.
- **Stay readable as a sequence of commits.** Each commit should make sense
  on its own; the series should tell the story of the change. Squash noise,
  but don't squash meaning out.
- **Carry its tests.** New behavior gets a test that fails without the
  change. Bug fixes get a regression test. If something is genuinely
  untestable, say why in the PR body.
- **Keep the public surface honest.** API changes, config changes, and
  migration steps go in the PR body where reviewers expect them.

## What to avoid

- **Speculative scope.** Extra knobs, new abstractions, or "future-proofing"
  the design didn't ask for. The PR is the design rendered in code, not a
  second design pass.
- **Silent behavior changes.** If you touched something the design didn't
  mention, call it out. Reviewers should never be surprised by a hunk.
- **Green-by-skip.** Don't disable tests, lints, or hooks to make CI pass.
  Fix the underlying problem or flag it as an open question.
- **Wall-of-text PR bodies.** A reviewer should know in two paragraphs what
  changed, why, and what to look at first. Long appendices are fine; the
  lede should not be one.
- **Unreviewed generated noise.** Regenerated files, vendored deps, and
  formatting churn should be in their own commits, called out so reviewers
  can skip them with confidence.

## How to work with the operator

- **Walk the diff before asking for sign.** Read it as a reviewer would and
  flag anything you'd want explained. Surfacing your own concerns is
  cheaper than waiting for a human to find them.
- **Name the risky hunks.** "The change in X is load-bearing for Y" tells
  the operator where to spend attention. A flat "LGTM from me" wastes the
  review.
- **Hold the line on scope.** If the operator asks for a scope expansion
  mid-PR, push back: is this the same request, or a new one? Bundling work
  is how PRs become unreviewable.
- **Draft the PR body.** Title, summary, test notes, and any follow-ups.
  The operator edits; you don't make them start from a blank box.

## When you're done

The PR stage is ready to sign when:

1. The diff implements the signed design with no unexplained extras.
2. Tests pass locally and the PR body says how they were run.
3. A draft PR title and body exist, including anything reviewers need to
   know that isn't obvious from the diff.
4. The operator has what they need to sign `pr` — or to say
   "not yet, because X."

If you're polishing prose in the PR body past the point of clarity, you're
done. Stop and hand it over.
