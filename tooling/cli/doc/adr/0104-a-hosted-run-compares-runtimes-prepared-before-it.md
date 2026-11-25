# ADR 0104 — A hosted run compares runtimes prepared before it

- **Status**: accepted
- **Date**: 2026-11-25
- **Amends**: [ADR 0103](0103-an-unchanged-extension-runtime-reaches-no-consumer.md),
  decision 4 (a hosted run looks the base runtime up and prepares nothing)
- **Scope**: `@putnami/cli` (`internal/commands/extensions` `extensions
  prepare`, `internal/jobs/scheduler` `PrepareComparisonRuntimes` and
  `CompareExtensionRuntimes`), feature `cli/workspace-discovery-selection`

## Context

ADR 0103 lets `--impacted` cut off the extension-consumer edge of an extension
whose prepared runtime is byte for byte the one the diff's base prepares. A
hosted run executes no repository code during the run
([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)), so it
may only look runtimes up in the artifact store, and nothing put them there:
`./putnamiw --print-engine` runs `putnami extensions prepare` before the run,
and that command prepared only the runtimes of the path extensions that serve
a provider, and only for the tree. The hosted comparison also required an
extension to be such a provider. In CI, where the gain matters, the cut-off
therefore applied to almost nothing.

## Decision

### 1. `extensions prepare` admits the runtimes the comparison looks up

After the provider runtimes, the command resolves the baseline an
`--impacted` run of the tree resolves with no `--baseline`, and its merge base
with `HEAD`. For every path extension whose `runtime.prepare` builds its
runtime and that exists at the merge base, it computes the runtime input
digest of the tree and of the merge base, each with the toolchains its own
lock pins. When they differ, it prepares, unless the store already holds it,
the runtime of the tree and the runtime of the merge base, written as raw
blobs, under those digests. Extensions whose inputs are the same at the merge
base are skipped: the hosted comparison answers them unchanged from the two
digests alone. Outside a source workspace, and when nothing changed since the
merge base, the command prepares none of these.

### 2. It is best effort

The preparation runs before the run and holds no credential, as the provider
runtimes do. A runtime it cannot prepare, or that lands under another digest
than its lock gives, is named on stderr and never fails the command: that
extension only keeps its consumers in the hosted run.

### 3. A hosted run of a source workspace compares any path extension

The hosted comparison no longer asks whether the extension serves a provider,
and runs only in a workspace whose lock records it as its CLI's source: only
there did the workspace's own launcher fill the store. Equal locked digests
answer unchanged; otherwise it looks the head runtime and the base runtime up
by those digests, prepares neither, and keeps the edge of an extension either
is missing for. Outside a source workspace it answers every extension before
it writes the base tree, and writes none.

## Consequences

- A pull request run, whose selection resolves `origin/main` as its baseline,
  finds both runtimes of every extension whose closure the change touched, and
  cuts off those whose runtime is unchanged. A run given another baseline,
  such as the green baseline of a `main` run, finds the base runtimes only
  when an earlier preparation left them in the store.
- The preparation costs one build of each such extension per side and per
  base, before the run, on the runner's Go build cache; the store keeps them
  for later runs against the same base.
- The cache key and the provider runtimes are unchanged.
