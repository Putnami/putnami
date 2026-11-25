# ADR 0105 — A toolchain that names its own executable is recorded

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [ADR 0102](0102-planning-reads-recorded-facts.md), decision 2
  (a toolchain probe answers once per binary identity) and its rejected
  alternative "Have each toolchain name its own executable"
- **Scope**: `@putnami/cli` (`internal/jobs/toolchain`), the runtime
  toolchain probe of `go.putnami.dev/protocol/extension` (ADR 0002 of that
  module), CLI contract 26 of `go.putnami.dev/protocol/cli`, the first-party
  manifests that declare a toolchain probe, feature
  `cli/job-planning-execution`

## Context

ADR 0102 records a toolchain probe's answer across runs only for a release the
Putnami home's `toolchains` hold. Any other executable is probed once per run,
because a native dispatcher (a proto or mise shim, the dotnet muxer, an Xcode
`/usr/bin` shim) answers for the toolchain its configuration names, and no
file identity follows that configuration. The CLI names no language, so it
cannot tell such a dispatcher from the toolchain itself.

`./putnamiw lint,test,build,validate --all --dry-run` on this repository at
`740e905d`, measured on 2026-11-25 on a 4-core Linux container whose Go, Bun,
PHP and uv come from the system, starts four toolchain probes on every run,
after two runs that wrote every record: `go env GOVERSION`, `bun --version`,
`php -r 'echo PHP_VERSION;'` and `uv self version`. The Go and Bun there do
not match the lock's pins, so a Putnami home release answers after them from
its record; the system binaries are probed again on each run all the same.

A toolchain can say which file it is: `go env GOROOT` names the root whose
`bin/go` is the running Go, and Bun's `process.execPath` names the running
executable, both read from the file the kernel runs. A dispatcher, which runs
the toolchain as a child or execs it, gets an answer that names the toolchain,
another file. An answer read from `argv[0]` does not: PHP's `PHP_BINARY` on
Unix is the real path of `argv[0]`, so a compiled dispatcher that execs PHP
with its own argv makes PHP name the dispatcher, measured on 2026-11-25 with a
Go passthrough dispatcher whose PHP 8.3.6 answer was recorded and served
after its target moved to another release.

## Decision

1. **A probe may declare how the binary names itself.** A runtime toolchain's
   probe may declare `self`: the `args` of a second probe, whose one-line
   answer is an absolute path, and an optional relative `path` joined to that
   answer. The executable the answer names is `<answer>/<path>`, or the
   answer itself without a `path`. The version probe that decides whether
   the candidate qualifies runs in the CLI's working directory, as before,
   and its identity names that directory: a version manager's shim (asdf,
   goenv, nodenv, proto) walks up from it to the version the workspace pins.
   The check runs in one empty directory of its own, with the probe's
   variables, set and unset: the `self` probe, then the version probe again.
   A configuration file of the working directory, such as a `bunfig.toml`
   whose preload `bun --print` runs, is code the repository controls, so
   the check runs none.
2. **The engine checks it once, when it records.** A candidate outside the
   Putnami home's toolchains whose probe declares `self` is checked when no
   record answers for its binary identity: after its version probe succeeds,
   the engine runs the check. When the executable the `self` answer names,
   with its symbolic links resolved, is the same file as the candidate's,
   with the same file identity, and the version probe of the empty directory
   answers what the working directory's did, the version answer is recorded
   as a home release's is, under the binary identity, which also holds the
   `self` declaration. Otherwise the engine records that the binary names
   another file, with no answer: a dispatcher that selects its target in the
   working directory, or a binary whose version follows it, as `go` without
   `GOTOOLCHAIN=local` under a `go.mod` that names another release, does not
   answer from a record when the two directories answer differently during
   the check, and a later run probes its version once per run and checks it
   no more. The check sees a dependence on the working directory only while
   it shows: a binary recorded while both directories agree keeps its answer
   after the working directory changes, so a probe that declares `self` must
   answer a version the working directory does not select (decision 5). A `self` probe that fails, answers no absolute
   path or names no file counts as naming another file, so a binary that
   cannot name itself costs no second process on later runs; one that times
   out, or a binary that moved while it ran, records nothing. A script, a
   file with several hard links and a candidate whose probe declares no
   `self` keep ADR 0102's rule: probed once per run.
3. **A record states which rule wrote it.** A record holds the answer of a
   home release or of a binary that named itself, or the verdict that the
   binary names another file. A reader takes an answer only from a record
   whose key is the identity it computed; a verdict never answers a version.
4. **The member is contract 26, and raises no floor.** From 26 on, the CLI
   reads `probe.self`. A reader below 26 drops a member it does not know and
   probes the binary once per run, as before, with the same answer, so a
   manifest that declares `self` keeps the stamp its other vocabulary
   requires and loads in every reader of that stamp.
5. **Each first-party manifest declares the self of the toolchains whose
   executable holds their version and whose answer names the file the
   kernel runs.** Go: `env GOROOT` and `bin/go`; Bun: `--print
   process.execPath`. PHP declares none: `PHP_BINARY` is read from `argv[0]`.
   Ruby declares none: a Ruby built shared answers `RUBY_VERSION` from
   `libruby`, so `RbConfig.ruby` names a launcher whose file does not follow
   the version. uv names no executable of its own and declares none. Declare
   `self` only for a binary whose own file holds the version it answers,
   whose answer does not come from `argv[0]`, and whose version the working
   directory does not select, as Go's under `GOTOOLCHAIN=local`.

## Rejected alternatives

- **Keep the check out of the record.** Running the `self` probe on every run
  that reads a record costs the process the record saves.
- **Run the check on every run for a binary that names another file.** A
  dispatcher would then start two probes per run instead of one; the verdict
  record keeps it at one, as before.
- **Raise the manifest's floor to 26.** An older reader probes the binary once
  per run and keys on the same answer; refusing the manifest would cost every
  older CLI the extension for no difference in what it runs.
- **Name the executable from the version probe's output.** Each toolchain
  prints its version its own way; a second probe keeps the version answer
  exact (ADR 0002, decision 3).

## Consequences

- Planning an unchanged workspace whose system toolchains declare `self`
  starts their probes on the first run only. On the machine measured above,
  the PHP and uv probes remain, once per run.
- A dispatcher whose target changes between runs is probed again on each run
  and answers the new version.
- The orchestration counts of the `tooling/cli` e2e orchestration tests run
  their extension cases on the machine's pinned Go and Bun where they lie,
  with no copy in a Putnami home, and their plans still start their git calls
  alone.
- A record written before this decision names no `self`, so the first run of
  a manifest that declares one probes its home releases once more.
- `self` is part of the toolchain's declaration, which the toolchain identity
  of runtime and task cache keys holds, so an older CLI and a CLI at 26 key a
  manifest that declares it differently.
- An older CLI loads such a manifest, but its strict validation (`putnami
  extensions validate`, an extension's packaging) refuses the member it does
  not know.
- Recording a system toolchain costs its check once: the version probe,
  then the `self` probe and the version probe again in an empty directory. A
  run whose artifact store is empty, short-lived or read-only pays it on
  every run, three probes where it started one.