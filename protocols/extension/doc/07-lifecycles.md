# Lifecycle Primitives

Contract v3 gives the orchestrator **exactly three** generic lifecycle
primitives, and nothing else that is language- or product-specific:

1. **extension runtime preparation** — `runtime`
2. **workspace probe and synchronization** — `workspace`
3. **invocation-scoped sensitive outputs with finalizers** — `declares.outputs`
   plus `runOn`

They exist so core can stop *recognizing* extensions and start *asking* them.
Before them, the CLI knew which extension shipped which wrapper binary, which
filename marks a project in which language, and which database a test suite
needs. Every one of those is knowledge that belongs to the extension that owns
the ecosystem, and every one of them made a fourth language a change to core.

There is no fourth primitive. A capability that does not fit one of these three
is a design question, not a new section.

All three are **additive**: a manifest that declares none of them is a v2
manifest and behaves exactly as it did.

---

## 1. Runtime preparation

```json
{
  "runtime": {
    "executable": "bin/putnami-ts",
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["cmd/**", "internal/**", "go.mod", "go.sum", "bin/**"]
    }
  }
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `executable` | **yes** | Runtime binary path, relative to the extension root (published) or to the prepared runtime directory (prepared). The *same* relative path in both cases |
| `prepare` | no | How to build `executable` from the extension's sources. Omit for an extension that ships its binary in each platform archive |
| `prepare.command` | **yes** | A bare program name resolved on `PATH`, or a path rooted at `{extensionRoot}`. Never absolute |
| `prepare.args` | no | Arguments, in order. Paths are extension-root-relative or explicitly rooted at `{extensionRoot}` / `{runtimeOutput}`; absolute, drive-qualified, backslash-separated, and parent-segment paths are rejected |
| `prepare.inputs` | **yes** | Extension-root-relative globs whose content the artifact digest is taken over |

Tasks reference the resolved binary as `{extensionRuntime}` instead of naming a
language-specific wrapper.

### Preparation is workspace-independent

A prepare is a function of the extension's **own module** and nothing else. It
runs with workspace resolution turned off — `GOWORK=off` for Go — and resolves
dependencies from the extension module alone, over the replace closure the
extension maintains.

That is what makes a locally prepared runtime and an archive-shipped one the
**same artifact semantically**: one digest covers both, and a packaged
extension is validated the way a local one is.

The declaration half is machine-checked. A command is either a bare `PATH`
program or `{extensionRoot}/<relative-path>`; `bin/prepare` is rejected because
it would inherit an unnamed ambient working directory. Arguments cannot name
absolute, drive-qualified, backslash-separated, or parent-segment paths. The
template vocabulary is closed:

| Token | Meaning |
|-------|---------|
| `{extensionRoot}` | The extension's own tree — the only source a workspace-independent build may read |
| `{runtimeOutput}` | The directory the CLI assigns for the built executable |

Every other token in the manifest vocabulary (`{workspaceRoot}`,
`{projectRoot}`, `{outputRoot}`, `{cacheRoot}`, the selection variables) names
something *outside* the extension module, so using one is rejected.

Declaration validation cannot prove that an arbitrary executable will not open
another path. The C1 preparer owns that execution isolation: it sets cwd to
`{extensionRoot}`, disables ambient workspace resolution (`GOWORK=off` for Go),
constructs the staged module view from the extension's maintained replace
closure, exposes only that source view and `{runtimeOutput}`, and uses the same
inputs for local and archive validation. This is the boundary between path
syntax the schema can reject and filesystem access the executor must enforce.

`inputs` is required for the same reason it is digested. A prepare whose digest
cannot see its own sources digests to one value forever, and every source edit
would keep serving the previous binary.

### Artifact digest

```
digest = H(prepare declaration ‖ inputs content ‖ platform ‖ extension identity ‖ runtime ABI)
```

Preparation runs **once per digest** during workspace synchronization, under an
inter-process lock, and publishes atomically.

### Lock-resolved toolchains

A mutable runtime can declare the executables used to prepare it and to run
selected tasks. Each alias names a workspace lock entry, an ordered candidate
list, an exact version probe and the environment derived from the resolved
executable. The CLI interprets this closed shape without knowing which language
or tool owns the executable.

```json
{
  "runtime": {
    "toolchains": {
      "compiler": {
        "lock": "compiler",
        "candidates": [
          { "from": "path", "path": "compiler" },
          { "from": "putnami-home", "path": "toolchains/compiler/compiler-{version}/bin/compiler" }
        ],
        "probe": { "args": ["--version"], "expect": "{version}" },
        "environment": {
          "COMPILER": { "from": "executable" }
        },
        "prependPath": true
      }
    },
    "prepare": {
      "command": "compiler",
      "args": ["build", "-o", "{runtimeOutput}/compiled/example", "./cmd/example"],
      "inputs": ["src/**", "module.lock"],
      "toolchains": ["compiler"]
    }
  }
}
```

The probe output must equal the `{version}` template after substitution. A
nearby version never qualifies. Candidate symlinks are resolved before
ancestor-derived environment values are computed, and the selected executable
directory is the only declared addition to `PATH`. The lock version, platform
integrity, declaration and availability form the portable toolchain identity in
runtime and task cache keys.

An `environment` candidate resolves below the value of the variable it names,
read from the CLI's environment with `PUTNAMI_WORKSPACE_ROOT` set to the
workspace root, the value every job receives. A toolchain that an extension
installs inside the workspace is therefore a candidate like one found on
`PATH`, and its probe still has to match the lock.

`prepare.toolchains` applies only while building a local runtime;
`runtime.runToolchains` applies to every runtime task; and a task's own
`toolchains` list adds a narrower requirement. An optional declaration permits
a task that does not need that tool to proceed. When a task does request it and
no exact candidate exists, executable environment bindings are published empty
so the task can choose an alternate operation or fail explicitly; the CLI never
substitutes a mismatching ambient binary.

The workspace lock pins every alias, and `workspace-install` writes the files
the pins derive from. Three cases therefore resolve an alias to the unavailable
identity instead of failing the run:

- A workspace with no lock document resolves every alias this way.
- An alias that only `workspace-install` needs in the run resolves this way when
  the lock does not pin it, has no digest for the host platform, or no candidate
  matches. An alias that another active command needs resolves strictly, and at
  execution a job of any other command refuses a required alias that the run
  resolved unavailable while a lock exists.
- In a run whose every command is `workspace-install`, the prepare toolchains of
  an extension declared by path resolve the same way.

The unavailable identity keys the run, and digests its prepared runtime, apart
from every pinned run, so nothing produced this way is served to a pinned run. A
lock that exists and does not pin an alias that any other command requires
stays a hard failure.

A run resolves the toolchains of its commands before it selects projects and
plans jobs. A required alias that the lock does not satisfy stays unresolved at
that point. Once the plan is final, and before any job runs or reaches the
cache, a planned job that needs the alias fails the run. A run that plans no
such job goes on: a build in a workspace that declares an extension and has no
project of its language does not need that language's pin.

Runtimes for **different** extensions are prepared **concurrently**: each
extension's chain reads only its own source tree and its declared replace
closure, and writes only into a staging directory the store creates privately
per admit. The exclusivity that matters is per **content digest**, and the
inter-process lock above is what provides it — so two extensions never wait for
each other, while two processes preparing the same digest still produce exactly
one build. A manifest cannot opt out of this and does not need to: an extension
whose prepare depends on another extension's output would be reading outside its
own module, which the isolation above already forbids.

### Lifecycle states

| State | Meaning |
|-------|---------|
| `absent` | The manifest declares no runtime; tasks name their commands explicitly |
| `pending` | A runtime is declared and its digest is not yet published |
| `preparing` | One process holds the digest lock and is building |
| `ready` | The executable is published and its `__putnami runtime-info` handshake agrees with the manifest |
| `failed` | Terminal. Reported with one of the codes below — **never** silently replaced by `go run`, a shell wrapper, or name-based classification |

### Failure codes

| Code | Cause | Remedy |
|------|-------|--------|
| `runtime.not_declared` | A task referenced `{extensionRuntime}` but the manifest declares no `runtime` | Declare `runtime.executable`, or name the command explicitly |
| `runtime.executable_missing` | The installed tree or platform archive has no file at `runtime.executable` | Packaging fix: the archive did not ship the declared executable for this platform |
| `runtime.prepare_failed` | The prepare command exited non-zero | The command's own diagnostics are the cause; core adds no interpretation and never retries with a different toolchain |
| `runtime.prepare_output_missing` | Prepare succeeded but wrote no executable at `{runtimeOutput}/<executable>` | The prepare and the declared executable path disagree |
| `runtime.handshake_failed` | `__putnami runtime-info` failed or returned a document that is not a runtime descriptor | Rebuild the extension against a current SDK |
| `runtime.handshake_timeout` | The executable started but had not answered `__putnami runtime-info` when the handshake deadline elapsed. The deadline counts from the runtime's first instruction, so the host's check of a freshly written binary is not charged to it. The same failure reports a check that outlasts its own, larger bound, and a runtime that blocked before the CLI first saw it run | Not a malformed build: the machine was too loaded to schedule the runtime, or the runtime blocks. Rerun on a less loaded machine; if it repeats, run the executable's `__putnami runtime-info` by hand |
| `runtime.identity_mismatch` | The handshake disagreed with the manifest on identity, platform, CLI contract, or runtime protocol | The resolved binary is not the one the manifest describes; it is never executed anyway |

---

## 2. Workspace adapter

```json
{
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json", "bun.lock", "tsconfig*.json"],
    "excludes": ["node_modules", "dist"],
    "syncTask": "workspace-sync"
  }
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `markers` | **yes** | Candidate-directory-relative patterns whose presence makes a directory a project this extension owns |
| `inputs` | **yes** | Patterns whose **content** determines the probe's answer. Core hashes this bounded set on ordinary loads |
| `excludes` | no | Directory patterns the scan must not descend into |
| `syncTask` | no | A task in this manifest that performs the extension's own workspace mutations |

