# ADR 0002: Promotion and demotion criteria, and the review that protects them

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/support` (`protocols/support`)

## Context

[ADR 0001](0001-support-entry-ownership-and-evidence.md) settles where a
classification lives: status on the wire, owner derived from the entry id,
evidence in the subject's own documentation. Three things remain: what earns a
status, what forces one back down, and what stops a status from changing in a
pull request nobody reviewed for that. A public support promise is the one value
in this repository that must never move as a side effect.

## Decision

### 1. Criteria are a per-kind checklist; `stable` is the only promise

Every criterion names an artifact a reviewer opens, not an intention.

**Every subject, every status**: the entry resolves to exactly one owning
project, and that project's public documentation carries a support section
stating this exact status and linking the catalog.

**`stable`** adds, by kind:

| | Protocol | Package | Feature |
| --- | --- | --- | --- |
| Consumers | a shipping producer and consumer, named in the README, at least one across a boundary this repository does not control | published on a consumable channel, documented on the site | reachable through a documented surface |
| Change control | a pinned version anchor asserted by a test | a version gate that fails a release on an undeclared break | covered by the owning package's change control |
| Corpus | valid and invalid fixtures run by a conformance runner | tests of the documented public surface at the coverage threshold | its spec requirements are covered |
| Compatibility | an N-1 read window, or a migration documented in `RELEASE.md` terms | the same, in release notes | the same |
| Parity | shared fixtures prove every implementation, or `parity: unsupported` | the same | the same |

**`preview`** adds: at least one active consumer in the tree, and a documented
statement of what is missing for `stable`. `preview` may break in a minor
release when the release notes state the required user change.

**`experimental`** adds `default: false`, which the wire enforces. It promises
no compatibility or parity, and the default experience of a `stable` subject may
not reach it.

Before v1.0.0 these promises read through [`RELEASE.md`](../../../../RELEASE.md):
`stable` means publicly supported, documented, and maintained, and a pre-1.0
minor may still carry a documented migration.

### 2. Demotion has its own criteria

A subject drops to `preview` when it loses a criterion it was promoted on: its
last external consumer, its version anchor, its running corpus, or a break
ships without a documented migration. It drops to `experimental` only when it
also leaves the default experience. Demotion is as hard as promotion, because
withdrawing a promise is the change most likely to arrive as cleanup.

### 3. The reviewed `stable` set is pinned

`reviewedStableSubjects` in `conformance_test.go` maps every subject the
repository promises to support, keyed by `(kind, id)`, to the `default` and
`parity` claims reviewed with that promise. The gate fails in every direction: a
`stable` subject missing from the map, a mapped subject no longer `stable`, and
a changed claim. Claims are pinned because `parity` is a `stable` criterion; an
unpinned claim would let a later edit assert the exemption.

A promotion or demotion is therefore two edits in one pull request, one of them
in a file whose only purpose is to record the review. `preview` and
`experimental` stay unpinned: a subject that is still moving must be able to
move.

### 4. The subject repeats the status the catalog records

`criteria.go` reads a document's support section, the first heading naming
support, and takes the first status written as inline code as the declaration.
It also requires a Markdown link to the catalog in that section.
`TestClassifiedProtocolsDeclareTheirReviewedStatus` applies it to every
classified protocol module's README.

Bullet, bold-paragraph and table layouts all pass. Later mentions ("why not
`stable`") are ignored. Fenced code blocks are stripped first, and the catalog's
filename in prose is not a link. The reader stays unexported: it belongs to the
gate, not to the wire. Package READMEs are not gated yet.

### 5. A feature inherits the status of the subject that ships it

A feature needs no entry: the package or protocol that ships it carries the
promise. A `feature` entry exists only to state a status that differs from the
shipping subject's, such as an experimental capability inside a stable package.
`specs validate` therefore measures completeness over publishable projects, not
features.

### 6. Machine checks and reviewer checks

The gate checks §3, §4, owner resolution, and `default: false` for
`experimental`. A reviewer checks the §1 table: whether an artifact means what it
claims is a judgment a scoring checker would overstate. The pin forces that
review.

## Rejected alternatives

- **CODEOWNERS on `putnami.support.json`.** It depends on branch protection that
  is not verifiable from a clone, and it fails open. The in-repo pin needs no
  repository setting.
- **Score the criteria in code.** "Has an external consumer" is not decidable
  from the tree.
- **Pin every entry.** It freezes `preview` and teaches reviewers to
  rubber-stamp the pin.
- **Make `specs.missing_support_entry` an error.** An unclassified project is
  scheduled work, a broken document is a contract violation; they must not share
  an exit code. This repository's completeness is pinned by a workspace test.
- **`criteria` or `evidence` wire fields.** Policy that changes must not require
  a wire version bump.

## Consequences

- The gate's failure message points at this record.
- The pin forces a deliberate second edit, not a second human: one author can
  write both halves. That is the ceiling of an in-repo check.
- Two pull requests changing the `stable` set conflict on one Go declaration.
  Concurrent changes to public promises should meet.
- Renaming a `stable` subject fails the gate twice, as a lost promise and an
  unreviewed one. Renaming the public identity is a compatibility event.
- The current `stable` set is the reviewed baseline, not re-derived from §1.
  Auditing it belongs to release readiness.
