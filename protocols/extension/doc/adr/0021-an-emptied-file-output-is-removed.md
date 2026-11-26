# ADR 0021 — An emptied file output is removed

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `emptied` member of a declared file output, CLI contract 27, and the CLI's
  emptying and restore paths
  ([CLI ADR 0107](../../../../tooling/cli/doc/adr/0107-an-emptied-file-output-is-removed-on-execution-and-on-an-empty-restore.md))
- **Amends**: [ADR 0009](0009-an-optional-project-output-may-ask-to-be-emptied.md)
  decisions 2 and 4

## Context

A task whose run may legitimately write no file has two ways to declare it,
and neither caches that outcome honestly.

- A **required** file output is present in every entry the engine stores. A
  run that writes none, a skip included, caches nothing, so the task executes
  on every run of an unchanged workspace. The Go and TypeScript `build-infra`
  skip every library and leave `.gen/requirements.json` unwritten, so each
  library runs one extension process per build. `package-deployment` has the
  same shape with `.gen/deployment.json`.
- An **optionalEmpty** file output is recorded absent by such a run, and a
  restore of that record leaves the path as it stands. A manifest an earlier
  run wrote there survives the hit, although the run that produced the entry
  left none. A deployer that reads it acts on requirements the current inputs
  do not produce.

ADR 0009 solved the same problem for a directory with `emptied`, and refused
the flag on a file because "a file is replaced whole". That holds for a run
that writes the file, not for one that writes none.

## Decision

1. **`emptied` is valid on a file output.** ADR 0009 decision 2 now reads: the
   output is a `directory` or a `file`, durable, with a literal `path`, under
   the `project` or `workspace` root, and `optionalEmpty`. A `runtime-file`, a
   `pathFrom` output, an invocation-scoped output, an output under the
   `command-output` root and a required output stay `invalid-output-emptied`.
   A required file is refused because every entry the engine stores holds it,
   so the flag would state nothing.
2. **The engine removes an emptied file** before its task executes, and on a
   restore of an entry that records it absent. A run that writes no file is
   then cached as the file's absence: an execution and a restore from the same
   key leave the same tree.
3. **The field enters under CLI contract 27** (`EmptiedFileOutputContract`).
   ADR 0009 decision 4 relied on the lock to keep an older CLI from reading
   the flag; for a file the protocol states it. A manifest that declares an
   emptied file output is stamped 27, so a reader below 27, which refuses the
   flag on a file or would leave a file an earlier run wrote where the entry's
   run left none, refuses it as written for a newer putnami. A reader at 27
   refuses such a manifest stamped below 27, from every origin, with the
   remedy `putnami extensions update`. A manifest without such an output
   keeps its stamp, and an emptied directory keeps contract 4.
4. **The field is part of the task contract**, as ADR 0009 decision 3 states:
   declaring it moves the task's key once, so an entry captured without the
   removal is never served.

## Rejected alternatives

- **Write an empty or sentinel manifest for a skip.** The output stays
  required and every run caches, but a reader then finds a manifest for a
  project that declares nothing, and each reader needs to learn the sentinel.
- **Let the task remove the file itself.** It reaches an execution, not a
  restore: a hit of an absent record would still leave the earlier file.
- **Empty every optional file output.** A file that a task writes beside a
  committed default would lose the default.

## Consequences

- `@putnami/go` and `@putnami/typescript` declare `build-infra`'s
  `.gen/requirements.json` and `.gen/infra/runtime.json` and
  `package-deployment`'s `.gen/deployment.json` optionalEmpty and emptied, and
  their manifests are stamped 27. A skip of either task, for a library, a
  workload that declares nothing or a withheld aggregate, is cached, and a hit
  removes a manifest an earlier run left.
- An older CLI refuses those manifests with the contract error. The lock pins
  the CLI with the extensions, so a workspace meets it only when it updates
  the extensions without the CLI.
- Every other reader of extension manifests refuses them the same way when it
  was built before contract 27. The lock pins each extension on its own, so a
  workspace that runs `putnami extensions update @putnami/go` alone keeps an
  older `@putnami/cloud`, whose CI graph (`putnami cloud ci validate` and the
  commands that resolve it) then refuses the 27-stamped manifest as written
  for a newer putnami. `putnami extensions update` without a name moves every
  extension and clears it.
- An emptied file the Git index tracks is never removed, as a tracked file in
  an emptied directory is not
  ([CLI ADR 0068](../../../../tooling/cli/doc/adr/0068-an-emptied-optional-project-output-is-emptied-on-execution-and-on-an-empty-restore.md)
  decision 6). The engine asks the index only while a file is at the path, so
  an absent file costs no git call.
