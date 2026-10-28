# Show

Show puts the intent in front of the owner before the code: a draft proposal
whose first commit holds the declarations, and a mockup before any screen
code.

## Open the draft

Publication is the default. Unless the run is local (`--local`, a mandate
that says to stay local, `--dry-run`, or no bound proposals provider), open the
proposal as a draft once the plan is settled, before the first implementation
edit: run `"$PUTNAMI_CLI" change finalize --draft` with the planned title, a
body that states the intended change, and the task reference. Pass
`--partial-delivery` when the proposal delivers one unit of a larger task. The
command adds an empty commit carrying the title when the branch has none; the
squash removes it. A draft starts no hosted run.

## What to show

- A behavior change: the spec increment, with the [spec](../../spec/SKILL.md)
  skill.
- An architecture change: the architecture increment and a small diagram,
  with the [design](../../design/SKILL.md) skill, which also records an ADR
  and runs a bounded prototype when needed.
- A screen change: a mockup as static HTML built with the design system's
  components, captured as images into `.putnami/<issue>/`.

Each is a delta, not a rewrite. It becomes the reference slices, review and
the slice reports measure against; a later change to it is a visible drift.

## Intent first

When the outcome changes a declaration, the reviewer sees the intent before
the code, and a drift costs one declaration edit, not a rewritten change.
A declaration is a `putnami.features.json`, a `specs/*.json`, a
`specs.baseline.json`, a `putnami.architecture.json`, a `decisions.json` or a
decision record (ADR).

1. Write the declarations first, and commit them alone: the commit touches no
   other file. Open the draft with that commit, or push it to the open draft,
   before any implementation commit. It is the one checkpoint pushed before a
   `--projects` gate: the checks it names stay unproven until the code lands.
   State the intent in the body, so the reviewer knows what to approve.
2. Go on with the code without waiting for an approval, unless the launch
   rule of the skill core says code waits.
3. Before each later checkpoint, read the proposal's reviews with
   `proposals status` and what reviewers wrote with `proposals comments`.
   Record in the run directory the refs of the comments you handled and of
   the reviews you published. A comment is new when its ref is not recorded.
   A comment whose `review` is a review you published is your own answer:
   skip it. A comment is on a declaration when its `path` is a declaration,
   or, without a path, when it names a declaration or the stated intent.
   Handle each new comment on a declaration before the next code checkpoint:
   correct the declaration and the code that follows it, or keep it and say
   why. Answer on the proposal with `proposals review`, verdict `comment`, and
   an `idempotencyKey` that names the comment, so a repeat posts no second
   answer. A provider that does not offer `comments` answers `unsupported`:
   report that the run cannot read comments, and read the review verdicts
   alone.
4. An approval names the commit it reviewed: a review with verdict `approve`,
   or a comment that reports the approval of one declaration file at a
   commit. A declaration that changes after its approval goes in its own
   commit, which touches declarations only, so the reviewer sees what changed
   since the approval.
5. Read the reviews and comments once more before the ready finalization,
   and handle a new comment on a declaration first. The proposal turns ready
   for review when the final local gate and the dossier pass. An approval is
   not required for ready.

A settled small correction that changes no declaration keeps the order of the
code stage. A local run keeps the commit order and pushes nothing.
