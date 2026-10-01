# Well-Known Verbs and Command Traits

Command names in `putnami.extension.json` are free-form, but Putnami
ships a canonical verb vocabulary. A command using a well-known name
inherits its documented semantics and default **traits** — the
machine-readable orchestration hints the CLI consults instead of
matching command names.

## Why traits exist

Before traits, verb semantics lived as hardcoded name checks inside the
orchestrator: `test` was special-cased as a heavy job, `serve`/`run`
were skipped for libraries, and `publish`/`package` had publish channels
injected. A new extension (or a fourth language) had to hope its command
names hit the right special cases. Traits make those semantics
declarative.

Traits describe the **command**, never the workload it runs against.
`publish`/`deploy` once carried a `preflight: config-schema` trait that
made core walk a project's imports and infra markers to decide whether
an artifact core does not own had to exist first; contract 3 deleted it.
A task that needs another task's output declares a typed input port
instead — see [Typed producer requirements](#typed-producer-requirements).

## The verb registry

| Verb | Semantics | Default traits |
| --- | --- | --- |
| `build` | Compile/transpile sources into runnable or distributable outputs. | — |
| `test` | Run tests; emit test counts and coverage metrics. Declare `heavy` on the runner step. | — |
| `lint` | Check code; emit per-issue diagnostics. | — |
| `format` | Rewrite sources into canonical formatting. | — |
| `serve` | Run the dev server until interrupted. Long-running workloads only. | `requiresRunnable` |
| `run` | Execute the project once and exit. | `requiresRunnable` |
| `package` | Assemble distributable artifacts per publish channel. | `injectPublishChannels` |
| `publish` | Push packaged artifacts to registries. | `injectPublishChannels`, `sideEffects: registry` |
| `deploy` | Converge a cloud environment on the workload's aggregated infra manifest. | `sideEffects: cloud` |
| `workspace-fetch` | Download ecosystem dependencies for the workspace without running any of their code. Runs before every `workspace-install` on a hosted run. | — |
| `workspace-install` | Install ecosystem dependencies for the workspace. | — |
| `deps-upgrade` | Upgrade framework and tooling dependencies. | — |
| `config-extract` | Extract the project's config schema manifest. | — |
| `config-merge` | Merge config defaults from workspace dependencies. | — |

Custom verbs remain allowed; they carry zero traits unless the manifest
declares them.

## Declaring traits

```json
{
  "commands": {
    "cloud-release": {
      "description": "Release to the cloud registry",
      "traits": { "sideEffects": "registry" },
      "run": [{ "id": "release", "task": "release" }]
    }
  }
}
```

A declared `traits` object **replaces the verb's defaults wholesale**;
fields are not merged. Per-step heaviness uses `heavy` on the pipeline
step:

```json
{ "id": "golangci-lint", "task": "golangci-lint", "heavy": true }
```

| Trait | Effect in the orchestrator |
| --- | --- |
| `heavy` | Worker tuning leaves the command's jobs CPU/memory headroom. |
| `requiresRunnable` | The command is skipped for `library` projects. |
| `injectPublishChannels` | The project's `publish: [...]` channels are auto-activated as boolean params. |
| `sideEffects` | `registry` or `cloud`; documents what the command mutates outside the workspace. |

### Gated capability transport is not server authorization

Runner-scoped transport is explicitly enabled by a non-empty
`PUTNAMI_CLOUD_CAPABILITY_AFTER`. Only in that mode, the CLI captures and
removes both it and `PUTNAMI_CLOUD_TOKEN` from the process environment before
extension setup, discovery, hooks, runtime preparation, workspace probes,
cache providers, and jobs. The token stays in memory. It is never a task
parameter, plan or job context member, cache-key input, session field, or
output value. Runtime events and terminal results are redacted in memory before
they reach live renderers, session projection, or local/remote cache conversion;
the bearer is never registered in a persisted secrets list. A bearer found in
a JSON object key is removed rather than renamed (renaming could overwrite a
different member), and the protected result fails with
`sensitive.leak_detected`. A token without AFTER retains the historical ambient behavior
used by local Mac `publish` and `upgrade`. Once a non-empty AFTER contract is
captured, any manifest-owned copy of `PUTNAMI_CLOUD_TOKEN` is removed too; an
AFTER-only, half-configured runner therefore fails closed.

The framework-owned release-set coordinator is the sole pre-plan transport
exception. For both hosted CI and the historical token-only Mac path, it invokes
the current Putnami executable with fixed
`cloud release-set {resolve,put,advance}` protocol argv, not repository-selected
argv. Its explicit child environment is rebuilt from the scrubbed parent
environment with the exact captured bearer plus an internal marker bound to the
already resolved provider identity; AFTER stays removed. The nested CLI captures
and removes both values immediately. For `advance`, it also captures the private
`PUTNAMI_RELEASE_SET_PUBLICATIONS_FILE` path created by the framework and removes
that path from the ambient environment. This happens before pin handling, artifact installation,
bootstrap, hooks, discovery children, or runtime preparation. It parses the raw
fixed argv without workspace aliases, skips artifact/bootstrap mutation, and
grants the bearer only to the freshly discovered interactive job whose extension
name, version, and flat command exactly match the resolved reserved provider.
The publication path is reintroduced only for that exact granted provider job;
ordinary jobs receive neither it nor the bearer. The marker and AFTER never
reach that provider job. A missing or changed owner,
non-interactive binding, different command, non-absolute request path, or any
extra argv fails closed.
`resolve` may run before planning to pin the immutable channel head; `put` and
`advance` run only in the finalizer after global success and are not called for
dry-run. This exception does not grant a manifest job or hook the bearer and
does not reintroduce it into the parent process environment. Token-only local
publish/upgrade jobs outside this reserved child keep their ambient compatibility.

The trusted runner must separately set `PUTNAMI_CLOUD_CAPABILITY_AFTER` to a
comma-separated command list. The production release runner uses
`lint,test,build,validate,validate-workspace`. This is runner-image control
data: the CLI captures it at the same boundary, never delivers it to a job, and
ignores a manifest job's attempt to define it.

After the final plan exists, the engine creates an in-memory authorization that
manifests cannot represent. Every `registry` or `cloud` job must have **all
planned leaf jobs of every listed command** as transitive `dependsOn`
predecessors — except a **release-set member publication**, which flows from
its own `package` chain; for it, the AFTER leaves gate the release-set stamp
(the same-session barrier, or the finalizer) instead (CLI ADR 0023). `runOn:
finally` steps are coordinator work, so they are excluded from the leaf set and
can neither prove nor block a gate. `serializeAfter` ordering does not count.
The scheduler grants the token only after those leaves **and every other
transitive functional predecessor of the protected job** have terminal
`success` results; a member publication needs only its own predecessors.
Missing commands or leaves, cycles, and any failed/skipped/canceled functional
predecessor all fail closed; `--continue-on-error` never carries a grant past
a failed predecessor. Direct job runners and adapters
receive no grant. Modes which execute no child (`--plan` and ordinary dry-run
previews) construct no authorization and therefore cannot grant a token.

This is still capability transport, not a sandbox or the final authorization
boundary. Repositories control manifests, including traits, commands, and DAG
edges. CI must supply only a run-, workspace-, and resource-qualified
capability with the minimum server-enforced scopes. Under the current tenancy
invariant, one repository is one workspace and one CI client: the issuer binds
that client to the exact repository and workspace server-side; neither an
environment variable nor repository manifest chooses the workspace. The
capability also binds the run and source SHA. Its expiry must cover the
authoritative CI deadline plus operational margin; the CLI deliberately treats
the capability value as opaque and does not assume OAuth, `dcr1`, or another
credential format, nor guess a shorter TTL. The issuer must support
terminal/cancel revocation through token introspection or `jti`. Never put a
broad human token in CI. Server authorization remains authoritative even if a
repository forges plausible gate jobs or edges.

## Typed producer requirements

A command whose task needs an artifact another task produces states it
on the **task**, not as a trait:

```json
{
  "tasks": {
    "cloud-publish-config": {
      "inputs": {
        "schema": { "from": "task" }
      }
    }
  }
}
```

A `from: "task"` port that is not `optional` is a hard requirement. The
consuming pipeline step must bind it to a producing step with `with`,
and that producer must survive into the plan; otherwise planning fails
with both identities — the consumer job key, its extension and task, the
port name, and the producer step that is missing. `"optional": true`
means the consuming task handles absence itself (a workload with no
config blocks, a project that produces no schema).

This is what replaced the `preflight: config-schema` trait: core never
inspects a workload's sources to guess whether an artifact is required,
because the manifest says so.

## Standard flag vocabulary

These flags converged across the language extensions by imitation; they
are now the canonical names and types. An extension implementing the
matching behavior must use these names so workflows transfer across
languages:

| Flag | Type | Verbs | Meaning |
| --- | --- | --- | --- |
| `--fix` | boolean | `lint`, `format` | Apply fixes instead of only reporting findings. |
| `--coverage` | boolean | `test` | Collect coverage and emit the coverage payload/artifacts. |
| `--coverage-threshold` | number | `test` | Fail the job when coverage falls below this percentage; implies `--coverage`. |
| `--timeout` | number (ms) | `test` | Per-test timeout. |
| `--update-snapshots` | boolean | `test` | Refresh recorded snapshot files. |

Per-verb outputs are equally standardized — see the runtime protocol's
typed payloads (`protocols/runtime/schemas/payloads.json`):
test/coverage/lint summaries in result data, per-issue lint
diagnostics, binaries as `binary` artifacts.

## The `deploy` contract

No extension implements `deploy` yet; this is its specification so the
first implementation conforms instead of defining the convention by
accident:

- **Inputs.** The workload's aggregated infra manifest
  (`.gen/requirements.json`, see `protocol/infra`) and the version
  metadata from the job context (`protocol/job`). `deploy` consumes the
  aggregated manifest — it must not re-derive requirements from source.
- **Ordering.** Depends on `!publish` so deploy waits for every publish job in
  the current session. `publish` itself is the release gate and depends on
  `!lint`, `!test`, `!build`, and `!package`.
- **Schema dependency.** A deploy task that needs the workload's
  extracted config schema declares it as a typed `from: "task"` input
  port and binds it to the producing step. Core does not inspect the
  workload to decide whether the artifact is required.
- **Side effects.** `cloud`. Never cached; re-running converges, it
  does not duplicate.
- **Readiness.** A deploy is complete when the workload's operational
  endpoints (see `protocol/platform`) report ready: poll `/readyz`
  until 200 or a timeout, and report failure with the envelope's
  failing checks.
- **Output.** Emit one `artifact` event per deployed unit (kind
  `deployment`) and a `result` whose data carries the environment,
  workload, and version deployed.
