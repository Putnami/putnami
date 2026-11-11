# ADR 0008 — a verdict policy states what a gap and a declaration change cost

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`)
- **Related**: [ADR 0006](0006-a-run-carries-one-verdict.md),
  [ADR 0007](0007-a-verdict-names-the-tested-tree-and-what-it-does-not-know.md)

## Context

A verdict lists its gaps and the declarations a change modifies. The CI plane
decides which of them block the change, with one rule for every repository:
a gap blocks until a person accepts it, and a declaration change waits for an
approval. A repository cannot say that an `adr` change needs no approval, or
that an unproven requirement of a `modeled` feature only informs.

A rule belongs next to the code it governs, so the owner of a directory owns
its rules. A rule in a file that tasks read re-runs them on every edit. In a
project's `putnami.json` it re-runs the project's build and tests. In a scope's
`putnami.json` it re-runs every project of the scope.

## Decision

1. **One file, `putnami.verdict-policy.json`, at the root, in a scope
   directory or in a project directory.** No task reads it, so an edit re-runs
   no task but its validation.
2. **The nearest file that names a key decides.** The files are read from the
   directory of the project or the declaration up to the root. A key that no
   file names takes the CI plane's default, which is the rule above.
3. **A key that concerns a feature can take one value per maturity.** The
   value of the highest maturity the feature reaches applies. The map holds
   `modeled`, so it covers every feature. An unknown maturity takes the
   strictest value of the map: the policy fails closed.
4. **`unknown-impact` and `unknown-declarations` are set in the root file
   only.** They concern the whole change, which no project owns.
5. **A policy change always waits for an approval.** It is a declaration of
   kind `policy`, which no file can set. A file that could waive the approval
   of its own change would make every rule optional.
6. **The policy never changes the verdict.** The gaps and the mark stay what
   the run found. The policy decides what blocks the change.
7. **`lost-proof` is a key of the policy only.** The verdict lists such a gap
   as `unproven-requirement`. The CI plane knows that the base proved it.

## Consequences

- The verdict's declaration kinds gain `policy`. The vocabulary is closed, so
  a reader of an older release refuses a verdict that lists one. The CI plane
  and its readers move together.
- The protocol does not know scopes and projects. The workspace validation
  step refuses a file in another directory.
- A CI plane reads the files at the change's base, so a change cannot loosen
  the policy that judges it. `VerdictPolicyLoosens` tells it whether a policy
  change weakens the policy.
