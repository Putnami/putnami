# 0010: Missions version 2 adds typed runs

- **Status**: accepted
- **Date**: 2026-11-19
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  the CLI route that calls a provider, and `@putnami/local-collaboration`
- **Amends**: [ADR 0003](0003-missions-contract.md), decision 1 (the
  operations of the contract), and
  [ADR 0004](0004-a-provider-is-declared-under-providers-and-speaks-its-own-wire.md)
  (one version per contract in a provider declaration)

## Context

A mission names one piece of work: an issue or a branch. Several kinds of
agent work run on one mission: a build, a review, a rebase, an audit, a
triage. `missions` version 1 reports a session's state, plan and stage, but it
cannot say which work ran, what it was given, or what it produced. Each skill
and each provider that wants to keep a review's findings or an audit's verdict
would invent its own record.

ADR 0006 freezes version 1: a new member or a new required operation opens
version 2. A provider manifest declares one version per contract under
`providers`, so a provider that moves to version 2 would stop serving the
workspaces that bind version 1.

## Decision

1. **`missions` version 2 holds every version 1 operation, with the same
   documents, and three run operations.** `sync`, `ask` and `reply` stay
   required and `status` optional, each with the request and result of
   version 1. `start`, `report` and `runs` are required in version 2.
2. **A version may include an earlier one.** `ContractSpec.Includes` names
   the version whose operations and documents a version repeats unchanged:
   missions version 2 includes version 1. A provider that declares version 2
   is offered version 1 too, with the version 1 operations it declares, so a
   workspace bound to version 1 keeps working when its provider moves to
   version 2. The orchestrator calls the provider with the version the
   workspace bound, and the provider answers that version's documents.
3. **A run is one execution inside a mission.** Its document carries `id`, a
   bare id as messages and decisions have (ADR 0003 decision 4), the
   `mission` (repository and key), the `kind`, the `state`, the `input`, the
   `result` once it `succeeded`, the `failure` once it `failed`, the
   `session` that ran it when the caller names one, `startedAt` and, once it
   ended, `endedAt`. Both times are the provider's clock, in UTC. A run
   starts `running` and ends `succeeded`, `failed` or `canceled`; an ended
   run never changes again.
4. **Five kinds, each with a closed input and result.** `input` and `result`
   are objects with exactly one member, named by the run's kind, whose
   document the kind defines:

   | Kind | Input | Result |
   |---|---|---|
   | `build` | `task` or `intention`, optional `change` | `stage` reached, optional `proposal` and `commit` |
   | `review` | `change`, `commit` reviewed, optional `proposal` | `commit`, `verdict`, `findings` |
   | `rebase` | `change`, optional `commit` before | `commit` after, `onto` base commit |
   | `audit` | `scope`, `revision` | `scope`, `revision`, `verdict`, `findings` |
   | `triage` | `task` | `summary`, optional `state`, `labels`, `duplicateOf` |

   A finding carries `severity` (`high`, `medium`, `low`, `info`), `title`,
   and optional `file`, `line` (with a file) and `body`. A result holds at
   most 100 findings whose titles and bodies total at most 64 KiB. An audit
   verdict is `pass`, `concerns` or `fail`; a review verdict is the review
   verdict of the proposals contract. A new kind, or a new member of a kind,
   opens version 3.
5. **`start` is keyed by an idempotency key.** Its content is the mission's
   repository and key, the kind, the input and the session. The same key
   with the same content replays the run in its current state; with other
   content it is a `conflict` with reason `idempotency.mismatch`.
6. **`report` is keyed by the run.** It ends a running run with a final
   state, the result of a `succeeded` run and the failure of a `failed` one.
   The same report again replays; another report on an ended run is a
   `conflict` with reason `run.closed`. An unknown run is `not_found` with
   reason `run.not_found`, a run the caller may not report is `denied` with
   reason `run.denied`, and a result of another kind than the run's is
   `invalid` with reason `run.kind_mismatch`. An unresolved `start` or
   `report` is reconciled by repeating it.
7. **`runs` reads a mission's runs.** It answers a bounded page of the named
   mission's runs, optionally of one kind, each once, the latest start
   first: of runs started at the same instant, the one started last comes
   first, so a provider whose clock counts seconds still lists the newest
   run first. Runs do not hold a mission (ADR 0008): a mission retired while
   it has runs keeps them, and `runs` still lists them by its repository and
   key.
8. **The route checks identity.** An ok `start` must name the mission and
   kind the request named and, when not replayed, be `running`. An ok
   `report` must name the requested run in a final state. An ok `runs` page
   must hold only runs of the named mission, and of the named kind when the
   request filters, and no more than the page size. Any other answer is
   `provider.identity_mismatch`.
9. **Shared scenarios.** `providertest.RunMissionRuns` runs the run
   semantics against any provider of version 2, and a provider of version 2
   still passes `RunMissions` on version 1 and version 2.

## Rejected alternatives

- **Several declarations per contract in the extension manifest.** It
  changes the extension protocol and every manifest reader for a case the
  inclusion rule covers: version 2 only adds operations.
- **Runs in `memory` records.** A memory record is free text with
  provenance; it has no typed result a skill can read back, and memory is
  bound apart from missions.
- **An open `input` and `result` with a schema per kind outside the
  contract.** A provider could not validate them, and a hosted provider and a
  local one would drift.
- **A run `kind` member only, with an untagged `input`.** The strict parser
  decodes a document into one Go type; the member named by the kind keeps
  each kind's document closed and its schema checked against its Go type.

## Consequences

- A skill records each execution with one shape, whatever provider the
  workspace binds, and reads a review's findings or an audit's verdict back.
- A provider that moves to version 2 keeps serving version 1 workspaces with
  no change to the manifest protocol.
- Version 1 documents and the frozen corpus do not change.
