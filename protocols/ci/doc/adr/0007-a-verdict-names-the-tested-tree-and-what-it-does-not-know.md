# ADR 0007 — a verdict names the tested tree and what it does not know

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`)
- **Amends**: [ADR 0006](0006-a-run-carries-one-verdict.md)

## Context

ADR 0006 defined the verdict before any producer wrote one. Writing the first
producer showed two things a producer could not say truthfully.

**The tree.** ADR 0006 put the head commit and its tree in the subject. The
first shape of version 1 also recorded the tested commit, only when it was not
the head. A CI plane that tests a pull request merged into its base tests a
tree that is not the head's tree. That tree is what the answer is about: every cache key and every
artifact digest of the run is a hash of it. The commit that carries it is
often one the runner builds. It is on no branch, and nobody can fetch it. So
the verdict held a tree the run did not test and a commit ID nobody can
resolve.

**What the producer does not know.** Every list had to be present, and the
validator derived the gaps from the lists. A producer may not hold the impact
of a run, the requirement states of a project, or the files the change
modifies. It could then only write an empty list or zero counts. An empty
impact with no declaration is a `verified` verdict. Zero counts are a
`no-requirement` gap, which says the project states no requirement. Both are
claims about the change that the producer cannot back.

No consumer reads a verdict yet, so version 1 changes in place.

## Decision

1. **The tree is the tree the run tested.** `change.tree` holds it.
   `change.testedCheckout` is required and says what the tree is the tree of:
   `head` when the run tested the head as it is, `merge` when it tested the
   head merged into `change.base`. A `merge` names its base.
2. **The subject holds the head commit only.** Its digest is `gitCommit`. An
   in-toto digest set names one artifact, and the tested tree is not always
   the tree of the head commit.
3. **A producer says what it does not know with a gap, never with an empty
   list.**
   - `unknown-impact`: it does not know which projects the change reaches.
     `impact.projects` is then empty.
   - `unknown-requirements`, with a project: it does not know how that
     project's requirements stand. The project then carries no `requirements`
     counts.
   - `unknown-declarations`: it does not know which declarations the change
     modifies. `declarations` is then empty.
4. **The validator checks what it can.** It derives `unknown-requirements`
   from the projects without counts, the way it derives the other project
   gaps. Only the producer knows whether its impact or its declarations are
   unknown, so the validator does not derive those two gaps. It refuses a list
   that holds an entry beside the gap that says the list is unknown.
5. **The sums skip a project without counts.** `requirements.projectsUnknown`
   counts those projects.
6. **`weakening` is what the producer found.** It is true when the producer
   found that the change weakens the declaration. A producer that does not
   compare the declaration's content writes false.
7. **No tree, no verdict.** A verdict is an answer about a tested tree, so
   `change.tree` and `change.testedCheckout` stay required. A run that tested
   no tree has no verdict: a run canceled or failed before its checkout, and
   a run that lost the base of the merge it tested.
8. **`touched` and `reached` say how the change got to a project.** A project
   is `touched` when a changed path claims it: the project owns the path, or
   reads it as a declared input. It is `reached` when the change reaches it
   from another impacted project, touched or reached. `via` names that
   project when the producer knows it.

## Rejected alternatives

- **Keeping the head's tree and adding the tested tree beside it.** The head's
  tree follows from the head commit, so it tells a reader nothing new. Two
  trees also let a reader pick the wrong one.
- **A second subject for the tested tree.** A store that indexes statements by
  subject could then find a verdict by tree. One subject keeps the validator
  and every reader simple, and a consumer that needs the lookup indexes
  `change.tree`.
- **The ID of the tested commit.** A commit the runner builds is on no branch.
  Its ID resolves to nothing outside the run.
- **No verdict when the impact, the requirement states or the declarations
  are missing.** A run that failed would then have no answer document, and a
  consumer would need a second path to show it.
- **A verdict without a tree,** for a run that tested none. The verdict would
  then answer for nothing a consumer can name. Such a run answered no check,
  and its own record says how it ended.
- **Zero counts, or a flag that says a list is unknown.** A reader that skips
  the flag reads zero as "none". An absent member cannot be read as a count.
- **A new predicate version.** No consumer reads version 1, so a second
  version would only leave a first one nobody can use.

## Consequences

- A verdict in the first shape of version 1 is refused: `gitTree` in the
  subject is an unknown member, and a commit ID is an unknown tested checkout.
- A consumer cannot check a verdict against the tree of its head commit. It
  checks the head commit, and reads `tree` as what the run tested.
- A reader must test for `requirements` on a project before it reads the
  counts.
- The validator cannot detect a producer that omits an `unknown-impact` or an
  `unknown-declarations` gap it owes. A consumer trusts the producer for
  these two, as it does for every answer.
- A producer that knows the impact but no requirement state writes one gap per
  impacted project: at most 4,096 gaps, inside the bound of 16,384.
- A consumer finds no verdict for a run that tested no tree. It reads how that
  run ended from the CI plane's own record of the run.
- `ParseVerdict` reads an optional member written as `null` or as an empty
  string like an absent one, like every Go reader in this package. The schema
  refuses both.
- The schema states the mark of a passed verdict and the empty list beside an
  `unknown-impact` or an `unknown-declarations` gap. It does not state the
  folds, the sums, the match between gaps and lists, or the order of a list.
