# Agent-context orientation benchmark

This benchmark is the dogfood evidence for the protocol's premise: an agent
oriented by one `putnami context pack` document (or one read-only MCP
`agent_context` call) makes **fewer orientation reads / round-trips** than one
that reconstructs the same facts by hand from the filesystem — on at least one
Go and one TypeScript task.

## Methodology

"Manual orientation" is the number of filesystem round-trips an agent makes to
reconstruct, by hand, the facts a `putnami context pack` document already
aggregates for a project. At minimum that is:

- **1** directory enumeration (discover what is in the project),
- **K** reads — one per referenced committed file (composition roots,
  capability/contract/infra/migration references, representative sources,
  adjacent docs, config schema), and
- **1** dependency-graph reconstruction from the manifests,

i.e. **≥ K + 2** round-trips per project. The read-only MCP `agent_context` tool
returns all of it in exactly **1** structured call, after which the agent reads
only the specific source **ranges** it actually needs.

`K` is the count of **distinct** workspace-relative file paths the manifest
references. Structural counts (`K`, graph deps `D`, composition roots) are stable
across commits; the digests and workspace revision embedded in the artifact are
not, and are deliberately not part of this table.

## Reproduce

```bash
bash protocols/agentcontext/bench/orient-bench.sh
```

The script locates the repo root from its own path, runs
`putnami context pack --project <id>` for a fixed, ordered list of Go + TS
samples (`unit-of-work-proof`, `task-api`, `capabilities-proof`, `06-database`,
`14-capabilities`), parses each emitted `<project>/.gen/agent-context.json` with
`python3` (not `jq`), and prints the table below. It is deterministic in output
order and exits 0 on success.

The emitted `.gen/agent-context.json` artifacts are **ephemeral and gitignored**
— they are never committed (verify with
`git check-ignore <project>/.gen/agent-context.json`).

## Results (structural)

Two headline samples, one per language:

| Sample | referenced files K | graph deps D | composition roots | tests | manual round-trips (≥ K+2) | `agent_context` |
| --- | --- | --- | --- | --- | --- | --- |
| Go `unit-of-work-proof` | 4 | 16 | 1 | 2 conformance packs | 6 | 1 |
| TS `06-database` | 4 | 4 | 1 | no-packs | 6 | 1 |

The full sample set the script prints:

| Sample | K | D | roots | tests |
| --- | --- | --- | --- | --- |
| Go `unit-of-work-proof` | 4 | 16 | 1 | 2 conformance packs |
| Go `task-api` | 4 | 26 | 1 | no-packs |
| Go `capabilities-proof` | 1 | 14 | 0 | unsupported-project-type |
| TS `06-database` | 4 | 4 | 1 | no-packs |
| TS `14-capabilities` | 3 | 3 | 1 | unsupported-project-type |

## The sparse `capabilities-proof` case (honest note)

`capabilities-proof` looks under-populated (K = 1, no composition roots), and
that is **correct behavior, not a bug**. Its capabilities are
describe-generated: there is no committed `schema/capabilities.json`. The
agent-context artifact aggregates only **committed** facts by reference, so with
nothing committed to point at, a sparse manifest is the honest result. An agent
still gets the identity + dependency graph in one call and learns, from the
`unsupported-project-type` tests absence reason, that there are no conformance
packs to run.

## Acceptance

This benchmark demonstrates the protocol's acceptance statement: agent-context
orientation costs **fewer orientation reads / round-trips than manual
reconstruction on at least one Go and at least one TypeScript task** — here Go
`unit-of-work-proof` (6 → 1) and TS `06-database` (6 → 1), with every other
sample showing the same direction.
