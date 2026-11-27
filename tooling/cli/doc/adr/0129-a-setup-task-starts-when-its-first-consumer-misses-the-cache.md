# ADR 0129 — A setup task starts when its first consumer misses the cache

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`: the invocation
  relation and the coordinator), `protocols/extension` (the `finalizes`
  lifecycle wording)

## Context

A `finalizes` relation is a producer that provisions an invocation-scoped
resource, the consumers that read it, and a finalizer with `runOn: finally`
that cleans it up (`protocols/extension/doc/07-lifecycles.md`). The Go and
TypeScript extensions declare one per project with a test database:
`test~test-env` (`test-env-up`), `test~test` and `test~test-race`, and
`test~test-env-teardown` (`test-env-down`). The producer is `cache: false` by
contract: a cache hit would report success without creating the artifact.

An unchanged `lint,test,build,validate --all --enforce-coverage` run of this
repository starts 51 job processes. 48 of them are these 24 producers and
24 finalizers, although every consumer they serve is a cache hit: 1.03 s of
job CPU and 1.16 s of task wall summed over the workers, measured for
ADR 0113 §8.

The relation already had a way to skip them, decided at plan time. Before the
first task, `prune` precomputed every job's key, looked each consumer up, and
pruned a relation whose whole frontier was present. A consumer key that a
planned write could move was only an estimate, so prune also computed the
plan's key movement and kept the relation unless every such writer settled
before the producer. ADR 0113 §8 measured it on this repository: prune
settles none of the 48 relations, and once their `pruneIf` is valid it costs
about 3 s of wall on every unchanged run to compute the movement. It costs
more than it saves, and it is rejected there.

The decision does not need a prediction. A consumer learns whether it hits
the cache when it starts, at the key it computes then, after its writers ran.
The producer only has to have run before a consumer that executes.

## Decision

### 1. A lazy relation's producer is parked, not dispatched

A relation is **lazy** when:

- its finalizer's `pruneIf` is absent, or is valid and true under the params
  its producer receives (the workspace options and the command line, merged as
  for its job context) and its command's flag defaults, aliases included,
  since a pipeline step's own job definition carries no flags. A false
  predicate keeps the relation eager, so an optimization never swallows an
  imperative side effect such as `--infra-down`; so does an invalid one, such
  as a param no layer sets. Plan-time prune read the command line alone, where
  a flag left at its default is missing, so a declared `--infra-down` would
  have kept every relation eager;
- the plan gave it no conflict (a step in two frontiers blocks both, as
  before);
- every job that depends on the producer in the DAG is one of its consumers.
  A job outside the frontier that waits for the producer keeps it eager.

When a lazy relation's producer becomes ready, the coordinator **parks** it:
it completes the producer in the DAG, so the jobs that waited for it become
ready, and dispatches nothing. Nothing is spawned, no scratch is created, no
lease is written and the finalizer is not armed.

### 2. The first consumer that misses starts it

A consumer opens as every task does: it computes its key and looks its entry
up, locally and remotely. A hit is served and never needs the resource.

A consumer that misses while its producer has not finished returns
**deferred** to the coordinator before it claims the key, holding no lease and
no admission, and without a start row. It records the key it missed at. The
coordinator queues the producer the first time one of its consumers defers,
and holds each deferred consumer until the producer finishes. It then queues
them again through the ordinary ready path: a producer that did not deliver
blocks them with `sensitive.setup_failed`, as before; otherwise each opens
again, computes its key, and runs after its setup. When the key is the one it
missed at, it claims it without asking the stores again, so the run counts one
miss for it.

A consumer that depends on the producer only through another consumer
(`test-race` after `test`) is covered by the same rule: it defers on its own
miss, whatever the other consumer did.

### 3. A frontier served from the cache leaves one row

When every consumer of a parked, unrequested producer is terminal, the
coordinator ends the producer with a `skipped` row, *not needed: every
consumer was served from cache, or did not run*. Its finalizer was never
armed, so the existing settlement records it skipped as well. Neither
spawned a process.

### 4. A parked setup is not an ancestor a grant waits for

A protected job (one with external effects) receives its process capability
only once every functional ancestor succeeded. A parked producer is waived:
its consumers on the job's path are ancestors too, they run only after a
requested producer succeeded, and a failed one blocks them with
`sensitive.setup_failed`, so the job is refused through them.

### 5. Plan-time prune is removed

`prune`, its key precompute and movement, and the producer's confirmation of
a moved key (`isPruned`, `recheck`) are deleted. The run's remote negotiation
keys the plan itself when it needs keys, as it already did on any plan
without a prunable relation (ADR 0113 §8).

```text
            eager (pruneIf false/invalid)          lazy (this ADR)
            ─────────────────────────────          ───────────────────────────────────
