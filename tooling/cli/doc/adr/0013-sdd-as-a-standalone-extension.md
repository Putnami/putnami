# ADR 0013 — SDD is a standalone extension, not part of the CLI

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`) and `@putnami/sdd`
  (`tooling/sdd-extension`): the `features`, `specs`, `architecture`, and
  `contracts` command groups, their MCP tools, and the `validate` /
  `validate-workspace` jobs

## Context

Specification-driven development (SDD) covers authored features, durable
specs, executable architecture, and generated contracts. Compiled into the CLI,
it cost every workspace binary size, help output, and completion entries, even
a workspace that authors no `putnami.features.json`. It also could not be a
gate: the planner cannot schedule the CLI's own built-in commands as jobs
without a special case.

Architecture meaning already lives in `go.putnami.dev/protocol/architecture`
([ADR 0008](0008-executable-architecture-command-boundary.md)), and spec
meaning in `go.putnami.dev/protocol/features`. What remains is evidence
acquisition, which has no reason to live in the host binary.

## Decision

### 1. SDD ships as `@putnami/sdd`, an ordinary first-party extension

`tooling/sdd-extension` owns the four command groups, the five MCP tools
(`sdd.list_features`, `sdd.feature_context`, `sdd.list_specs`,
`sdd.spec_context`, `sdd.architecture_context`), and the `validate` and
`validate-workspace` commands. `@putnami/cli` contains no SDD engine, command,
or MCP tool. Core keeps only the orchestrator's selection guards for extension
tools: `projects` and `impacted` are mutually exclusive, and an unresolvable
selector fails instead of narrowing silently.

The extension is ordinary in every mechanical sense: discovered from
`putnami.workspace.json`, prepared by `bin/prepare`, validated like any other
manifest, and planned with no special case. Only its maintainer is first-party.

In a workspace that does not declare it, the four command groups do not exist
and help does not list them. When a run plans zero jobs for one of them, the
CLI prints a hint naming `@putnami/sdd`. The hint changes no exit code and
stays silent when the extension is loaded.

MCP tool names are dotted, with no undotted alias. The CLI drops an extension
tool whose name has no dot, and core refuses an unknown tool with `-32602`.
Generated agent guidance names the dotted spelling.

### 2. Architecture ships inside `@putnami/sdd`, not as its own extension

Architecture is a separate protocol with a separate audience, but the four
verticals share one workspace view (`internal/wsview`), one git reader
(`internal/gitread`), and one result-envelope path. A separate extension would
duplicate all three and keep the copies byte-compatible.

A future split stays a manifest and module operation: a second
`putnami.extension.json`, a second `cmd/` main, and a shared module for
`wsview`, `gitread`, and rendering. It does not touch `protocols/architecture`.

### 3. The extension gets workspace knowledge from the wire, never from a loader

The extension may not import `go.putnami.dev/tooling/cli/internal/...`, may not
`replace` into `tooling/cli`, may not copy the CLI's workspace loader, and may
not exec `putnami` at run time. Everything it knows about the workspace arrives
on a wire: the job-context document for a job or interactive subcommand, the
`ToolCallRequest` for an MCP tool.

A second loader is a second definition of workspace membership, and a
validator that disagrees with the orchestrator produces findings nobody can
act on. Re-deriving `--impacted` would add a second baseline resolver, owner
map, and dependent walk.

`internal/wsview` is the shape the engines read, not a loader. The wire
carries the complete membership with direct dependency edges
(`workspaceProjects`), each sibling's raw `putnami.json`, `ProjectRef.version`,
the run's selection, and the committed workspace option blocks. A view built
only from a narrowed selection raises `workspace.provider_view_unavailable`,
and a workspace-scoped task fails closed rather than report a subset as the
whole. A fact the extension needs and the wire lacks is an additive, versioned
change to `protocols/job` or `protocols/extension`, never a private read. A
task that depends on a committed file declares that file as a cache input.

A job that runs green while blind to what it exists to catch is worse than no
job, so each wire gap is proven by a measurement, not by inspection.

### 4. `features-validate` is uncached; `specs-validate` is cached

A task may be cacheable only when its declared inputs cover what it reads. An
under-declared key restores a stale verdict while reporting that the check ran.

`features-validate` declares `cache: {enabled: false}`. Its verdict depends on
evidence source bindings (arbitrary bound files and their git blob state),
which no file pattern can express. Caching it would need a key computed after
the bindings resolve: the binding set plus each bound blob's digest.

`specs-validate` is cached. It reports on one project but reads the whole
workspace's authored identities, because "one spec per feature" is a
workspace-wide guarantee. Its key therefore carries two project ports and a
workspace-wide identity port (`**/putnami.features.json`, `**/specs/*.json`),
so a sibling's manifest cannot flip the verdict silently. Its
executable-criteria projection is a declared output, so a hit restores it
byte-identically; a hit that lost it would disarm the spec gate.

`architecture-validate` is cached and worktree-only, with the workspace policy
file in its key (ADR 0008, decisions 4 and 7).

### 5. `contracts` stays interactive-only

`contracts generate` and `contracts check` are not steps of `validate`. `check`
has its own exit 2 for drift and breaking changes, which the job exit-code
contract cannot carry without flattening it into generic failure or adding a
third outcome for one caller. Both take a per-project positional target rather
than the selection vocabulary. `generate` writes into the source tree, which a
gate must not do.

### 6. Parity is pinned by recorded fixtures, with no `-update` flag

`@putnami/sdd` must write the bytes the built-in commands wrote. The answers
are recorded under `tooling/cli/internal/cli/testdata/sdd-parity`, captured
from core's implementation before its removal: structured, human, and MCP
invocations plus generated contract artifacts.

- The capture pins git dates, and each answer's `head` field is compared
  verbatim, because it proves which tree the answer describes.
- A one-byte tamper fails the test.
- There is no `-update` flag. The only code that could rewrite these bytes is
  the code under test, so an update flag would let a drift record itself as
  truth. Regenerating means re-capturing from the pre-removal core.

`TestRecordedParityAnswersCoverEveryCaseAndNothingElse` fails in both
directions, so the acceptance cannot shrink while every test passes. Commands
added after the extraction have no recorded oracle and are tested in the
extension.

### 7. The extension projects the spec gate; core sanctions it

`specs-validate` emits one bounded, deterministic executable-criteria
projection per spec-owning project (`SpecCriteriaProjection`, artifact id
`putnami-spec-criteria`), derived only from manifests and specs. The
post-session result finalizer in core joins it with the observation reports in
the session's job results through `protocols/features` only. Core never parses
manifests or specs, the extension never computes exit status, and `enforce`'s
synthetic result happens once, in core's session reducer. There is no separate
verify job. [ADR 0026](0026-spec-gate-evidence-scope.md) owns which
observations the join may use.

## Rejected alternatives

- **Keep SDD in the CLI with a planner special case for `validate`.** The
  scheduler would know one command's name, and the special case would outlive
  its reason.
- **Ship architecture as its own extension now.** Duplicates the workspace
  view, git reader, and rendering layer (decision 2).
- **Let the extension load the workspace or shell out to `putnami`.** Two
  definitions of membership and of what changed (decision 3).
- **Keep undotted MCP tool names as aliases.** An undotted extension tool is
  dropped, and a core-owned name wins a collision, so the alias could never
  work.
- **Cache `features-validate` on manifest and evidence file patterns.** The
  key would not cover the source bindings the verdict reads.
- **Delete the parity tests with the implementation they compared against.**
  See decision 6.

## Consequences

- A workspace must declare `@putnami/sdd` before the four command groups
  exist.
- Validation is a gate: `validate` and `validate-workspace` are in
  `putnami.ci.json` and in the canonical gate,
  `putnami lint,test,build,validate,validate-workspace --impacted --enforce-coverage`.
- Activation needs two conditions: the command's `activationFiles` match *and*
  the project lists the extension in its `extensions` array. A project that
  authors a spec without the declaration is not validated, which looks like
  having nothing to validate.
  `tooling/sdd-extension/workspace_adoption_test.go` asserts the invariant for
  this repository; a downstream workspace owns its own.
- An interactive SDD command costs one subprocess spawn.
