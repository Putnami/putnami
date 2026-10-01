# ADR 0008 — Keep executable architecture semantics in the protocol

- **Status**: accepted
- **Scope**: the architecture engine of `@putnami/sdd`
  (`tooling/sdd-extension/internal/sdd`), the `putnami architecture` command
  group, the `architecture-validate` task, and the `sdd.architecture_context`
  MCP tool

## Context

Architecture Rules as Code (ARC) and Domain Access & Replication Contracts
(DARC) must give CI, local review, agents, and Putnami Cloud identical answers
from the same committed declarations. Any second interpretation drifts.

The features and specs verticals already separate durable protocol meaning
from filesystem and workspace mechanics. Architecture needs the same boundary.

## Decision

### 1. The public protocol owns meaning

`go.putnami.dev/protocol/architecture` owns strict wire parsing, local and
repository validation, canonicalization, graph aggregation, declared/observed
comparison, stable finding IDs, baseline/waiver classification, and the
snapshot shape. It is hermetic: no filesystem, Git, workspace, or Cloud
dependency. Tooling may source inputs; it may not reinterpret them.

### 2. One extension engine owns local evidence acquisition

The engine performs one contained recursive discovery of exact
`putnami.architecture.json` filenames, reads the workspace-root
`architecture.baseline.json` and `architecture.waivers.json`, reads project
membership and resolved direct dependency edges from the job-context wire
(`workspaceProjects`, `ProjectRef.dependencies`), and asks the protocol to
build the result. It never loads the workspace itself
([ADR 0013](0013-sdd-as-a-standalone-extension.md), decision 3).

Discovery skips hidden, generated, dependency, fixture, vendor, and tool-state
directories and never follows a manifest symlink. Reads and manifest counts are
bounded. A provider view that would make project-edge evidence incomplete fails
closed instead of publishing a falsely clean result.

### 3. Evidence comes from committed files only

Project dependencies are compared only between projects that domain manifests
map explicitly; the engine never infers a domain from a path or a name.
Framework evidence comes from the committed `schema/capabilities.json` of each
mapped project, declared as a task input, never from running a build: its
`domainAccess` rows, and their API and event transports, raise coverage to
`framework-evidence`, never to `observed`. Database coverage stays
`not-detected`. The protocol owns what each coverage value means
([architecture ADR 0001](../../../../protocols/architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md)).
The `go.putnami.dev/protocol/architecture` support entry stays `experimental`.

### 4. Baseline comparison is interactive only

The interactive `architecture validate`, `snapshot`, and `inspect` resolve the
selected Git baseline (the global `--baseline` override or Putnami's default),
read the prior `architecture.baseline.json` from that immutable commit, and
hand both versions to the protocol's shrink-only ratchet. No command checks
out, stages, or rewrites a debt file. No current baseline file means adoption has not begun.

The cached `architecture-validate` task and the `sdd.architecture_context` tool
are worktree-only: they resolve no Git ref, and `baseline.compared` stays
false. A resolved commit is not in a cache key or a tool wire, so a verdict
that depended on it would rest on state nothing names. Dropping the comparison
is strictly stricter: nothing is excused as accepted debt.

### 5. One evaluator behind every read surface

- `architecture validate`: admission verdict, diagnostics, findings, detector
  coverage, ratchet counts, baseline provenance.
- `architecture snapshot`: the complete deterministic projection.
- `architecture inspect <domain-id>`: one exact manifest-minted domain, its
  declarations, and its touching edges.
- `sdd.architecture_context`: the same inspection projection for an agent. It
  requires one exact domain id and a resolved workspace selection. Malformed
  arguments or an unresolved selection fail before evaluation with no report;
  an invalid repository or unknown domain fails after it, with the typed report
  attached.

All four call `EvaluateWorkspace` and return typed ResultV2 payloads. No
surface adds a parser, graph builder, workspace loader, or selection resolver.

### 6. Authoring writes only what needs no decision

`architecture init` creates one canonical manifest for a new domain with
`O_EXCL`, carrying only the id, owner, and selected projects. `architecture
sync` drops stale project entries and unobserved bindings, and proposes a
binding only for an import the consumer domain already declares; it writes
nothing without `--apply`. Neither writes an import, an export, or a project
membership: each is an agreement between domains. Neither is a step of
`validate`, because a gate does not write into the source tree.

### 7. The workspace chooses the automatic adoption mode

`architecture-validate` reads `options.sdd.verification.architecture` from the
committed `putnami.workspace.json`, which is a declared input of the task.
`enforce` blocks on findings; `report` publishes the same findings as warnings
and keeps an otherwise successful exit; `off` skips only this automatic
evaluation. Parse, config, and incomplete-provider-view errors fail closed in
every mode. The task result carries the effective mode, its provenance, and
whether evaluation ran. The explicit commands ignore the mode.

## Rejected alternatives

- **Implement the protocol in Cloud and mirror it here.** Two parsers and two
  finding vocabularies drift, and local CI could not prove parity.
- **Put filesystem discovery in the protocol.** Wire validation would depend
  on repository layout and lose hermetic conformance tests.
- **Authorize by domain pair.** One legitimate binding would silently permit
  every later dependency between those domains.
- **Generate or grow the baseline from `validate`, or imports from `sync`.** A
  validator that writes its own exceptions makes new debt indistinguishable
  from accepted debt.
- **Cloud access during local validation.** Declarations and the resolved
  local graph suffice; a network dependency makes review non-reproducible.

## Consequences

- Any Putnami workspace can author and check ARC/DARC without Cloud.
- A new detector widens coverage without changing the meaning of existing
  declarations.