### Paths, not modules

Every member of the adapter is a **path pattern relative to a candidate
directory**. Nothing in the contract can express "the module that owns this
project", and that is deliberate: a probe contract must never assume a project
root is also a module root — that a Go project's directory holds the `go.mod`
that declares it, that a package directory is its own npm workspace entry, that
a Python project owns its `pyproject.toml`.

Those coincide in most repositories today and stop coinciding the moment one
module hosts several projects. Keying on paths keeps that a layout choice
instead of a protocol break.

### The markers are inputs

Every marker must also appear in `inputs`. The probe *reads* the marker to
answer "what project is this?", so a marker nobody hashes would leave the
snapshot valid and the answer stale — editing a `package.json` name would never
re-probe. Declaring a marker outside `inputs` is a validation error.

### How the probe is invoked

Core spawns the extension's runtime with the reserved control call
`<runtime> __putnami workspace-probe`, working directory set to the workspace
root, writes one `WorkspaceProbeRequest` to stdin, and reads one
`WorkspaceProbeResult` from stdout. Stdout carries the result and nothing else —
a provider that logs there produces a document with trailing data, which is
rejected rather than truncated. The wire and the `ServeProbe` helper live in
`protocol/workspace`.

### Snapshot validity

- Results are stored normalized and atomically in `.putnami/workspace-index.json`,
  per provider, so a stale snapshot re-probes only the providers whose own
  declared inputs moved and carries every other provider's answer forward.
