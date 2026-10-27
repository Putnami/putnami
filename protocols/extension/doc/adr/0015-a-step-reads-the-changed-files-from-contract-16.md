# ADR 0015 — A step reads the changed files from contract 16

- **Status**: accepted
- **Date**: 2026-10-26
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `activation.changedFiles` gate and the `from`/`path` input binding of a
  pipeline step, the `finalizes.consumers` relation, and CLI contract 16
  (`go.putnami.dev/protocol/cli`, `protocols/cli/contract.go`)

## Context

`@putnami/go` runs `go test -race` on the packages a change touched
([ADR 0013 of `@putnami/go`](../../../../go/extension/doc/adr/0013-the-race-detector-runs-on-the-packages-a-change-touches.md)).
The step needs two facts at plan time: whether the target project owns a
changed file, and which files they are. An impacted run measures both, but no
manifest member could read them.

The schema already described a binding `{ "from": "command" | "context",
"path": ... }`, and the documentation showed `from: command` with
`path: flags.target`. No CLI resolved any `from` binding: a step that declared
one received no parameter, and its key did not hold the value its author meant
to bind.

A `runOn: "finally"` step cleans up a resource after every listed consumer is
terminal. A consumer that an `if` or an activation gate removes from the plan
never runs, so a cleanup that waited for every listed consumer had nothing to
wait for, and the relation left the resource without cleanup.

## Decision

1. **`activation.changedFiles` gates a step on the run's changed files.** Its
   globs read with the workspace file-pattern grammar, negation included, and
   match the changed files the target project owns, project-relative. A
   project owns a file when the file is under its directory; a project the
   change reaches through an edge owns none. A run that measured no diff
   (`--all`, `--projects`) plans no gated step. An unmet gate splices the step
   out like every other activation gate.
2. **One binding source and one path.** `{ "from": "context", "path":
   "selection.changedFiles" }` binds the changed files the target project
   owns, sorted, unique and project-relative. On a step with a change gate, it
   binds only the files the gate's globs select. The value joins the step's
   own parameters beside its literals, so the task receives it and the step's
   key holds it. A run without a diff binds no value.
3. **`from: command` is deleted, and no other context path exists.** Strict
   validation, which gates packaging and `putnami extensions validate`,
   refuses any other `from` or `path` (`unresolvable-binding`), and so does
   loading a manifest stamped 16. A manifest stamped below 16 that spells one
   loads and binds nothing: no earlier reader resolved it.
4. **The vocabulary enters as additive contract 16, and the floor binds every
   origin.** `DeclaresChangedFiles` is the predicate: a `changedFiles` gate,
   or the exact pair of decision 2. `RequiredLoadContract` includes the rung.
5. **Cleanup waits for the consumers the plan holds.** `finalizes.consumers`
   still lists every downstream step that uses the resource, and each must be
   downstream of the producer. The planner keeps the listed consumers it
   planned; a consumer that an `if`, an activation gate, `disable` or a task
   scope removed never runs and is not waited for. A plan that holds none of
   them leaves the finalizer inert.

## Rejected alternatives

- **Keep `from: command`.** It names no path vocabulary and no reader ever
  resolved it; a command flag already reaches every step as a parameter.
- **Require 16 for any `from` binding.** A manifest stamped 15 that spelled one
  loaded and bound nothing; refusing it would break a load that changes nothing
  in what the step receives.
- **Bind every owned changed file.** A README edit beside a Go edit would move
  the gated step's key and repeat its work on the same packages.
- **Keep the complete-frontier rule.** A finalizer whose consumers include a
  gated step would wait for a step that never runs on every run without a
  diff.

## Consequences

- A manifest that gates on or binds the changed files is stamped 16, so a CLI
  below 16 refuses it with "requires a newer putnami".
- A manifest stamped below 16 with either form is refused with "requires 16
  for a step that reads the run's changed files" and `putnami extensions
  update` as the remedy.
- `@putnami/go` claims 16 for its `test-race` step.
- A gated step's key moves with the change it reads, and only with the files
  its gate selects; the other steps of the command keep their keys.
