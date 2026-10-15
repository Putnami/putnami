# ADR 0009 — An optional project output may ask to be emptied

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `emptied` member of a declared output, and the CLI's emptying and restore
  paths ([CLI ADR 0068](../../../../tooling/cli/doc/adr/0068-an-emptied-optional-project-output-is-emptied-on-execution-and-on-an-empty-restore.md))

## Context

Before a task executes, the engine empties each required directory output and
each `optionalEmpty` directory under the command-output root, and a restore of
an entry that records such an output empty empties it too
([CLI ADR 0064](../../../../tooling/cli/doc/adr/0064-an-executing-task-starts-from-an-empty-owned-directory.md),
[CLI ADR 0065](../../../../tooling/cli/doc/adr/0065-an-optional-command-output-directory-is-emptied-on-execution-and-on-an-empty-restore.md)).
An `optionalEmpty` directory under the project or workspace root is left as it
stands, because it can hold a file no task writes, such as a committed
configuration.

That rule is right for a directory that holds such a file, and wrong for one
that does not. The Go `build-describe` writes `.gen/schema` and
`.gen/clientgen`, and nothing else writes either path. A file an earlier run
left there survives an execution that writes nothing, and a restore of an
empty record. A task that reads the directory afterward cannot fold
describe's key in place of the bytes, because the bytes are not a function of
that key. It has to hash them, and they are written during the run, so the key
the run computes before any task starts differs from the key the task computes
when it starts.

Only the task's author knows that no committed file lives in the directory.

## Decision

1. **A declared output may carry `emptied: true`.** The engine then empties it
   before the task executes and on a restore of an entry that records it
   empty, with the keep set it uses for every emptied output: the ceded
   (`excludes`) and preserved (`preserves`) subpaths and the build stamp.
2. **The flag is valid only where it changes something the engine can do.**
   The output is a `directory`, durable, with a literal `path`, under the
   `project` or `workspace` root, and `optionalEmpty`. Each other shape is
   `invalid-output-emptied` at the `emptied` field: a file is replaced whole,
   a `pathFrom` value is unknown until the task runs, an invocation-scoped
   output lives in a tree created for the run, and a required directory or an
   optional `command-output` directory is emptied without the flag. The schema
   states the same rule as a conditional.
3. **The field is part of the task contract.** It enters the task-contract
   digest, so declaring it moves the task's cache key once, and an entry
   captured without the emptying is never served to the task.
4. **The field enters under CLI contract 4**, for the reason in
   [ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
   decision 7. `RequiredCLIContract` does not change for a manifest that
   declares it. A CLI that predates the field would drop it and leave the
   output as it stands, and that is not harmless: a task that reads the
   output keys on the producer's key in place of its bytes, so that CLI would
   serve the task a result computed from files an earlier run left there. No
   such CLI reads these manifests, because the lock pins the CLI
   (`putnami.lock.json#cli`) together with the extensions that declare the
   field.

## Rejected alternatives

- **Drop `optionalEmpty` from such an output.** A required output must be
  present in every entry, so a run that writes nothing there would cache
  nothing, and a skip would execute on every run.
- **Empty every optional project output.** A slice that commits a file inside
  one loses it before the task runs.
- **Have the task remove the directory itself.** The task reaches an
  execution, not a restore: a hit of an empty record would still leave the
  earlier run's files.

## Consequences

- `@putnami/go` `build-describe` sets the flag on its `schema` and `clientgen`
  outputs.
- **This is a breaking change.** Committing a file under `.gen/schema` or
  `.gen/clientgen` is no longer supported. The CLI never empties an emptied
  output that holds a Git-tracked file, so describe fails while a project
  commits one there
  ([CLI ADR 0068](../../../../tooling/cli/doc/adr/0068-an-emptied-optional-project-output-is-emptied-on-execution-and-on-an-empty-restore.md)
  decision 6). To
  migrate, move `.gen/clientgen/config.json` to `clientgen.json` at the
  project root and `.gen/schema/openapi.json` to `schema/openapi.json`, then
  delete both from `.gen`. The `@putnami/clientgen` workspace check reports
  each file still committed there as `clientgen.committed-generated-contract`.
  Under D-004 the change ships in a minor release, whose notes name this
  step.
- A later task that reads an emptied output keys on the producer's key
  instead of the bytes, so its key does not move when the producer writes the
  path during the run.
- A file another task writes inside an emptied output goes at the owner's
  next execution or empty restore, as it does in any emptied output.
