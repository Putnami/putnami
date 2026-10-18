# ADR 0069 — A source-bound entry is stored at its binding

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/jobs`, `internal/jobs/cachekey`,
  `internal/jobs/remotecache`, `internal/commands/cachecmd`),
  `@putnami/cli-model`, `go.putnami.dev/protocol/extension` (the
  `cache.sourceBound` field and the contract floor),
  `go.putnami.dev/protocol/cli` (contract 10), `@putnami/go`
  (`build-describe`)

## Context

Go `build-describe` writes feature evidence that names the source binding of
its project: the `source-v1` digest the scheduler stamps into
`<project>/.gen/version.json`. The binding covers every project file, test
files, specs and `putnami.features.json` included.

The describe key leaves test files out on purpose. Describe builds no test
file, and `^describe` is the edge every dependent's describe, compile and test
sits behind, so a test file in the key would move the whole dependent closure
for an edit none of it reads.

The two facts collide. After an edit to a test file, a spec or
`putnami.features.json`, the describe key stays the same and the cache serves
the entry an earlier tree produced. Its evidence names the earlier binding,
and the committed `schema/feature-evidence/go-framework.json` goes stale.

## Decision

1. **A task declares that its outputs embed its project's binding.** The task
   cache policy gains `sourceBound`, a boolean that defaults to false and is
   left out of the JSON when false. `build-describe` declares it.
2. **The binding is not a key input.** `ComputeJobCacheHash` does not read it,
   so a test-only edit keeps the task's cache key and every key that folds it.
3. **The binding is part of the entry's store key.** A source-bound entry is
   stored, leased, uploaded, fetched and looked up at
   `cachekey.SourceBoundEntryKey(cacheKey, binding)`, a domain-separated
   digest of the cache key and the binding. The local store and the remote
   cache use the same key. The store takes it as an opaque key and does not
   change.
   - **At store time** the binding is the one the execution was stamped with:
     after the stamp refresh and the preBuild hook, the scheduler reads the
     binding the stamp names for the task's own project. A follower of a
     shared execution uses its leader's binding.
   - **At lookup** the binding is the project's current one, from the memo the
     stamp writer reads.
   - **An unmanaged workspace** uses the empty binding on both sides.
   - **Inside a repository, a binding the CLI cannot compute** gives no lookup
     key and no store key. The task executes and stores nothing.
4. **Dependents fold the cache key.** The key a hit or an execution publishes
   for dependents, the key-movement closure and the failure record all use
   the cache key, not the store key.
5. **Entries stay immutable.** An edit that moves only the binding stores a
   second entry beside the first. A tree that returns to an earlier binding
   finds the earlier entry.
6. **The failure cache leaves source-bound tasks out.** Its record lives at
   the cache key alone, so it records no source-bound failure and replays
   none: the cause can be a file the key leaves out.
7. **No projection counts a source-bound entry as present.** Invocation
   pruning, the impact plan and remote negotiation work from cache keys and
   do not compute the current binding. They report the entry absent, and the
   restore fetches it on demand at its store key. `cache verify` reads each
   run's entry at the store key that run used.
8. **A manifest that declares the field requires CLI contract 10**
   (`protocols/cli.SourceBoundCacheContract`) wherever it loads from. An older
   CLI stores the entry at its cache key alone, so it refuses the manifest.

## Rejected alternatives

- **Fold the binding into the cache key.** The key of every task that folds
  the describe key would move on an edit none of them reads, which is what the
  test-file exclusion exists to prevent.
- **Record the binding beside the result and refuse a hit at another binding.**
  The entry at a cache key is immutable, so after one refusal every later run
  at that key executes the task until a key input changes. A store-key binding
  keeps both entries and needs no refusal check on each restore path.
- **Add a store replace operation.** Replacing the entry at a key breaks the
  immutability that leases, coalescing and remote uploads rely on, and a tree
  that returns to the earlier binding would execute again.
- **Add test files, specs and `putnami.features.json` to the describe inputs.**
  The binding covers every project file, so the input list would have to name
  every file, with the same effect on dependent keys.
- **Disable the describe cache.** Every run would build and run a host binary
  for every Go project, and every dependent would wait for it.
- **Rewrite the binding inside a restored output.** The CLI would have to
  parse an extension's evidence format, and the evidence would claim a tree its
  producer never described.

## Consequences

- The cache policy is part of the task contract digest, so the describe key of
  every Go project, and every key that folds it, misses once after the
  upgrade.
- `@putnami/go` is stamped 10. A CLI that reads only up to contract 9 refuses
  it.
- A test-only edit executes describe once. The next run at the same tree hits
  the new entry, and dependents stay served from their unchanged keys.
- Each binding a project passes through leaves one describe entry. Store
  collection reclaims them like any other entry.
- Two sessions at the same cache key and different bindings take different
  leases, so a waiter never waits on, or receives, another binding's entry.
- `@putnami/typescript`'s `build-generate` also writes a binding into
  `.gen/schema/feature-evidence/typescript-framework.json` and does not
  declare the policy. This ADR does not change it.
