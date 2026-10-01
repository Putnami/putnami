# ADR 0006 — Two boundaries: a model module, and command verticals

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`), `@putnami/cli-model` (`tooling/cli-model`, module `go.putnami.dev/cli/model`)

## Context

Renderers and serializers need the CLI's data types (`JobResult`,
`ScheduledJob`, `Project`, manifest types) but none of its behavior. When those
types live beside the scheduler, the probe or the registry client, every
renderer depends on the executor, and nothing at the call site shows it:
`jobs.TaskResult` reads the same whichever package supplies it.

A flat command package has the same problem one level up. Unrelated command
surfaces share a package, so nobody asks whether a new helper belongs to one
command, to several, or to none, and the package grows one file at a time.

Both are dependency-graph problems. A convention cannot fix them; the module
system and import ratchets can. This ADR makes the result and engine
boundaries of [ADR 0001](0001-cli-foundation-boundaries.md) structural.

## Decision

### 1. `tooling/cli-model` holds the data model

`go.putnami.dev/cli/model` is an unpublished Go module beside `tooling/cli`
with three packages:

| Package | Holds |
| --- | --- |
| `workspace` | `Workspace`, `Project`, the dependency graph, target and filter resolution, identity, the probe view, change-to-project impact |
| `extension` | `ExtensionDescription`, `JobDefinition`, manifest and contract types, pipeline expansion, the expression evaluator, flag declarations |
| `jobs` | `ScheduledJob`, `JobResult`, `Execution`, the plan contract, the canonical result reducer, the runtime-event parser, identity and invocation types, the job-context shape |

The module holds type declarations and pure methods only. Filesystem, process,
network and terminal behavior stays in `go.putnami.dev/tooling/cli`.

**`tooling/cli-model/go.mod` requires only `go.putnami.dev/protocol/*`.** A
model package that reaches back into the CLI does not compile. That is why the
model is a module and not another `internal/` package.

Package names match the CLI packages they serve, and `internal/workspace`,
`internal/extension` and `internal/jobs` each keep a `model_alias.go` of
`type X = model.X` aliases, so a consumer moves one import line at a time.

Known limits: the module's tests are not hermetic
(`TestFirstPartyPackageFlagsAreCompatible` reads repository manifests), and
its one filesystem call is `workspace.ResolveLinks`
(`filepath.EvalSymlinks`, plus a standard-library copy of
`dirlink.Resolve` for Windows junctions, which the module cannot import).
`TestModelResolveLinksAgreesWithDirlink` (`internal/workspace`) pins the two to
the same answer.

### 2. One package per command surface, plus `shared` and `sharedtest`

`internal/commands` is a container directory (documented in its `doc.go`)
with one package per command surface: `agentctx`, `cachecmd`, `ci`,
`completion`, `composecmd`, `configcmd`, `doctor`, `extensions`, `lifecycle`,
`migrate`, `qualifycmd`, `sessions`, `treecmd`, `versioncmd`. Only the
surface golden (`surface_golden_test.go`, `testdata/surface/`) stays at the
root.

A helper two or more verticals need moves to `internal/commands/shared` when
its callers are production code, and to `internal/commands/sharedtest` when
they are tests. Rationale: a test helper in `shared` makes it import
`testing`, which links the testing runtime into the shipped binary.

`lifecycle` is the only vertical that imports siblings (`agentctx`,
`extensions`, `versioncmd`, `completion`): install, upgrade and
workspace-init sequence the other verticals' phases. The graph is acyclic with
`lifecycle` on top. Hoisting those implementations into `shared` to empty the
exception list is rejected; it would rebuild the flat package under another
name.

### 3. The method for a mechanical move

A future move cites these rules instead of re-deriving them.

| Rule | Statement | Why |
| --- | --- | --- |
| R1 | **Alias, then flip.** A moved type leaves `type X = model.X` in the same commit; a separate commit changes consumers. | Each commit reviews as a rename or as import lines. |
| R2 | **Shared rule.** A symbol 2+ verticals need moves to `shared`; a symbol one vertical needs moves with it. The compiler decides. | Guessing grows `shared` into the flat package. |
| R3 | **Split files by dependency.** In a file mixing model and I/O, type declarations and pure methods move; I/O stays. | The split follows the dependency, not the filename. |
| R4 | **Pins move with the break.** Baselines, goldens and ratchets change in the commit that breaks them. | A pin fixed later was briefly not a pin. |
| R5 | **Selector-preserving names.** Model packages keep the origin package's name. | Every flip is an import-line diff. |
| R6 | **No renames**, stutter included, unless a casing collision forces one. | A pure move stays greppable. |
| R7 | **Exported-field rule.** An unexported member of a moved type that the remaining half uses becomes exported in the moving commit. | Behavior-neutral; the goldens prove it. |

Classify by receiver type first, files second. Go requires a method in its
receiver's package, so moving a type moves every method on it, whatever file
it sits in. Compute the closure of the type, its methods and the types they
mention before writing a file table.

### 4. Enforcement

1. **The module boundary**: `cli-model/go.mod` requires only
   `go.putnami.dev/protocol/*`.
2. **`TestStructuralBaseline_ModelConsumersStayDecoupled`**
   (`internal/cli/model_decoupling_ratchet_test.go`): the projection packages
   `internal/{changeplan,machine,output,watch,workspace_state}` do not import
   `internal/{jobs,workspace,extension}` in production code. It checks the
   import line, because R5 aliases keep a regression compiling and every
   behavioral test green. Each exception names its file and the one
   execution-side symbol that blocks it (`jobs.DefaultTimeoutMs`,
   `jobs.TuningReport`, `jobs.JobContextVersion`). Moving those symbols is a
   judgment about ownership, not a mechanical move.
3. **`TestStructuralBaseline_VerticalsStayIsolated`**
   (`internal/cli/vertical_isolation_ratchet_test.go`): a vertical imports
   `shared`, `sharedtest`, `go.putnami.dev/cli/model/*` and CLI support
   packages, never a sibling, except through a named per-file exception. It
   scans test files too: a test importing a sibling is the same layering fact.

Both ratchets fail on a stale exception. Every structural scan reads
`cliModules` in `structural_baseline_test.go`, so a scan follows code across
the module boundary instead of reading zero and inviting a pin to be lowered.

The model keeps an 80% coverage threshold and must pass with
`--enforce-coverage`.

## Rejected alternatives

- A second `internal/` model package: it permits imports back into execution
  code.
- Duplicating types at the boundary: aliases keep one identity and make
  consumer changes import-only.
- Moving every cross-vertical helper into `shared`: it recreates the flat
  package.
- Moving the three decoupling-exception symbols only to reach zero: each is
  accurately owned by the execution half and ratcheted.