- The **content digest** of the declared inputs is the validity oracle — never a
  size and never an mtime, so a same-size/same-second rewrite still invalidates.
  A glob input's digest covers the whole matched set, so creating or deleting a
  matching file invalidates too.
- File stats may prioritize work *within* a running process; they may never
  establish cross-process correctness.
- Under `--plan` and `--dry-run` core may probe in memory but never writes.
- A changed input that no adapter claims — `putnami.json`, the workspace config,
  a scope config — invalidates **every** provider: those files decide membership
  and explicit identity.

### Core-owned exclusions

Core always excludes `.git`, `.putnami`, and gitignored directories, for every
extension. An adapter that restates one of those gets a `redundant-exclude`
warning: it is not wrong, it simply has no effect.

### Merge rules

Probe results and explicit configuration are merged by core, in this order:

| Contribution | Rule |
|--------------|------|
| Canonical project path and ID | **Core alone.** No probe may assign them |
| Explicit `putnami.json` values | Override every probe-derived value |
| Source identity (project name) | Probe identity outranks scope `namePattern`, which outranks the directory basename |
| Dependencies | Explicit and probe-derived are **unioned** and normalized to project IDs |
| Conflicting non-empty scalars | **Hard error** unless explicit project config resolves them (`workspace.probe_conflict`) |
| Source **file** (provenance) | Follows the winning source identity; never a conflict. Two providers reporting two manifests for one directory (a Go service that also carries a `package.json`) are both telling the truth, and nothing authors a `sourceFile`, so a hard error would have no escape hatch |
| Provider-specific data | Lives under `project.metadata[extensionName]`, namespaced by the contributing extension |
| Scope tags and extensions | Fill unset values; never overwrite explicit project configuration |

### Lifecycle states

| State | Meaning |
|-------|---------|
| `unknown` | No snapshot exists; the next graph-dependent command probes |
| `probing` | One batched request per affected extension is in flight |
| `indexed` | The snapshot is present and its input digests match the tree |
| `stale` | A declared input changed; only the affected extensions are re-probed |
| `failed` | Graph-dependent commands fail; the recovery commands (`install`, `extensions`, `projects sync`, `help`, `version`) stay available |

### Failure codes

