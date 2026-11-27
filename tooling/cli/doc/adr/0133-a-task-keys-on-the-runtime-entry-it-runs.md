# ADR 0133 — A task keys on the runtime entry it runs

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `protocols/extension` (`runtime.entries`,
  `runtime.compiledInputs`, `tasks.<name>.runtimeEntry`, contract 29),
  `@putnami/cli` (`internal/jobs/cachekey`: the implementation digest;
  `internal/jobs/scheduler`: the executable a task runs), `@putnami/go` (the
  codegen entry)

## Context

Every key names the implementation of the extension that runs its task
([10-caching.md](../10-caching.md#extension-implementation)). For a
workspace-local extension with a prepared runtime, that implementation is the
runtime input digest: the declaration, every prepare input file and the
module replacement closure. One digest covers every task of the extension.

Measured at `35d1a8bc` on a 4-core Linux container (a warm
`lint,test,build,validate --all` baseline, then the change reverse-applied and
run with `--impacted --baseline HEAD`): reverting a merged edit of
`internal/codegen/describe.go` re-keyed 1,717 tasks and
1,720 task-seconds, 1,472 tasks and 1,688 s without the evidence collectors.
They included 145 Go test executions. Yet the Go runtime routes only
`build-generate` and `build-describe` to `internal/codegen`
(`cmd/putnami-go/main.go`). The other tasks re-ran because they shared one
digest with code they never execute.

## Decision

### A runtime may declare entries and compiled inputs

- `runtime.entries` names further executables beside `runtime.executable`,
  under the same rules and the same root: `{"codegen": {"executable":
  "compiled/putnami-go-codegen"}}`. At most 16, named by lowercase letters,
  digits and hyphens.
- A task names one with `runtimeEntry`. `{extensionRuntime}` in its command
  and arguments then expands to that entry, and its job context names it as
  its runtime. The member is task contract: it moves the task contract digest.
- `runtime.compiledInputs` lists prepare inputs, spelled as in
  `runtime.prepare.inputs`, whose content reaches a task only through the
  executables prepare builds from them.
- Any of the three requires CLI contract 29. An older reader would run the
  main executable for a task that names an entry, which need not serve it, so
  it refuses the stamp.

### With compiled inputs, a task keys on the executable it runs

The implementation digest of a task of such a runtime is:

- the runtime declaration, the extension name and version, the platform, the
  runtime ABI and the prepare toolchains;
- every prepare input file that some uncompiled pattern selects;
- the SHA-256 of the executable the task runs: its entry, or the main
  executable;
- the main executable too when a lifecycle hook of the extension runs it,
  because such a hook can affect a task whose own command does not.

The module replacement closure is left out with the compiled inputs: it is the
Go modules prepare compiles into the executables. A file prepare selects
through both a compiled and an uncompiled pattern stays in.

### Why it is sound

This is the rule of [ADR 0043](0043-a-cross-project-carrier-task-keys-only-on-what-it-reads.md)
applied to the extension: a task declares what its action reads, and the burden
of proof sits on narrowing.

- What the task runs is exactly the executable's bytes. Every package that
  executable links, its initializers included, is in those bytes, whatever
  source file it comes from. A source edit that changes what the task runs
  changes the bytes, so it moves the key.
- An edit that builds the same bytes leaves the task's behavior unchanged, so
  the key stays: an edit of a package the executable does not link, or a
  comment that moves no line, such as one at the end of a line or of a file. A
  comment that shifts lines changes the bytes, because positions are compiled
  in.
- Prepare builds with `-trimpath`, `-buildvcs=false`, `CGO_ENABLED=0` and an
  empty build ID (`-ldflags=-buildid=`), so the bytes follow the compiled code
  and the toolchain, not the machine or the source text: Go's build ID hashes
  the main package's sources, comments included. A build that is not
  reproducible would cost misses, never a stale result.
- What a task reads at run time from outside its executable, from
  `{extensionRoot}` or a toolchain, is an uncompiled input or a key field of
  its own. Listing such a file in `compiledInputs` is a false declaration,
  the same error as an input left undeclared.

A key computed before synchronization, by a caller that never prepares the
runtime, cannot read the executables. It keys on the runtime input digest, which
only costs it a miss against an entry the synchronized run wrote.

### The Go extension builds a codegen entry

`cmd/putnami-go` keeps one package. Its dispatch table is split in two files
behind build tags. Prepare builds `compiled/putnami-go` with the
`putnami_runtime_main` tag, which leaves the codegen jobs out, and
`compiled/putnami-go-codegen` with `putnami_runtime_codegen`, which keeps only
them, both with an empty build ID. An untagged build, the one tests and linters see, holds both. The
packaged archive builds the codegen entry as a declared executable with its tag
(`options["@putnami/go"].executables[].tags`). `build-generate` and
`build-describe` name the `codegen` entry. The manifest declares `cmd/**` and
`internal/**` compiled.

## Consequences

- An edit of `internal/codegen` that changes the codegen executable re-keys
  the generate and describe tasks directly. Their dependents still fold those
  keys through their dependency edges (ADR 0043), so a Go project's
  `test~test` and `build~infra` re-key with them; keying those on artifacts
  instead is a separate decision. The other tasks of the extension, such as
  tidy, config merge, lint and the API check, keep their keys. An edit of a
  job package the codegen entry does not link, such as `internal/jobs/test`,
  leaves the generate and describe keys alone.
- An edit of code both executables link (the shared control calls, the
  credential wrappers, the SDK) still re-keys every task when it changes their
  bytes.
- A comment that moves no line re-keys nothing; one that shifts lines re-keys
  the tasks of every executable that links the edited package.
- The first run after this change misses every task of `@putnami/go` once,
  because the digest changes shape.
- Each run hashes the executables it keys on once, about 34 MB for the Go
  extension, memoized for the run.
- Published extensions keep their tree digest: their keys already follow the
  installed bytes.

## Rejected alternatives

- **A source closure per task** (`go list -deps` of the task's handler). Every
  package linked into the one binary runs its initializers for every task, so
  a closure narrower than the binary is not sound without proving that the
  packages left out have no initialization effect. It also asks the CLI to
  understand Go.
- **Requiring contract 29 only for a task that names an entry.** A reader
  below it drops `entries` and `compiledInputs`; the manifest would then load
  with a runtime declaration other than the one the author wrote, and its
  tasks would key on a different identity on each side. The stamp covers all
  three.
- **One executable per task.** It is sound, but it multiplies the build and
  the archive by about 15 for little gain over two: the measured change touched
  the codegen half.
- **Declaring a file list per task by hand.** A file wrongly excluded serves a
  stale result silently, which is the failure ADR 0043 forbids.
