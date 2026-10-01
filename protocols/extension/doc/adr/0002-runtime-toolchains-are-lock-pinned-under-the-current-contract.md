# ADR 0002 — Runtime toolchains are lock-pinned, and stay under CLI contract 4

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `putnami.extension.json` manifest, and the CLI's job runner

## Context

`putnami.lock.json#toolchains` pins each toolchain's exact version, vendor
source and per-platform SHA-256. Without a link from that pin to the process
that runs a task, two machines build the same commit with different ambient
`go` or `bun` binaries and store different bytes under one cache key.

Naming `go` and `bun` in the CLI would put an ecosystem's tool back in the
CLI, which [ADR 0001](0001-ecosystem-profiles-declared-by-extensions.md)
forbids: adding a language is a manifest change, not a CLI change.

## Decision

1. **A runtime declares its toolchains** under `runtime.toolchains`, keyed by
   a local alias. A declaration carries a `lock` identity, an `optional` flag,
   an ordered `candidates` list, a `probe` whose trimmed output must equal the
   locked version exactly, the `environment` derived from the resolved
   executable, and `prependPath`. The vocabulary is closed and
   provider-neutral: candidates come `from` `path`, `environment`, `home` or
   `putnami-home`; environment bindings come `from` `executable`, `ancestor`
   or `literal`. The CLI interprets the shape and names no tool.
2. **Three scopes reference the aliases.** `runtime.prepare.toolchains`
   applies while building a mutable local runtime, `runtime.runToolchains` to
   every task the runtime executes, and a task's own `toolchains` adds a
   narrower requirement.
3. **Exact or nothing.** A probe that does not equal the locked version
   disqualifies the candidate: `1.2.30` never satisfies `1.2.3`. A required
   toolchain no candidate satisfies fails the run, naming the extension, the
   alias and the pin. An `optional` one publishes its executable-derived
   bindings empty, so the task chooses another operation or fails explicitly.
   The CLI never substitutes a mismatching ambient binary.
4. **Pinned tools win every lookup.** The resolved directories come first on
   the child `PATH`, in declaration order, ahead of the CLI's own directory
   and every inherited directory; each inherited directory is kept once. The
   environment bindings pin the tool a second time for tools that read a root
   rather than `PATH`. A `home` candidate resolves through `os.UserHomeDir`.
   An `environment` candidate reads its variable from the CLI's environment
   with `PUTNAMI_WORKSPACE_ROOT` set, so an extension that installs its
   toolchain inside the workspace declares that location as a candidate.
5. **Bootstrap resolves to the unavailable identity instead of failing**, in
   three cases:
   - the workspace has no lock document (`putnami install` and
     `putnami deps install` write it);
   - an alias that only `workspace-install` needs in the run is not pinned,
     has no digest for the host platform, or matches no candidate;
   - the prepare toolchains of an extension declared by path, in a run whose
     every command is `workspace-install`.

   The unavailable identity keys the run and digests the prepared runtime
   apart from every pinned run, so a bootstrap artifact is never served to
   one, and `workspace-install` is not cached. A lock that exists and does
   not pin an alias another command requires stays a hard failure; a job of
   any other command refuses a required alias the run resolved unavailable.
   `putnami install` and `putnami init` end with the refresh that records the
   pins, and the implicit first-use install adds a declared pin the lock lacks.
6. **The resolved identity is a cache-key input.** It is the SHA-256 of the
   declaration, the locked version, the host platform, the platform integrity
   and availability: portable, never a host path. It is folded into the task
   cache key and the prepared runtime's artifact digest. Runtime and prepare
   aliases resolve during runtime synchronization, before selection, planning
   and every key. A task alias the lock does not satisfy resolves once the
   plan is final, before any job runs or reaches the cache, and fails the run
   only when a planned job needs it.
7. **The fields enter under CLI contract 4; `CurrentContract` does not move.**
   The CLI loader decodes permissively, so a reader that predates the fields
   would drop them and build with ambient tools. That reader does not exist:
   every reader of a manifest carrying these fields is a CLI built from this
   repository, and the lock pins the CLI (`putnami.lock.json#cli`) together
   with the extensions and toolchains. A manifest member that a reader outside
   that set must not drop takes an additive contract
   ([ADR 0006](0006-agent-content-is-an-additive-contract.md)).

## Rejected alternatives

- **Increment `CurrentContract`.** Every published extension stamped 4, and
  every runtime handshake, would stop matching, for a message no reader needs.
- **A sibling toolchains document an old reader never opens.** It ends "the
  manifest is one closed document".
- **Name `go` and `bun` in the CLI.** It recreates the coupling ADR 0001
  removed.
- **Drop every inherited `PATH` directory that offers a pinned name.** A
  shared `bin` directory holds hundreds of unrelated tools: dropping the one
  that holds `go` also dropped `gpg`, and signed commits in test fixtures
  failed. `PATH` cannot hide one file of a directory, so ordering keeps the
  pinned tool first and co-located tools reachable. A child that rebuilds
  `PATH` from scratch escapes either design.

## Consequences

- No extension folds an ambient tool version into its cache key; every task
  folds the identity of the toolchains it declares. Changing a declaration,
  such as adding a candidate, changes that identity once.
- A task resolves the bare name of a pinned tool to the pinned executable. An
  ambient copy can still exist later on `PATH`.
- A workspace whose lock lacks a pin or a host-platform integrity for a
  required alias fails at synchronization, not at an arbitrary task.
