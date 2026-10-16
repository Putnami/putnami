# ADR 0010 — A pathFrom output may bound its path

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `within` member of a declared output, the CLI's capture of a `pathFrom`
  output, and the planner's key movement

## Context

A `pathFrom` output names a port whose reported value is the output's path.
The value exists only after the task runs. Before the run, the planner has to
decide which keys a planned write can move, and for a `pathFrom` output it
knows only the root. It counts the write as one to the whole root, so every
key that reads a file of the project moves: the run cannot record a
precomputed key for any of them, and pruning rechecks each one.

The Go `config-merge` task reports its merged file through the `mergedConfig`
port, because the file name carries the ambient `APP_ENV`. The file always
lands under `.gen/conf`. Before this decision, the planner moved every key of
every Go project that runs `config-merge`.

Only the task's author knows where the reported path can land.

## Decision

1. **A `pathFrom` output may carry `within`**, a root-relative subtree. The
   reported path must equal it or lie under it, compared without case like
   every ownership comparison.
2. **A path outside the bound leaves the run uncached.** The capture refuses
   the output and warns, as it does for a required output the task did not
   produce, so no cache entry records a write the planner did not count.
3. **The planner counts the write as one to the bound.** A key that reads no
   file under it does not move.
4. **The field is valid only with `pathFrom`.** A literal `path` is its own
   bound, so `within` on one is `invalid-output-within`. The bound is a
   canonical concrete path, never the root itself, or `invalid-output-path`.
   The schema states the first rule as a conditional.
5. **The field is part of the task contract.** It enters the task-contract
   digest, so declaring it moves the task's cache key once.
6. **The field enters under CLI contract 4**, for the reason in
   [ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
   decision 7. `RequiredCLIContract` does not change for a manifest that
   declares it. A CLI that predates the field drops it, counts the write as
   one to the whole root, and accepts any reported path. That is the
   behavior before this decision, so it is safe.

## Rejected alternatives

- **Infer the bound from the port's description or from past runs.** A past
  run does not bound the next one, and a wrong guess leaves a key unmoved
  while a write changes a file it reads.
- **Declare a literal path per environment.** The environment set is open,
  and a project adds one without editing the extension.
- **Bound the generated-client outputs too.** Their directory is configured
  per project and may land on files other tasks read, so their only safe
  bound is the project.

## Consequences

- `@putnami/go` `config-merge-exec` declares `within: ".gen/conf"` on its
  `merged` output. A key that reads no file under `.gen/conf` no longer moves
  when `config-merge` is planned.
- An extension that reports a path outside its declared bound caches nothing,
  and the warning names the port, the path and the bound.
- The bound states where the reported path lands, not every file the task
  writes. A task that writes outside its declared outputs breaks the contract
  whether its path is literal or reported.