| Code | Cause | Remedy |
|------|-------|--------|
| `workspace.probe_failed` | The probe process exited non-zero or produced no result document | The probe's own diagnostics are the cause |
| `workspace.probe_invalid_result` | The result does not conform to the probe-result contract | An extension bug; core never guesses at a partially readable result |
| `workspace.probe_conflict` | Two non-empty scalar contributions for one project disagree | State the value explicitly in the project's `putnami.json` |
| `workspace.snapshot_invalid` | The stored index is unreadable or written in an incompatible format | `putnami projects sync` rebuilds it |

---

## 3. Invocation-scoped sensitive outputs and finalizers

```json
{
  "tasks": {
    "test-database-setup": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": [
        "database",
        "up",
        "--dsn-file",
        "{invocationArtifactRoot}/database/dsn.env",
        "--lease-file",
        "{invocationArtifactRoot}/database/lease.json"
      ],
      "cache": false,
      "declares": {
        "outputs": {
          "dsn": {
            "kind": "runtime-file",
            "scope": "invocation",
            "sensitive": true,
            "path": "database/dsn.env"
          }
        },
        "effects": ["process"]
      }
    }
  },
  "commands": {
    "test": {
      "run": [
        { "id": "setup", "task": "test-database-setup" },
        { "id": "run", "task": "test-run", "dependsOn": ["setup"] },
        {
          "id": "teardown",
          "task": "test-database-teardown",
          "runOn": "finally",
          "finalizes": {
            "producer": "setup",
            "consumers": ["run"]
          }
        }
      ]
    }
  }
}
```

Three additions to a declared output and one explicit pipeline relation:

| Member | Meaning |
|--------|---------|
| `kind: "runtime-file"` | A file whose content only means anything inside the invocation that wrote it — a connection string for a container that dies with the run, a socket path. Never captured, never restored |
| `scope: "invocation"` | The output is confined to the current CLI invocation: written to its private scratch (mode `0600` when sensitive), never captured, discarded when the invocation ends |
| `sensitive: true` | The output's path and bytes must never reach an event, a result, a session record, telemetry, or cache traffic |
| `runOn: "finally"` + `finalizes` | The containing step is the finalizer. `producer` start arms it exactly once; cleanup waits for every named `consumer` to reach a terminal state |

`scope` defaults to `durable`: every output declared before this vocabulary
existed outlives the invocation and is captured, exactly as before.

### What the declaration must agree with

| Rule | Why |
|------|-----|
| `runtime-file` requires `scope: "invocation"` | A durable pointer to a resource that no longer exists is worse than no pointer |
| `sensitive` requires `scope: "invocation"` | A durable output is captured *by definition*, and capture is the one path a secret may never take |
| `sensitive` requires a literal `path` | A `pathFrom` value is reported through the task result, which would publish the path the flag exists to withhold |
| An invocation-scoped output leaves `root` unset | The invocation scratch is not a staging root; naming one says two contradictory things about where the file lands |
| A task with an invocation-scoped output sets `cache: false` | A cache hit would report success without ever creating the artifact |
| `finalizes.producer` uses a task with an invocation-scoped output | The relation is part of this artifact primitive, not a generic teardown mechanism |
| Every `finalizes.consumer` is downstream of the producer | The complete completion frontier is explicit and mechanically derivable |
| A finalizer has no ordinary `dependsOn` | Trigger and frontier come only from `finalizes`, so they cannot conflict |
| No step may bind an input from a `runOn: "finally"` step | A finalizer runs regardless of outcome and produces no result a dependent can consume |
| Neither a finalizer task nor a command may export its result | Cleanup has no data result; success or failure is reported only as lifecycle status |

Paths follow the same strict rule as every other declared path: relative,
cleaned, slash-separated, no escape above the root.

### Artifact-root delivery

Invocation paths are explicit rather than conventional. For one `finalizes`
relation, the orchestrator creates a non-secret invocation handle and private
artifact root, then includes job-context
`invocation: {id, artifactRoot}` **only** for the producer, listed consumers,
and finalizer. Those tasks also receive the same root through the
`{invocationArtifactRoot}` template token in their manifest-authored arguments,
as the example shows. Unrelated tasks receive neither.

Every `scope: "invocation"` output path resolves beneath that artifact root.
The root and ID are locators, not credentials, but they still never enter task
results, runtime events, cache documents, telemetry, or public session records.
Sensitive bytes and paths remain confined to the private tree.

### Execution and cache identity

- Cache negotiation happens **before** materialization. Because `finalizes`
  names the producer and complete consumer frontier, an all-hit frontier prunes
  the producer and its finalizer together; any consumer miss keeps the producer
  and arms exactly one finalizer.
