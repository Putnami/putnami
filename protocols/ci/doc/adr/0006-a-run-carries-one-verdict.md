# ADR 0006 — a run carries one verdict

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/ci` (`protocols/ci`)

## Context

A CI run of one change produces many answers: one per check, one per deploy
channel, and a requirement state for each impacted project. A consumer that
shows the change, gates a merge or keeps a history needs one answer and the
reasons behind it. Without a shared document, each consumer folds the same
answers its own way, and two consumers can disagree about one run. A consumer
also needs to know which commit an answer is about, so that it can refuse an
answer about another commit.

## Decision

1. **A verdict is an unsigned in-toto Statement v1.** `_type` is
   `https://in-toto.io/Statement/v1`. The one subject is the repository URL,
   with the head commit as `gitCommit` and its tree as `gitTree`.
   `predicateType` is `https://putnami.dev/verdict/v1`. A producer may sign
   the statement; this package neither signs nor verifies.
2. **The predicate type carries the version.** Version 1 is
   `https://putnami.dev/verdict/v1`. A later version is a new predicate type,
   and `ValidateVerdict` refuses every type but its own.
3. **The answer is the fold of the required checks.** The fold keeps the most
   severe answer: `failed`, `infrastructure`, `canceled`, `unanswered`, then
   `passed`. `unsupported` never blocks: it is the fold when no required check
   answered anything else. An advisory check is reported and never changes the
   answer.
4. **A passed verdict carries a mark.** `verified` means it passed with no gap.
   `checked` means it passed with at least one gap. A verdict that did not pass
   carries no mark.
5. **Only a verified requirement is proven.** Each impacted project counts its
   requirements, and `missing` counts every state other than `verified`. The
   verdict sums the counts. It lists one gap per missing requirement, one per
   project that states no requirement, and one per weakening declaration
   without an approval. The validator checks that the gaps match the counts
   and the declarations.
6. **Every list is canonical.** Checks sort by name, projects by ID, deploy
   channels by name, and gaps by kind, project, requirement, then path.
   Declarations sort weakening first, then by path. No list is absent or null.
7. **A higher revision supersedes.** One run attempt may compute its verdict
   more than once, for example when a late answer arrives. `revision` counts
   from 1, and a higher revision replaces every lower one.
8. **The schema states the shape and the validator states the rules.**
   `schemas/putnami-verdict.json` closes every object. `ValidateVerdict` also
   checks the fold, the mark, the sums, the gaps and the order, which the
   schema cannot state.

## Rejected alternatives

- **A `version` member in the predicate.** in-toto already versions a
  predicate by its type, and a consumer chooses a reader by the type before
  it reads the predicate.
- **Counting a stale or unmapped requirement as proven.** A verdict could then
  say `verified` when the proof is old or absent.
- **A `passed` boolean.** It cannot tell a failing change from failing
  machinery, or a canceled check from one nothing answered. A consumer needs
  that difference to decide whether to retry.
- **Gaps as counts only.** A count tells a reader that something is missing,
  not what to fix.
- **A signed envelope in this package.** Signing needs keys and a trust policy,
  which belong to the producer and the consumer, not to the document.

## Consequences

- A producer must compute the counts, the gaps and the fold the way the
  validator does, or every verdict it writes is refused.
- The gap list is exhaustive and holds at most 16,384 gaps. Version 1 cannot
  express a change with more missing requirements than that.
- The run's `conclusion` is a code the CI plane defines. This package does not
  interpret it, so it cannot check that the conclusion agrees with the answer.
- The repository URL follows the ChangePlan rule: an `https`, `http` or `ssh`
  URL without a user, query, fragment or trailing slash. A producer writes an
  scp-like `git@host:org/repo.git` as `ssh://host/org/repo.git`.
- `ParseVerdict` reads an absent integer or boolean member as zero or false,
  like every Go reader in this package. The schema requires those members.
