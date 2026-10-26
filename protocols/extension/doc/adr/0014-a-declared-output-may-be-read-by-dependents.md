# ADR 0014 — A declared output may be read by dependents

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `readByDependents` member of a declared output, the planner's ordering of
  the jobs of dependent projects, and the `@putnami/clientgen` generators

## Context

A generated client is often a project of its own. Its `clientgen~generate-go`
job rewrites the client the project commits, such as
`cloud/clients/auth-server/go/client.gen.go`. The projects that depend on the
client project compile those files.

The generator declares the `clients-go` write. A resource conflicts only
within one project, so the planner never orders the generator against the jobs
of a dependent project. In this repository, the 28 Go and TypeScript
generators of the 14 `/cloud/clients/*` projects stay unordered with the
`build`, `lint` and `test` steps of `/cloud/extension` that read the clients:
`/cloud/extension` can compile a client while its generator rewrites it.

Only the generator's author knows that the projects depending on its project
read the output.

## Decision

1. **A declared output may carry `readByDependents`.** It states that the
   projects which depend on the owning project read the output as part of
   that project, such as a generated client they compile.
2. **The planner orders every job of those projects after the task.** The
   rule covers direct and transitive dependents, through declared dependency
   edges. The implicit edge from a scope include to its scope reads nothing,
   so it does not count. Each edge is an ordering edge (`SerializeAfter`),
   never a functional one, so it never enters a cache key and a failed writer
   does not cancel the waiting job.
3. **The planner keeps the plan a DAG.** It adds no edge when the waiting job
   already runs before the writer over the edges the plan holds. It skips
   finalizers, which the invocation runtime dispatches, and a job that already
   depends on the writer. It adds no job to the plan, so an unplanned writer
   orders nothing.
4. **The field is valid only on a durable output under the `project` root.**
   The `workspace` and `command-output` roots and the invocation scratch lie
   outside the project, so its dependents read nothing there. Any other output
   that carries it is `invalid-output-read-by-dependents`. The schema states
   the rule as a conditional.
5. **The field is part of the task contract.** It enters the task-contract
   digest, so declaring it moves the task's cache key once.
6. **The field enters under CLI contract 4**, for the reason in
   [ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
   decision 7. `RequiredCLIContract` does not change for a manifest that
   declares it. A CLI that predates the field decodes manifests permissively:
   it drops the field and plans as before this decision, race included, but
   it loads the extension. A strict validator that predates the field, such as
   an older `extensions validate`, rejects it as an unknown member.

## Rejected alternatives

- **Order dependents after any dependency job that declares a project-rooted
  `pathFrom` directory output.** Every Go `describe` and TypeScript `generate`
  step declares such an output, so the rule serializes the `lint` and `test`
  steps of every dependent behind its dependencies' generators. On this
  repository's `lint,test,build,validate --all` plan it grows the edges from
  8896 to 15118.
- **Declare a project `sources` write on `clientgen-go`.** The clientgen spec
  forbids it: generation leaves authored sources alone, and a `sources` write
  implies `mutatesSources`. A project-scoped resource also conflicts only
  within its project, so it would still not order `/cloud/extension`.
- **Declare the workspace-scoped `workspace-clients` write.** It serializes
  every generator against every reader in the workspace, and it creates a
  cycle in `lint,test,build,package,validate`.
- **Make the emitters write atomically, and only on a change.** The plan still
  holds the race: a cache restore or a drift rewrites the client while a
  dependent compiles it.

## Consequences

- `@putnami/clientgen` declares `readByDependents` on the `client` output of
  `clientgen-go` and `clientgen-ts`. Every `/cloud/extension` job now waits for
  the generators of the clients it depends on, except its test finalizer.
- On this repository's `lint,test,build,validate --all` plan, the rule grows
  the edges from 8896 to 9400.
- The clientgen task keys move once, because the field enters the
  task-contract digest.
- The rule orders the jobs of a dependent project. It does not order the jobs
  of the writer's own project, which declared resources already sequence.
- A generator that writes a client into another project is not covered: a
  provider's Go `describe` or TypeScript `generate` step whose `pathFrom`
  output lands in a client project orders that project's jobs through the
  contract tree rule, but not the jobs of the projects that depend on the
  client. No project of this repository has such a dependent today.
- The rule adds an edge even when another path already implies the order, so a
  later pass that drops a job in between keeps the order. A generator that
  produces nothing, such as the TypeScript generator of a Go-only client, still
  makes the dependents wait for it.
