# ADR 0017 — A test run keys on the identity of its test binaries

- **Status**: accepted
- **Date**: 2026-11-28
- **Scope**: `@putnami/go` (`go/extension`): the `test-identity` step of
  `test`, the `test-identity-exec` task, the inputs of `test-exec` and
  `test-env-up`, and the output ports of `build-generate` and `build-describe`
- **Applies**: CLI [ADR 0043](../../../../tooling/cli/doc/adr/0043-a-cross-project-carrier-task-keys-only-on-what-it-reads.md)
  (artifact inputs, CLI contract 32)

## Context

`test-exec` depended on `describe`, and folded its key. `describe` folds
`^describe` and `generate`, and `generate` folds `^generate`: the key of a
project's test run moved with every non-test Go source of its dependency
closure. An end-of-line comment in `go/framework/errors`, which compiles to
the same bytes, re-ran the tests of every project that depends on it
(measured below).

What a test run executes is its test binaries, plus the files its tests open.
The Go toolchain already names a binary's inputs by what they compile to:
`go list -export` reports each package's build ID, `actionID/contentID`, and
the content ID is the hash of the compiled output. Two measurements decided
how to read it (`go/framework/errors` and `go/framework/app`, the same
sources at two checkout paths):

- Without `-trimpath`, content IDs and the test binaries differ between the
  two checkouts: compiled objects embed absolute paths. A key on them would
  stop sharing a verdict between two machines whose workspace roots differ,
  which today's source keys share.
- With `-trimpath`, they are identical. Running trimmed binaries is not an
  option: about 20 test files locate their fixtures through `runtime.Caller`,
  and `go.putnami.dev/logger`'s `TestNoStdlibSlogLoggingInFramework` fails
  with `lstat go.putnami.dev`.

## Decision

**The test step keys on the trimmed content IDs of its test binaries' link
closures, and on what its project's own steps wrote, instead of on the keys
of those steps.**

1. **The identity step.** `test-identity` (task `test-identity-exec`) runs
   after `generate`, whose `.gen` Go files the binaries compile, and before
   `test`. It runs
   `go list -e -trimpath -buildvcs=false -export -deps -test ./...` in the
   project, in the environment `go test` runs in (`testCompileEnv`) and with
   the compile flags of the run. One function derives those flags for the
   solo run, for a batch member and for the identity (`binaryFlags`,
   `projectBinaryFlags`): coverage instrumentation, `-coverpkg` over the
   packages the module owns, `-covermode`, `-race`. Every other flag of
   `go test` configures the run of the binaries, not their bytes.
   `-buildvcs=false` matches `go test`, which stamps no test binary.
2. **The manifest.** The step writes `test-identity.json` into the command
   output and names it on the output port `manifest`: the manifest version,
   the Go version, `GOOS`, `GOARCH`, the flags, and the SHA-256 of the sorted
   (import path, content ID) pairs of the link closure of each test main
   package (`<pkg>.test`, the test variants `<dep> [<pkg>.test]` included) and
   of each package `./...` matches, so a test that builds one of its project's
   packages at run time keys on what it builds. A matched package that holds
   tests alone builds nothing and gets no entry: its test main names it. A `go test` with `-trimpath`
   and the same flags after the listing compiles nothing: the listing performs
   exactly the compile actions of the trimmed twins of the run's binaries.
3. **Its key moves freely.** `test-identity-exec` keys on the project's Go
   sources and embeds, its modules, the workspace modules, the compile
   parameters, the host platform, and `generate`'s key, which covers the
   closure. It is cached: a warm listing reuses the trimmed compile actions in
   the Go build cache.
