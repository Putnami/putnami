# ADR 0043 — A cross-project carrier task keys only on what it reads

- **Status**: accepted
- **Scope**: task input declarations, `internal/store` (project config digest),
  `protocols/extension` (`optionNamespaces`, the `fold` of a task input port)

## Context

A dependent reaches its dependencies through a carrier edge such as
`^describe`, and its own `compile` and `test-exec` sit behind that edge. Every
byte a carrier's key reads therefore moves the keys of every dependent's build
and test closure ([upstream hash propagation](../10-caching.md#upstream-hash-propagation)).
When `build-describe` declared `**/*.go`, one blank line in a `_test.go` file
of `go/framework/http` re-ran ten tasks in projects that do not contain the
file: 7.4 s of warm wall became 2.2 min.

## Decision

### A task declares the files its action reads, and nothing else

The rule is load-bearing on a task that carries a cross-project edge.
`build-describe`, `build-compile` and `build-generate` declare
`["**/*.go", "!**/*_test.go"]`: `go build` never compiles a test file, and
describe's host binary is a `go build` of the main package. `test-exec` keeps
`**/*_test.go` because it runs them.

The burden of proof sits on narrowing. A file wrongly EXCLUDED serves a stale
verdict, silently. A file wrongly INCLUDED costs a rebuild, loudly. So a
declaration is narrowed only against the action's own code (here
`ModuleHasMain` and `usesPutnamiApp` skip `_test.go` as `go build` does).
"It probably does not read that" is not an argument.

### The value folded into a dependent's key stays the dependency's cache key, except on an edge that declares an artifact input

A dependent folds each dependency's cache key, except on an edge that declares
an artifact input: a `from: "task"` input port with `fold: "artifact"`, which
its step binds to an output of the producer (`fromStep` and `output`, or
`^step` for that step in every direct dependency). That edge folds the digest
of the artifact the producer wrote instead, and not the producer's key:

- **The consumer reads the artifact, not the producer's inputs.** A test run
  that executes the JavaScript its dependencies emitted, or a test binary,
  reads those bytes and nothing else of the producer. Folding the producer's
  key re-runs it for every producer edit that writes the same bytes.
- **An artifact that differs costs a rerun, never a stale verdict.** The
  digest is of the bytes the consumer reads: a file's bytes, a directory's
  sorted relative paths, executable bits and content digests, a fixed empty
  value for an output that holds nothing. Any byte that changes moves the
  consumer's key, except the project's build stamp, `.gen/version.json`, which
  no artifact digest reads: the scheduler rewrites it at the current identity
  after every restore, so a cached verdict already replays against another
  stamp, and no cache key folds it, since a task whose output embeds the
  version folds the version itself. A producer's output port may also name
  subpaths of a directory artifact that no consumer's verdict reads
  (`artifactExcludes`), which its digest leaves out.
- **An artifact that is not what the consumer reads is a manifest error**,
  the same fault class as an undeclared input. A consumer that also reads the
  producer's sources, or a file outside the bound output, must declare those
  as inputs of its own or bind no artifact.
- **A keyless consumer relays the digest too.** A consumer whose cache is off
  publishes no key, and a dependent folds what it stands for
  ([ADR 0097](0097-a-project-names-what-a-command-waits-for.md)): the action
  of an invocation producer, or what reaches a keyless step of the
  dependent's own project. On an edge where that consumer binds an artifact,
  what it relays is the digest, not the producer's key. No edge only orders,
  so a keyless step that waits for its producer to rewrite a file it reads
  declares that file as an input of its own and binds the producer's declared
  outputs: an artifact it does not read costs a rerun when its bytes change,
  never a stale verdict. Go's `test-env` binds what `describe` writes and keys
  the `infra/requirements.json` it reads.

The digest is known once the producer ran or was restored, or, before that,
when the producer's local entry at its precomputed key recorded it, which is
the digest its hit publishes. A source-bound producer's entry is read at that
key bound to its project's current source binding, where its lookup reads it. A key computed before the producer ran folds that
recorded digest, and otherwise the producer's key on that edge. The consumer
is never decided on it: the producer may still execute, so it computes its key
when it starts.

The general rule stands for every other edge, for these reasons:

- **Not the output digest.** Under `go.work` a dependent compiles its
  dependencies' sources, so an implementation change with a byte-identical
  `describe` output would serve a stale test verdict.
- **Not a closure over the dependencies' sources.** Go reads more than `.go`
  files: `//go:embed` admits any path, including `.gen/version.json`. A closure
  narrow enough to skip config would skip generated bytes; one wide enough
  would hash `.gen/**` before this run's generator writes it.

The dependency's key is derived from its own declared inputs, is known before
it runs, and moves exactly when its output can move. `PrecomputeKeys`,
`CacheKeysRecomputableAtRevision` and the selection fingerprints depend on
that.

### A project config is hashed through the reading task's scope

A project config holds one `options` block per addressee. The digest keeps the
identity fields, every namespace it cannot attribute to a single extension,
and the reading extension's own layers. It drops the layers spelled for another
extension: `options.<ext>` and `options.<ext>:<command>`, where `<ext>` is a
package name or a workspace path reference.

- **Attribution is declared, never inferred.** A bare namespace is dropped only
  when a manifest claims it in `optionNamespaces` (`@putnami/sdd` declares
  `sdd` and `publish`). An undeclared namespace stays in every key; one two
  manifests declare stays in both. A COMMAND name is never attributed: the CLI
  merges `options.<command>` into the parameters of every extension providing
  that command. Declaring a namespace never widens the declarer's keys; it
  narrows everyone else's, so the declaration belongs to the reader.
- **Scoping reads content, not the file set.** Adding or removing a
  `putnami.json` or `.putnamirc.json` still moves the key: which files an input
  selects is part of the key. `.putnamirc.json` stays a declared input of
  `build-generate` and `build-describe` and goes through the same scoped digest.

## Consequences

- A `_test.go` edit re-runs the owning project's `test` and nothing else.
- A cross-project edge in any extension must follow the same rule. TypeScript
  complies because `build-generate` reads `src/**` and `test-run` reads
  `test/**`; a `*.test.ts` colocated under `src/` would reopen the hole.
- A dependency's `putnami.json` still re-keys its dependents when the edit lands
  in a namespace the digest keeps, because a generated artifact can depend on
  any of it. The project-config scope, not the edge, is where that narrows.
- A consumer that binds an artifact keeps its key across a producer edit that
  writes the same bytes. Its key before its producers ran is not the one it
  executes under, so a remote negotiation records no key for it and its hit is
  decided when it starts (CLI contract 32, `ArtifactInputsContract`).
