# ADR 0022 — A step activation reads the project's type and alternatives

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `activation` gate of a pipeline step, and CLI contract 28
  (`go.putnami.dev/protocol/cli`, `protocols/cli/contract.go`)

## Context

A step's `activation` drops it from the plan for a project it has no work for.
Its conditions read one named file (`contains`), the existence of a file in
the project or its closure (`files`, `closureFiles`) or the change
(`changedFiles`), and all of them must hold. An infrastructure step has work
for a workload, which is the project's declared type, and for a project that
holds an output an earlier workload build left, which its run clears. Neither
could be written, so the step was planned for every project (CLI ADR 0132).

## Decision

1. **`projectTypes`** lists project types. The gate holds when the project's
   declared type is listed; `""` names a project that declares none.
2. **`anyOf`** lists alternative gates. The gate holds when at least one of
   them holds, besides every other condition of the gate. An alternative is a
   gate of the same vocabulary, nested alternatives included.
3. **The members enter as additive contract 28** (`ContextActivationContract`).
   `DeclaresContextActivation` is the predicate; `RequiredLoadContract`
   includes the rung. A reader below 28 decodes manifests strictly and would
   refuse the members with a decoding error, so a manifest that declares one
   is stamped 28 from every origin, and a reader below refuses it with the
   remedy.

## Rejected

- **A project type in `if`.** `if` already reads `project.type`, but an `if`
  cannot be one alternative of a file condition.
- **A gate on the content of any source (`filesContain`).** It would decide
  the Go generate step, but that step stays planned for the key chain its
  dependents follow (CLI ADR 0132), and no other step needs it.

## Consequences

- `@putnami/go` and `@putnami/typescript` are stamped 28.
- An older CLI refuses those manifests with "requires 28 for a step activation
  on the project's type or on alternatives".
