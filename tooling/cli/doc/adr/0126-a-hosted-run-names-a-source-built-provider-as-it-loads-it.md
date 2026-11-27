# ADR 0126 — A hosted run names a source-built provider as it loads it

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`: the implementation
  identity of a provider; `internal/engine`: the workspace probe's bindings;
  `internal/jobs/inputresolve`: the identity input-resolve records key on)
- **Related**: [ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)
  (a hosted run loads a source-built runtime under the digest from the
  workspace lock, and probes no toolchain before its credential is handed
  out), [ADR 0113](0113-an-input-digest-whose-witness-holds-is-not-collected-again.md)
  (the input record keys a selector's digest with the identity of its answer)

## Context

The workspace probe records each provider's answer with the identity of the
implementation that gave it, and asks the provider again when a run binds
another identity. A workspace-local provider is named by the digest of its
runtime inputs: the digest its loaded runtime carries, or, before the run
loads it, a digest computed on the spot.

A hosted run of a source workspace loads a source-built runtime under the
digest `./putnamiw --print-engine` prepared it under, which takes the
toolchain identity from the workspace lock (ADR 0055). The digest computed on
the spot before loading left the toolchains unresolved, and gave another
value. A hosted run synchronizes the probe more than once: the dependency
fetch of its implicit install, then the run itself, with different providers
loaded. Each synchronization found the other identity recorded and asked the
provider again.

Measured on 2026-11-27 on a 4-core Linux container, with `--credential-fd`
and a throwaway credential standing for a hosted run, a build of `45647bc5`
(main) against the same build with this decision:

| Measure | Main | This decision |
|---|---|---|
| `validate-workspace` on a fresh clone after `extensions prepare`, wall, median of three | 16.8 s | 14.7 s |
| Engine probe phase of that run | 4 to 5 s | 3 s |
| The same command repeated on one clone, wall, median of three | 16.6 s | 11.6 s |
| Providers asked again on each repeated run | 11 | 10 |

The ten providers still asked again are installed from the artifact store
for the dependency fetch and from their path for the run: two implementations,
which this decision keeps apart.

## Decision

In a hosted run of a source workspace, a source-built provider the run has
not loaded yet is named by the digest from the workspace lock it will be
loaded under, the digest a loaded one carries. Any other provider is named as
before: by the digest its loaded runtime carries, or by the digest computed
on the spot until it is loaded. The workspace probe and the input-resolve
records read that identity.

## Consequences

- In a hosted run of a source workspace, a source-built provider has one
  identity before and after it is loaded, so a probe answer one
  synchronization recorded serves the next.
- Outside a hosted run, a provider not loaded yet is still named with its
  toolchains unresolved; a run that loads it records the loaded digest, and
  the next run that binds the same loaded digest reads that record.
- No cache key, selection or result changes: the identity keys local records
  only.

## Alternatives considered

- **Resolve the toolchains to name a provider in a hosted run.** Rejected: a
  hosted run probes no toolchain before its credential is handed out
  (ADR 0055).
- **Let the dependency fetch probe without recording.** Not taken here: the
  fetch and the run bind different implementations of the same providers, and
  the run would still probe them; it is a separate cost of the hosted install.