4. **What the test step folds.** `test-exec` binds, as artifact inputs, the
   identity manifest, the `.gen` tree its project's `generate` wrote (`gen`),
   and the declared outputs of its project's `describe` that a test may read
   at run time (`schema`, `migrationBundle`, `migrations`, `design`,
   `httpRoutes`, optional because `describe` runs only in an app's closure).
   The feature evidence is left out: it names the source binding, which moves
   with every file of the project. The committed copy, `featureEvidence`, is
   not bound, and the `schema` port leaves the copy `describe` emits under
   `.gen/schema/feature-evidence` out of its digest (`artifactExcludes`), so
   an edit of a file no test reads, a `README.md`, runs `describe` again and
   not the tests. The step no longer depends on `describe`.
5. **What the test step keeps.** The project's own files stay ordinary
   inputs: its Go sources, `testdata`, `go.mod`, `go.sum`, `go.work`, its
   config, the parameters, the environment, the toolchain, and the project's
   `options.test.filePatterns`.
   A dependency's source that a test reads or compiles at run time, outside
   the binaries it links, is keyed only when the project declares it in
   `options.test.filePatterns`.
6. **Untrimmed run, trimmed identity.** The run's binaries are the
   identity's untrimmed twins: the same compile actions but `-trimpath`, so
   they differ by their embedded paths alone. On one machine the paths are
   the workspace root's, which no cache key names today: the source keys this
   replaces already shared a verdict between two roots. The identity is
   therefore exactly as sound across machines as those keys, and sharper.
7. **A failure is never named.** A package that does not compile has no
   content ID, and a listing can fail. Either writes a manifest with a random
   `unresolved` value and reports the step skipped. The engine stores the
   skip of a task that is not deterministic in no cache, so no later run
   replays that value, and it publishes the manifest's digest as it does a
   success's: the test step runs under a key no other run shares and reports
   the failure itself, as it did before.
8. **The platform is part of the identity.** The manifest names `GOOS` and
   `GOARCH`, and the identity's key folds the host platform, so a test result
   is not served to another operating system or architecture. The source keys
   shared one across platforms.

### What an edit of a dependency does

| Edit | Content IDs | Test of a dependent |
|---|---|---|
| A comment within a line | Unchanged | Cached |
| A comment on a line of its own, or any line-shifting edit | The edited package's, whose line table moved, and every package that inlines from it | Runs |
| A change of a body, a signature, an embedded file | The edited package's, and those whose compiled output depends on it | Runs |

A line-shifting edit re-runs every dependent that links the package: that is
correct, since the binaries changed (their line tables and inlined copies).

### Measured

`./putnamiw test --projects <the 24 go/framework projects>` on a warm cache,
then one edit of `go/framework/errors/code.go` at a time, each unique (it
embeds a timestamp) and reverted before the next; 20 of the 24 projects
depend on `go.putnami.dev/errors` in the project graph. Before is commit
`1d822313a`, after is this change, on one Linux x86_64 machine with 4 CPUs,
2026-11-28. A run of the unchanged tree re-ran no test before or after.

| Edit | `test~test` run again, before | after | Wall, before | after |
|---|---|---|---|---|
| (a) An end-of-line comment on `func (c Code) String()` | 21 | 5 | 53 s | 27 s |
| (b) A comment on line 1 | 21 | 20 | 52 s | 61 s |
| (c) A body change of `String()` on the same line | 21 | 20 | 47 s | 58 s |

- (a) After, `errors` itself runs again, its own sources being in its key,
  and so do the four dependents that plan `test-env` (`client`, `grpc`,
  `openapi`, `telemetry`), whose test step folded `describe`'s key through
  `test-env`. The 15 other dependents are served from the cache. With
  `test-env` binding the outputs of `describe` (see Consequences), measured
  the same way from `af56bdd2e`, (a) runs `errors` alone: 1 test run instead
  of 5, in 11 s of wall instead of 25 s.
- (b) and (c) After, `go.putnami.dev/cache` is served from the cache: its
  module names `errors` (a `replace`), so the project graph makes it a
  dependent, but none of its packages imports it, so no binary of it links
  `errors`.
- The identity steps ran again in the 21 projects whose closure holds
  `errors`, in 0.2 to 0.8 s each for (a), and up to 10 s for (b) and (c),
  which recompile the trimmed variants of the dependents: the walls of (b) and
  (c) are that cost.

### Rejected

- **No Go source of the project in the test key.** About 60 test files of
  this repository read Go sources as text: ratchets that count the lines
  holding a token (`tooling/cli`'s structural baseline), import and parser
  guards. A comment within a line leaves every content ID unchanged, so such
  a test would replay a verdict over a tree it never read. The cost of
  keeping the sources is the project's own test run on such an edit, one task
  of the five edit (a) re-runs in the measurement above (`errors`), against
  none of its dependents'.
- **A digest of the `go test -c` binary.** It embeds the checkout path
  without `-trimpath`, and the trimmed binary cannot run the repository's
  tests (Context).
- **An uncached identity.** It reads the environment as it is on every run,
  but costs a listing of every Go project on every run, a fully cached one
  included.

## Consequences

- The test command plans one more job per Go project. The Go case of the
  orchestration counts (`TestTheExtensionCounts`) runs 7 tasks and 6 engine
  job processes with every task executed, below its ceiling of 9, and a run of
  the unchanged workspace after it still executes nothing.
- An edit compiles twice: the identity's trimmed variant and the run's
  untrimmed one, both kept in the Go build cache. The identity of
  `go/extension` took 11.2 s cold and 1.7 s warm.
- Coverage-scope `batch` still instruments a batch's union of packages, which
  depends on the batch and on nothing a key names
  ([ADR 0008](0008-coverage-scope-decides-whose-tests-count.md)). The
  identity names the member's own instrumentation, and a dependency whose
  source changed changes its content ID instrumented or not.
- `test-env`, planned when the dependency closure commits an
  `infra/requirements.json`, depends on `describe` so it reads the
  requirements `describe` converged, which its `requirements` closure input
  keys. It binds the outputs of `describe` the test step binds, which it does
  not read, so the engine relays their digests instead of `describe`'s key
  through the action the test step folds and through what reaches the test
  step from it (CLI ADR 0043): such a project's test run does not move with
  the closure's non-test Go sources either.
- `test-race` is unchanged: it keys on the changed files and the project's
  sources, after `test`.

## Invariants

- The identity lists with the compile flags the solo run and a batch member
  pass to `go test`, and a coverage or race variant names other binaries
  (`TestIdentityListsWithTheCompileFlagsOfTheTestRun`,
  `TestBatchMemberCompilesWithTheIdentityFlags`).
- The manifest names every test main and every matched package by the digest
  of its closure's content IDs, independent of the listing's order and of
  action IDs; a dependency's content ID moves the closures that link it alone
  (`TestIdentityManifestNamesEveryTestBinaryAndPackageByItsLinkClosure`,
  `TestIdentityIsIndependentOfListingOrderAndActionIDs`).
- A broken closure or a failed listing is unresolved and never shared: the
  step reports a skip, which is never stored, and the engine publishes the
  digest of a skip's outputs to its consumers
  (`TestIdentityOfABrokenClosureIsUnresolved`,
  `TestAnUnresolvedIdentityIsReportedAsASkip`,
  `TestAnUnresolvedIdentityIsNeverStored`, and in the CLI
  `TestArtifactDigests_AnUncachedSkipPublishesItsDigestAndStoresNothing`).
- The `schema` artifact leaves the emitted feature evidence out of its digest
  (`TestTheSchemaArtifactLeavesTheFeatureEvidenceOut`).
- Against the real toolchain, the identity is the same at two checkout paths
  and after an end-of-line comment, and moves with a line-shifting comment and
  a body edit (`TestIdentityFollowsTheCompiledBytesNotTheSourceText`).
- The test step binds the identity and its producers' declared outputs, and
  folds no key but `test-env`'s; it keys on no Go source outside its project
  (`TestTestStepFoldsTheIdentityAndTheArtifactsOfItsProducers`,
  `TestTestExecKeysOnNoGoSourceOutsideItsProject`,
  `TestTestIdentityStepNamesTheBinariesBeforeTheTestStep`).
- `test-env` binds the outputs of `describe` the test step binds, and folds
  no key (`TestTestEnvStepFoldsWhatDescribeWroteNotItsKey`).
