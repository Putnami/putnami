# Core workspace experience

This is the shortest complete path from an unfamiliar Putnami workspace to a
verified change. It uses public CLI and MCP surfaces only.

## Discover, understand, plan, run, diagnose

1. Discover the root and project graph with `putnami workspace describe`,
   `putnami projects list`, and `putnami projects describe <project>`.
2. Understand product intent with `putnami context map`. If the workspace
   declares the `@putnami/sdd` extension, add `putnami features inspect
   <feature>`, `putnami specs list`, and `putnami specs inspect <feature>`;
   those four command groups are not part of the CLI
   ([ADR 0013](adr/0013-sdd-as-a-standalone-extension.md)). Feature maturity
   describes evidence; package support describes the compatibility promise.
   They are independent.
3. Select projects by exact project, scope, target expression, current
   directory, or `--impacted`. Add `--plan` to any job invocation to inspect the
   selected dependency DAG without executing it.
4. While iterating, select the projects you changed with
   `--projects <a>,<b>`: it keeps the upstream steps they need and skips every
   project that depends on them. Before declaring work complete, verify once
   with `putnami lint,test,build --impacted --enforce-coverage`. A
   `putnami.ci.json` replaces that gate with its blocking commands. Without a
   usable one, the gate stays `lint,test,build`, whatever the extensions of the
   workspace declare. In a repository that provides
   `./putnamiw`, use that pinned wrapper for build, test, lint, dependency, and
   generation workflows.
5. Diagnose with `putnami doctor`, structured `--output=jsonl`,
   `putnami sessions inspect`, and `putnami report`. Machine consumers should
   parse protocol output, not terminal prose.

Python integration is experimental, explicit opt-in, non-default, and does not
claim Go or TypeScript parity. `@putnami/sdd` — which provides `features`,
`specs`, `architecture`, `contracts`, and the `validate` /
`validate-workspace` jobs — is experimental on the same terms: its manifest
format, finding IDs, and detector coverage may change without a migration path.
The current license remains FSL-1.1-MIT.

## Public command ownership

The catalog is the source of truth for syntax and flags; run
`putnami help --markdown` for generated reference. This map groups every public
path by its user outcome and durable specification. A command can delegate to a
specialized subsystem while still belonging to the CLI outcome that exposes it.

| Outcome | Public commands | Feature and spec | Protection |
| --- | --- | --- | --- |
| Workspace and project discovery/selection | `workspace`, `workspace init`, `workspace describe`; `init`; `projects`, `projects list`, `projects create`, `projects describe`, `projects sync`, `projects tag`; `scopes`, `scopes list`; `config`, `config show`, `config set` | `cli/workspace-discovery-selection` / `specs/workspace-discovery-selection.json`; project creation also follows `tooling/project-scaffolding` | workspace-root, identity, target/filter/include, graph, impact, project command, and scaffold integration suites |
| Generated context and the workspace map | `context`, `context generate`, `context map` | `cli/context-mcp-discovery` / `specs/context-mcp-discovery.json` | generated-context, workspace-map, MCP catalog/equivalence, and public-content suites |
| Job selection, planning, and execution | workspace job verbs `build`, `test`, `lint`, `serve`, `run`, `format`, `package`, `publish`, `deploy`, `generate`; `install`, `upgrade`, `pin`; `version`, `version get`, `version tag`, `version list`, `version use`; `infra`, `infra plan`; `extensions`, `extensions install`, `extensions list`, `extensions update`, `extensions remove`; `templates`, `templates install`, `templates list`, `templates update`, `templates remove`; `deps add`, `deps remove`, `deps install`; `cache`, `cache clean`, `cache gc`, `cache verify`; `migrate`, `migrate vnext`, `migrate agent-content`; `dev`, `dev extension`, `dev template` | `cli/job-planning-execution` / `specs/job-planning-execution.json`; `deploy --env` follows `cli/deploy-environments` / `specs/deploy-environments.json`; `publish` channels, protected channels and visibility follow `cli/channels` / `specs/channels.json`; template/project materialization also follows `tooling/project-scaffolding`; agent artifact phases follow `cli/agent-workflow-lifecycle`; cold graph readiness, concurrent restoration and compact install progress follow `cli/workspace-initialization` / `specs/workspace-initialization.json` | catalog/help goldens, selection/plan, engine adapters, scheduler, cache, lifecycle, extension/template, migration, version, and machine-result suites |
| CI admission | `change-plan`; `impact-plan` | `cli/impact-plan` / `specs/impact-plan.json`; the gate the generated guidance derives from `putnami.ci.json` follows `cli/ci-document` / `specs/ci-document.json`; selection also follows `cli/job-planning-execution` and `cli/workspace-discovery-selection` | immutable change-plan and impact-plan, guidance-gate derivation, and impacted-selection suites |
| Workload execution | `compose` | `cli/workload-qualification` / `specs/workload-qualification.json` | runsWith closure planning, stable proxies, per-composition databases, typed readiness, injected-configuration redaction, and orphan reaping suites |
| Diagnostics and results | `doctor`; `sessions`, `sessions list`, `sessions inspect`, `sessions export`, `sessions summary`, `sessions replay`; `tree`, `tree fingerprint`, `tree verify`; `report`; `telemetry`, `telemetry on`, `telemetry off`, `telemetry status`, `telemetry show` | `cli/diagnostics-results` / `specs/diagnostics-results.json`; `cli/native-session-reporting` / `specs/native-session-reporting.json` | doctor fail-closed gate, redaction, result reduction/rendering, session persistence/export/summary/retention, bounded native reporter replay, telemetry consent, and exit-code suites |
| Workload qualification | `qualify` | `cli/workload-qualification` / `specs/workload-qualification.json` | route-inventory derivation, URL-target phase and sha-binding, verdict exit-code and failure-envelope, and shared qualify corpus suites |
| MCP and agent access | `mcp`, `mcp install`, `mcp pilot` | `cli/context-mcp-discovery` / `specs/context-mcp-discovery.json` | MCP protocol/catalog, tool annotations, CLI equivalence, stdout isolation, and external-side-effect refusal suites |
| Canonical guidance | `completion`, `completion bash`, `completion zsh`, `completion fish`; `help` | `cli/canonical-workspace-guidance` / `specs/canonical-workspace-guidance.json` | top-level/command/Markdown/man/structured help goldens, completion parity, generated-context, public-content, and documentation-link gates |