ready  ──►  test-env ─spawn─► test ─► test-race    test-env: parked (DAG-complete, no spawn)
                │                                      │
                └─ arms teardown                       ▼
                                                   test  ── hit ──► served
                                                     │
                                                     └─ miss ─► deferred ─► coordinator
                                                                              │ queue test-env once
                                                                              ▼
                                                                         test-env runs, arms teardown
                                                                              │ done
                                                                              ▼
                                                                  re-queue deferred consumers
                                                                  (setup failed ⇒ setup_failed skip)

frontier terminal, producer never requested ⇒ test-env: skipped "not needed",
teardown: skipped (never armed). No process.
```

## Consequences

- An unchanged run starts no `test-env` and no `test-env-teardown` process
  once their `pruneIf` is valid, that is once `putnami test` declares
  `--infra-down`. A run where one project's tests miss starts exactly that project's
  producer and finalizer.
- The guarantees of the relation hold unchanged: a consumer that executes
  runs after its producer finished; the finalizer runs exactly once whenever
  the producer started, and never when it did not; a producer that failed
  blocks its frontier with `sensitive.setup_failed`.
- A consumer that misses opens twice: once to learn it misses, and again
  after its setup. The second opening reads the memoized input digests of the
  first and computes the same key, unless a write in between moved it, in
  which case it looks that key up as any task does. At an unchanged key it
  claims without a second lookup: an entry another session stored at that
  key while the setup ran is not restored, and the consumer executes; the
  claim still coalesces with a session that holds the key's lease.
- A consumer that misses waits for its producer's whole run before it
  starts, where an eager producer might have finished earlier. This is the
  case where the environment is actually needed, and the producer starts as
  soon as the first consumer knows it misses.
- A consumer whose entry is only in the remote cache is served before the
  producer is considered: plan-time prune treated it as a miss.
- The keyless identity a consumer's key reads for its producer is computed on
  demand when the producer has not started (`keyless_identity.go`), so a
  consumer's key is the same whether its producer is parked or ran.

## Measurements

Measured on 2026-11-27 on a 4-core Linux container, signed out of Putnami
Cloud: `./putnamiw lint,test,build,validate --all --enforce-coverage` on
main `1a4a5f374` against this change merged with the declaration of
`--infra-down` on `test` (the flag that makes the relations' `pruneIf`
valid). Each tree was primed cold, then once warm; then three unchanged runs
of each, alternating. Medians of the three (2,221 tasks, 2,194 served from
the local cache, in both):

| Measure | main | this change |
|---|---|---|
| Job processes (`run.orchestration.processes`, kind `job`) | 51 | 3 |
| Extension CPU (`cpu.extensionMs`) | 1.06 s | 0.06 s |
| Wall of the command | 12.35 s | 11.03 s |
| Engine CPU (`cpu.engineMs`) | 17.9 s | 16.4 s |
| Run CPU (`cpu.runMs`) | 20.9 s | 18.5 s |

The 48 processes gone are the 24 `test~test-env` and 24
`test~test-env-teardown` tasks; their rows now read skipped, not needed.

## Rejected alternatives

- **Keep plan-time prune and make the movement cheaper.** ADR 0113 §8: it
  settles none of the 48 relations on this repository and costs about 3 s of
  wall per unchanged run once the predicate is valid.
- **Block the consumer's worker until its producer finishes.** A worker
  waiting on a task that needs a worker deadlocks a full pool. The deferred
  consumer gives its worker back instead.
- **Make the producer cacheable.** An invocation-scoped output exists only
  inside the invocation that wrote it; a hit would report a database that
  does not exist.