- A relation whose producer sometimes performs an imperative side effect beyond
  serving its frontier can declare `finalizes.pruneIf`, a plan-time expression
  over command params. Core prunes an all-hit frontier only when the expression
  is true; false or malformed expressions fail safe by running the producer and
  its finalizer. For example, `"pruneIf": "!params.teardown"` preserves warm-run
  pruning while ensuring an explicit teardown request is never swallowed.
- Downstream cache identity uses the producing action's digest, **never** the
  secret's content.
- The finalizer runs **exactly once** whenever its producer started — including
  consumer failure and cancellation — and receives an independent bounded
  cleanup context after cancellation.
- Setup failures block consumers with typed causal diagnostics.
- Finalizers and provider cleanup are idempotent.

### Crash recovery

A `SIGKILL` cannot execute a finalizer, so recovery is contractual rather than
best-effort:

- Core holds a **non-secret** invocation lease: invocation ID, PID, provider,
  action digest, creation time.
- External resources carry the corresponding non-secret lease identity.
- Provider setup and maintenance reap resources whose lease has no live owner
  **before** provisioning or reuse, so the next invocation after a kill recovers
  the orphan without waiting for a GC interval.
- Credentials never appear in leases, labels, names, filters, logs, or errors.

### Lifecycle states

| State | Meaning |
|-------|---------|
| `declared` | The task declares an invocation-scoped output; nothing has run |
| `provisioned` | Setup succeeded and the artifact exists in the invocation scratch |
| `consumed` | At least one consumer read it |
| `finalized` | The `runOn: finally` step ran; the resource is gone |
| `orphaned` | The owning process died without finalizing; a later invocation reaps it by lease |

### Failure codes

| Code | Cause | Remedy |
|------|-------|--------|
| `sensitive.setup_failed` | The provisioning task failed | Consumers are blocked with this as their causal diagnostic rather than run against a resource that does not exist |
| `sensitive.artifact_missing` | Setup succeeded but a declared invocation-scoped output is absent | The task and its declaration disagree |
| `sensitive.finalizer_failed` | A `runOn: finally` step failed | The invocation's outcome is unchanged, but the leak is reported and never swallowed |
| `sensitive.leak_detected` | A sensitive path or its bytes reached a surface that must never carry them | Fail-closed: the invocation fails rather than publishing the value |

### Recovery signals

| Code | Meaning |
|------|---------|
| `sensitive.lease_reaped` | A later invocation found an orphaned lease and successfully removed the resource before provisioning or reuse. This is successful SIGKILL recovery, not a failure |

---

## Validation codes

The codes above are **runtime** failures: a correct manifest could not be
executed. Manifest defects are reported by `ValidateManifest` with the kebab-case
validation codes, which never use the dotted spelling:

| Code | Rule |
|------|------|
| `invalid-runtime-path` | A runtime executable, prepare command/argument, or input uses an absolute, ambient-cwd, drive-qualified, backslash, escaping, or non-concrete path |
| `invalid-template-var` | A prepare used a token outside `{extensionRoot}` / `{runtimeOutput}` |
| `invalid-workspace-path` | A marker, input, or exclude is absolute, escapes its root, or uses a template variable |
| `marker-not-input` | A declared marker is missing from `workspace.inputs` |
| `unresolved-sync-task` | `workspace.syncTask` names a task the manifest does not define |
| `redundant-exclude` | *(warning)* An exclude restates a core-owned exclusion |
| `duplicate-value` | A pattern set declares the same pattern twice |
| `invocation-scope-required` | A `runtime-file` or `sensitive` output is not invocation-scoped |
| `invocation-scope-conflict` | An invocation-scoped output also names a staging root |
| `invocation-cache-conflict` | A cacheable task declares an invocation-scoped output |
| `sensitive-path-leak` | A sensitive output reports its path through a `pathFrom` port |
| `finalizer-binding` | A step binds an input from a `runOn: finally` step |
| `finalizer-export` | A task or command exports a result from a finalizer |
| `finalizer-producer-without-invocation-output` | `finalizes.producer` does not use a task that declares an invocation-scoped output |
| `invalid-finalizer-relation` / `invalid-finalizer-frontier` | The producer/consumer relation is self-referential, names a finalizer, or lists a consumer that is not downstream of the producer |
| `shared-finalizer-consumer` | A step appears in the consumer frontier of two `finalizes` relations; a step belongs to at most one invocation |

Conformance fixtures for every rule live in `fixtures/valid/lifecycle-v3.json`
and the matching `fixtures/invalid/*.json` counter-examples.
