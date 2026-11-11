# ADR 0007 — One file per decision

- **Status**: accepted
- **Date**: 2026-11-11
- **Decision**: `one-file-per-decision` in the root `decisions/` directory
- **Amends**:
  [CLI ADR 0030](../../../../tooling/cli/doc/adr/0030-settled-decisions-are-a-registry-validate-reads.md),
  which rejected one file per decision
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`: the
  decision file wire and the scope reader), `@putnami/sdd`
  (`tooling/sdd-extension`: `decisions-validate`), `@putnami/cli`
  (`tooling/cli/internal/commands/agentctx`: the generated guidance and the
  project context), `@putnami/contributor` (`tooling/contributor`: the
  verifier's policy files)

## Context

A scope kept its decisions in one `decisions.json` array. Two pull requests
that each settle a decision both append an entry before the closing `]` of
that array, so the second one to merge conflicts. The root registry of this
repository holds 70 decisions, and concurrent build sessions settle new ones
every day. An id alone does not help: a new id still lands in the same array.

ADR 0030 rejected one file per decision because it "keeps the context cost".
The guidance block no longer lists decisions, so the file layout does not
change what an agent reads.

## Decision

A scope holds its decisions in a `decisions/` directory, one file per
decision, named by its id: `decisions/<id>.json`. A scope is the workspace
root or a project directory, as before.

- A decision file holds one decision and its own `protocolVersion`. Its
  schema is `putnami-decision.json`. Its name is its id followed by `.json`,
  so the id is unique inside the scope by construction and a reader finds a
  cited decision by name.
- A new decision takes a slug id that names what it settles, such as
  `one-file-per-decision`. The decisions this repository settled before keep
  their `D-0NN` ids and move to `decisions/D-0NN.json`, so every citation
  still resolves.
- `decisions-validate` reads every decision file and refuses an id that
  another scope already holds, or that differs from one only in case: two
  such files collide on a case-insensitive file system.
- Paths a decision names (check globs, `adr`) stay relative to the scope
  directory, the directory that holds `decisions/`.
- A `.json` file in `decisions/` is a decision; any other entry is not read.

The single `decisions.json` file stays a supported form, because it shipped
in v0.4.0. A scope holds one form or the other: a scope with both fails
`decisions-validate`, naming how to move to the directory.

## Rejected alternatives

- **Slug ids in one array.** The id is new, the conflict is not: both pull
  requests still edit the end of the same array.
- **One file per decision, id from the file name only.** The file would carry
  no id, so a copied or renamed file would change the decision it states
  without a diff on any line a reviewer reads.
- **Directory form only.** A workspace on v0.4.0 holds `decisions.json`;
  dropping it would make its decisions vanish from the gate without a
  failure.

## Consequences

- Two pull requests that each record a decision add two files and merge
  without a conflict.
- Changing a settled value edits one small file, and the diff names the
  decision in its path.
- A reader lists a directory instead of opening one file; every reader goes
  through `features.ReadScopeDecisions`.
- A scope keeps the 256-decision bound: a directory with more decision files
  fails closed.