The internal `@putnami/cli-model` module owns the pure workspace, extension,
planning, and result data used by these outcomes. It is unpublished, absent from
the support catalog, depends only on `go.putnami.dev/protocol/*`, and is not a
public API. Filesystem, process, network, and terminal behavior remains in
`@putnami/cli`.

## MCP routing and access

The core MCP server exposes eleven tools and one `workspace://context`
resource. Tool annotations are contractual: read-only methods do not execute
jobs, and `run_jobs` is the only mutating method.

| Outcome | MCP methods | Feature and spec | Access and protection |
| --- | --- | --- | --- |
| Workspace orientation | `workspace_map`, `list_projects`, `describe_project`, `agent_context` | `cli/context-mcp-discovery` | read-only; map/project/context equivalence and bounded-result tests |
| Ownership and impact | `deps`, `find_owner`, `why_impacted`, `topo_sort`, `impacted` | `cli/workspace-discovery-selection` | read-only; graph, owner, impact-closure, and baseline tests. `impacted` returns a `reasons` entry per project and `why_impacted` an `edges` list, both over the same dependency, contract and extension-consumer edges, and both naming the same task scopes |
| Plan and run | `run_jobs` | `cli/job-planning-execution` | mutating; `dryRun` previews without execution; blocking serve and externally mutating jobs are refused |
| Recall diagnostics | `get_diagnostics` | `cli/diagnostics-results` | read-only; returns the latest executed run in the same server session and never reruns work |

Start with `workspace_map` for broad orientation, then use the narrow project or
impact method. Use grep for literal symbols and strings. If a tool is
unavailable or looks stale, fall back once to the equivalent read-only CLI
command.

Installed extensions add dot-namespaced tools after the core ones. A workspace
that declares `@putnami/sdd` also gets `sdd.list_features`,
`sdd.feature_context`, `sdd.list_specs`, and `sdd.spec_context` for functional
design; they were core tools under undotted names before that, and the undotted
names are refused with `-32602`
([ADR 0013](adr/0013-sdd-as-a-standalone-extension.md)).

## Maturity and support

`@putnami/cli` is stable under the repository's pre-1.0 migration policy:
breaking changes may ship in a minor release but require a changelog entry and
migration path. Support status and feature maturity are separate. The five core
workspace features in this chapter are
`modeled`; they must not become `coded` until an existing build/test producer
emits exact Feature Evidence. Evidence is never hand-authored.
