# ADR 0102 — Planning reads recorded facts

- **Status**: accepted, amended by
  [ADR 0105](0105-a-toolchain-that-names-its-own-executable-is-recorded.md)
  (decision 2)
- **Date**: 2026-11-25
- **Amends**: [ADR 0078](0078-the-core-asks-extensions-to-expand-their-input-selectors.md),
  decision 1 (one resolver per run asks and keeps)
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`,
  `internal/jobs/toolchain`, `internal/jobs/inputresolve`,
  `internal/artifactstore`, `internal/store`, `internal/engine`), feature
  `cli/job-planning-execution`, the input-resolve contract of
  `go.putnami.dev/protocol/workspace` (ADR 0004)

## Context

Planning an unchanged workspace asks again what it asked on the run before.
`./putnamiw lint,test,build,validate --all --dry-run` on this repository at
`fe1a0430`, measured on 2026-11-25 on a 4-core Linux container with a valid
workspace index, took 4.0 s and started 131 processes under `strace`: 39
`go env GOVERSION`, 19 `__putnami runtime-info`, 8 `bun --version`, two PHP
and one `uv` version probe, and 9 git calls. The epic's dry run of
2026-11-24 started 144, with 17 workspace probes before the index was
written. A run that keys its tasks also starts one `__putnami input-resolve`
per extension that lists an input selector (ADR 0078).

Each of those answers is a function of something the run can read without a
process: the runtime-info answer of an artifact-store entry, whose payload
never changes; the version a native executable prints, while the file is the
same file; an extension's input-resolve answer, while the extension, the
request and the tree its basis selects are the same.

## Decision

1. **A runtime handshake is recorded beside its store entry.** After a
   `runtime-info` handshake passes for an executable inside an
   artifact-store entry, the store records the answer in the entry's
   `runtimeinfo` sidecar, with the executable's path inside the entry and its
   file identity: size, modification and change times, device, inode and
   link count, taken before the handshake started. Nothing is recorded when
   the identity moved while the handshake ran, and the record is written
   under the store's shared lock. A later load of a runtime in that entry
   reads the record instead of starting the handshake while the record names
   the same executable with the same identity and states the identity the
   run expects: extension, version, platform, CLI contract, runtime protocol
   and ABI. A record that does not match is ignored, and the handshake that
   follows replaces it. A failed handshake records nothing. A runtime outside
   the store is always handshaken.
2. **A toolchain probe answers once per binary identity.** A binary identity
   is the resolved executable's path and file identity, the probe's
   arguments, the variables it sets and unsets, and the working directory.
   The run keeps each answer in memory under that identity and the whole
   probe environment, so a run probes each executable once; the memory lasts
   for that run alone. A release the Putnami home's `toolchains` directory
   holds, both by the candidate's path and by the file it resolves to, is also
   recorded in the artifact store's
   `toolchain-probes` directory, without the rest of the environment: the
   engine unpacked it from the archive the workspace lock pins, and it
   answers its own version. Any other executable is probed once per run,
   since a shim or a dispatcher answers for the toolchain its configuration
   names and no file identity follows that configuration. A script and a
   file with several hard links are never recorded either. A probe that
   fails or times out is never kept.
3. **An input-resolve answer is recorded between runs.** Decision 1 of
   ADR 0078 keeps an answer for the run. The resolver also records each
   answer it keeps in the artifact store's `input-resolve` directory, one
   record per workspace root and request (project, selector and sorted
   patterns). The record states the input-resolve protocol version, the
   extension's implementation identity (its prepared runtime's digest, its
   store entry's digest, or the version of a registry-installed extension)
   and the workspace's probe digest. A later run reads it while all three
   are the run's, the digest of its basis holds and every file it lists is
   there; a newer answer replaces it. The probe digest moves when a project
   or an edge moves, as when a directory becomes a project of its own: an
   answer can follow the projects around its own, which its basis does not
   select. An extension without an implementation identity is not recorded.
4. **A record is a cache.** Every record is written by an atomic rename, read
   only when it parses whole under its schema, and ignored otherwise. Losing
   one costs the process it replaced. Artifact-store GC removes a
   `toolchain-probes` or `input-resolve` record idle past `MaxIdle`; a read
   stamps a record older than a day, so a record runs still read stays.

## Rejected alternatives

- **Key a handshake on the executable's content digest.** Hashing a runtime
  of tens of megabytes on each load costs about what the handshake costs.
  The store entry is already content-addressed.
- **Key a probe answer on the whole environment.** The environment holds
  values that change on every run, so no record would ever be read again.
  The probe's own variables are part of the key; a native executable with one
  link answers its own version, whatever the others hold.
- **Keep the records in the workspace snapshot.** The snapshot is not
  written by a dry run, and a handshake or a probe answer belongs to the
  machine, not to one workspace.
- **Keep the input-resolve records in the per-repository build store.** Its
  root is named by the repository's Git common directory, which costs a git
  call to find; the artifact store root costs none. The artifact store sits
  outside the workspace unless the machine has no home directory, so a basis
  selects no record.
- **Record any native executable with one link.** A native dispatcher, such
  as a proto shim, the dotnet muxer or an Xcode `/usr/bin` shim, would answer
  from its record after a configuration switch, so a run would key on a
  toolchain it does not run. A system toolchain is therefore probed once per
  run: planning this repository on a machine whose Go, Bun, PHP and uv come
  from the system starts about five probes.
- **Have each toolchain name its own executable.** A probe that also names
  the executable or root it runs, such as `go env GOROOT`, would let a system
  toolchain be recorded too. It needs a declaration per toolchain in the
  extension manifest and a contract rung, so it is a change of its own.

## Consequences

- Planning an unchanged workspace whose toolchains are all Putnami home
  releases starts its git calls alone: the orchestration counts of the
  `tooling/cli` e2e orchestration tests say so for each extension they hold.
  Each system toolchain adds its probes, once per run.
- A hosted run that loads a source-built runtime the store holds starts no
  handshake when the store recorded one: it starts fewer programs, never
  others.
- The artifact store treats `runtimeinfo` as bookkeeping, as it treats
  `implementationdigest`: an installed extension's implementation digest and
  a template's rendered files never read it. Normalizing a store drops every
  record, since each names this host.
- The orchestration counts run their extension cases in a Putnami home of
  their own, whose `toolchains` hold copies of the machine's pinned Go and Bun
  executables and whose artifact store keeps the runs' records, so a plan's
  probes are read from records whichever directory the machine keeps those
  releases in. A run keeps probe answers in memory for itself alone, so the
  plan a test counts after other runs in its process reads the records.
