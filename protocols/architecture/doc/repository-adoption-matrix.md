# Repository adoption matrix

This repository maps **94 of its 134 projects** to **10 architecture domains**.
The other **40 are excluded on purpose**: 28 samples, 7 templates, 2 template
proofs, the standalone agent-readiness CLI, and the 2 Cloud runtime
destinations. Every one of the 134 appears in a table below, mapped or
excluded, with a reason.

The matrix exists so the adoption frontier is measurable instead of implied.
`putnami architecture validate` proves that every mapped project's cross-domain
dependency is authorized. It cannot prove that the right projects are mapped —
an unmapped project is simply invisible to it. This file is that second half.

## The completion rule

1. Every project inside the frontier belongs to **exactly one** domain. A
   project in two domains is a duplicate-project error the protocol rejects; a
   project in none is a silent hole this file closes.
2. Membership changes only through a reviewed diff that changes **both** a
   `putnami.architecture.json` and this matrix. One without the other is
   incomplete.
3. Exclusion is a decision with an owner, not an omission. A project you cannot
   find in the excluded table is a bug in this file.
4. A new project starts excluded and unmapped. It joins a domain when someone
   states which authority it carries — not when it compiles.

## The 10 domains

| Domain | Owner | Projects | Manifest |
|---|---|---|---|
| `protocols` | `protocols` | 40 | `protocols/putnami.architecture.json` |
| `go-framework` | `framework` | 24 | `go/framework/putnami.architecture.json` |
| `typescript-framework` | `framework` | 14 | `typescript/framework/putnami.architecture.json` |
| `extension-providers` | `extension-providers` | 8 | `go/extension/putnami.architecture.json` |
| `cli` | `cli` | 3 | `tooling/cli/putnami.architecture.json` |
| `sdd` | `sdd` | 1 | `tooling/sdd-extension/putnami.architecture.json` |
| `extension-sdk` | `tooling` | 1 | `tooling/extension-sdk/putnami.architecture.json` |
| `agent-workflows` | `tooling` | 1 | `tooling/contributor/putnami.architecture.json` |
| `observability` | `observability` | 1 | `sites/telemetry.putnami.dev/putnami.architecture.json` |
| `public-docs` | `public-docs` | 1 | `sites/putnami.dev/putnami.architecture.json` |

A manifest does not have to live inside a mapped project. `protocols`,
`go-framework`, and `typescript-framework` each declare a manifest at the root of
the tree they span, because no single member of those domains is more
authoritative than its siblings. Identity comes from the `domain` field, never
from the directory.

## Mapped projects

Each section states the domain's **membership test** first. A project belongs
because it passes that test, not because of where its directory sits.

### `protocols` — 40 projects

**Membership test:** the project decides what one wire document *means*. It owns
that document's strict parser, its validator, and its canonical writer, and no
other project in the repository may reinterpret it.

| Project | Name | Authority it carries |
|---|---|---|
| `/protocols/agentcontext` | `go.putnami.dev/protocol/agentcontext` | The generated agent-guidance document and its CI provenance. |
| `/protocols/analytics` | `go.putnami.dev/protocol/analytics` | The closed audience-measurement wire a web application's tracker posts and its server accepts. |
| `/protocols/architecture` | `go.putnami.dev/protocol/architecture` | ARC/DARC manifests, snapshots, baselines, waivers, and the finding vocabulary this matrix serves. |
| `/protocols/cache` | `go.putnami.dev/protocol/cache` | The remote build-cache negotiate/upload/restore wire. |
| `/protocols/capabilities` | `go.putnami.dev/protocol/capabilities` | The runtime capability declaration and its v1/v2 reader rules. |
| `/protocols/ci` | `go.putnami.dev/protocol/ci` | `putnami.ci.json`: the source-controlled CI intent. |
| `/protocols/cli` | `go.putnami.dev/protocol/cli` | The CLI contract: exit-code taxonomy, result envelopes, output modes. |
| `/protocols/clientcontract` | `go.putnami.dev/protocol/clientcontract` | The `x-putnami-client` metadata of first-party OpenAPI documents and operations. |
| `/protocols/collaboration` | `go.putnami.dev/protocol/collaboration` | The task, proposal and memory provider contracts: operations, references, revisions, capabilities and outcomes. |
| `/protocols/config` | `go.putnami.dev/protocol/config` | Application configuration schema extraction and canonical resolution. |
| `/protocols/contracts` | `go.putnami.dev/protocol/contracts` | The contract IR and its projections. |
| `/protocols/database` | `go.putnami.dev/protocol/database` | The cross-language database resource contract. |
| `/protocols/diagnostic` | `go.putnami.dev/protocol/diagnostic` | The one diagnostic shape every Putnami surface reports in. |
| `/protocols/distribution` | `go.putnami.dev/protocol/distribution` | Release channels, release sets, and artifact identity. |
| `/protocols/doccov` | `go.putnami.dev/protocol/doccov` | The documentation-coverage guard over the wire contracts. |
| `/protocols/doctor` | `go.putnami.dev/protocol/doctor` | The environment-diagnosis wire, contract only, never the engine. |
| `/protocols/events` | `go.putnami.dev/protocol/events` | The event/messaging resource contract. |
| `/protocols/extension` | `go.putnami.dev/protocol/extension` | `putnami.extension.json`: what an extension may declare. |
| `/protocols/features` | `go.putnami.dev/protocol/features` | Feature manifests, specs, criteria, and verification reports. |
| `/protocols/gomod` | `go.putnami.dev/protocol/gomod` | The Go module registry write protocol. |
| `/protocols/http-routes` | `go.putnami.dev/protocol/http-routes` | The deterministic public-route inventory. |
| `/protocols/identity` | `go.putnami.dev/protocol/identity` | The consumer-side identity vocabulary the frameworks share. |
| `/protocols/infra` | `go.putnami.dev/protocol/infra` | Per-project and aggregated infrastructure requirements. |
| `/protocols/job` | `go.putnami.dev/protocol/job` | The job execution context handed to every extension. |
| `/protocols/keyring` | `go.putnami.dev/protocol/keyring` | The cross-language keyring vocabulary. |
| `/protocols/migration` | `go.putnami.dev/protocol/migration` | The schema-migration contract. |
| `/protocols/oci` | `go.putnami.dev/protocol/oci` | Putnami's extensions over the OCI distribution spec. |
| `/protocols/platform` | `go.putnami.dev/protocol/platform` | The platform-target vocabulary. |
| `/protocols/put` | `go.putnami.dev/protocol/put` | The Put registry immutable write protocol. |
| `/protocols/qualify` | `go.putnami.dev/protocol/qualify` | Workload qualification: the smoke contract derived from a workload's contracts, and the verdict of running it. |
| `/protocols/registry` | `go.putnami.dev/protocol/registry` | The credential seam between publishers and the cloud. |
| `/protocols/runner` | `go.putnami.dev/protocol/runner` | The credential-free runner wire: source manifest, execution request, provider RPC and session bundle. |
| `/protocols/runtime` | `go.putnami.dev/protocol/runtime` | The runtime descriptor a built artifact carries. |
| `/protocols/sitecontent` | `go.putnami.dev/protocol/sitecontent` | `site-content-bundle/v1`: content produced in one repository, merged in another. |
| `/protocols/storage` | `go.putnami.dev/protocol/storage` | The object/blob storage resource contract. |
| `/protocols/support` | `go.putnami.dev/protocol/support` | `putnami.support.json`: the reviewed public support catalog. |
| `/protocols/telemetry` | `go.putnami.dev/protocol/telemetry` | OTLP/JSON push, identical from every Putnami runtime. |
| `/protocols/template` | `go.putnami.dev/protocol/template` | `putnami.template.json`: what a project template declares. |
| `/protocols/transaction` | `go.putnami.dev/protocol/transaction` | The unit-of-work contract. |
| `/protocols/workspace` | `go.putnami.dev/protocol/workspace` | Workspace and project configuration, and the project graph model. |

### `go-framework` — 24 projects

**Membership test:** the project publishes part of the `go.putnami.dev/*` runtime
that a tenant Go application links. It decides what an application can express;
the application composes it and owns none of its shape.

| Project | Name | Authority it carries |
|---|---|---|
| `/go/framework/api` | `go.putnami.dev/api` | The REST resource surface and its handler contract. |
| `/go/framework/app` | `go.putnami.dev/app` | The application lifecycle: boot, wiring, shutdown. |
| `/go/framework/cache` | `go.putnami.dev/cache` | The application-level cache surface. |
| `/go/framework/client` | `go.putnami.dev/client` | The typed outbound service client. |
| `/go/framework/config` | `go.putnami.dev/config` | Typed application configuration binding. |
| `/go/framework/ctxutil` | `go.putnami.dev/ctxutil` | Request-scoped context propagation. |
| `/go/framework/database` | `go.putnami.dev/database` | Pools, repositories, and the query surface. |
| `/go/framework/errors` | `go.putnami.dev/errors` | The application error taxonomy. |
| `/go/framework/events` | `go.putnami.dev/events` | Publish/subscribe for application events. |
| `/go/framework/grpc` | `go.putnami.dev/grpc` | The gRPC server and client surface. |
| `/go/framework/http` | `go.putnami.dev/http` | The HTTP server, router, and middleware pipeline. |
| `/go/framework/inject` | `go.putnami.dev/inject` | Dependency injection and container scoping. |
| `/go/framework/keyringstore` | `go.putnami.dev/keyringstore` | The application-side keyring store. |
| `/go/framework/logger` | `go.putnami.dev/logger` | The structured logging contract. |
| `/go/framework/migration` | `go.putnami.dev/migration` | Schema migration execution. |
| `/go/framework/migration/migratecli` | `go.putnami.dev/migratecli` | The migration entry point an application ships. |
| `/go/framework/openapi` | `go.putnami.dev/openapi` | OpenAPI generation from the declared API surface. |
| `/go/framework/parallel` | `go.putnami.dev/parallel` | Bounded concurrency primitives. |
| `/go/framework/platform` | `go.putnami.dev/platform` | Platform detection and platform-specific seams. |
| `/go/framework/proto` | `go.putnami.dev/proto` | Protobuf codegen support for application services. |
| `/go/framework/schema` | `go.putnami.dev/schema` | Struct-to-schema reflection. |
| `/go/framework/security` | `go.putnami.dev/security` | Authentication, authorization, and the identity seam. |
| `/go/framework/storage` | `go.putnami.dev/storage` | Object and blob storage access. |
| `/go/framework/telemetry` | `go.putnami.dev/telemetry` | Traces, metrics, and log export from an application. |

### `typescript-framework` — 14 projects

**Membership test:** the project publishes part of the `@putnami/*` runtime a
tenant TypeScript application links, with the same authority relationship as
`go-framework`.

| Project | Name | Authority it carries |
|---|---|---|
| `/typescript/framework/analytics` | `@putnami/analytics` | Cookieless audience collection, stored in the application's own database. |
| `/typescript/framework/application` | `@putnami/application` | The application container, plugins, and lifecycle. |
| `/typescript/framework/cli-protocol` | `@putnami/cli-protocol` | The TypeScript reader of the CLI wire contracts. |
| `/typescript/framework/client` | `@putnami/client` | The typed outbound service client. |
| `/typescript/framework/database` | `@putnami/database` | Connections, repositories, and the query surface. |
| `/typescript/framework/document` | `@putnami/document` | Document modelling and validation. |
| `/typescript/framework/events` | `@putnami/events` | Publish/subscribe for application events. |
| `/typescript/framework/migration` | `@putnami/migration` | Schema migration execution. |
| `/typescript/framework/runtime` | `@putnami/runtime` | The runtime entry point and environment seam. |
| `/typescript/framework/spectest` | `@putnami/spectest` | The spec-verification assertions a test emits. |
| `/typescript/framework/storage` | `@putnami/storage` | Object and blob storage access. |
| `/typescript/framework/ui` | `@putnami/ui` | The component set and its design tokens. |
| `/typescript/framework/utils` | `@putnami/utils` | The shared primitives the other packages build on. |
| `/typescript/framework/web` | `@putnami/web` | Routing, rendering, islands, and the web pipeline. |

### `extension-providers` — 8 projects

**Membership test:** the project is an extension binary that *answers the
orchestrator* — it owns the probe answers, the lifecycle jobs, the verification
observations for one ecosystem, or the provider results for one backend. Its
authority is what it reports, not what it builds.

| Project | Name | Authority it carries |
|---|---|---|
| `/go/extension` | `@putnami/go` | The Go ecosystem's probe answers, lifecycle jobs, and verification observations. |
| `/python/extension` | `@putnami/python` | The same for the experimental Python surface, which is the extension and its templates only. |
| `/typescript/extension` | `@putnami/typescript` | The same for the TypeScript ecosystem. |
| `/tooling/scaffold` | `@putnami/scaffold` | The template-archive job: it decides what a packaged template contains. |
| `/tooling/clientgen-extension` | `@putnami/clientgen` | The cross-language client-generation job and the shape of what it emits. |
| `/tooling/github-collaboration` | `@putnami/github-collaboration` | The task and proposal provider results for GitHub: how a GitHub issue or pull request maps onto the collaboration contracts. |
| `/tooling/local-collaboration` | `@putnami/local-collaboration` | The task and proposal provider results for the local file-backed store. |
| `/tooling/memory-store` | `@putnami/memory-store` | The memory provider results for the file and Git stores: context selection and compare-and-set checkpoints. |

### `cli` — 3 projects

**Membership test:** the project decides how a workspace resolves, how work is
planned, scheduled, and cached, or what the model of those things is. Internal
packages are not a consumable surface; other domains integrate through wire
contracts only.

| Project | Name | Authority it carries |
|---|---|---|
| `/tooling/cli` | `@putnami/cli` | Workspace resolution, the planner and scheduler, caching, command dispatch, and the `/tooling/doc` reference. |
| `/tooling/cli-documents` | `@putnami/cli-documents` | The gates over this repository's own documents: the governance surface, the contributor recipe, the release plan and its verdict, the public-cut candidate and the shipped manifests. A separate project so that their repository-wide inputs key only these tests. |
| `/tooling/cli-model` | `@putnami/cli-model` | The CLI's data model — the workspace graph, extension descriptions, job definitions — extracted so it can be depended on without the orchestrator. It is an unpublished internal companion and no workspace project may import it. |

### `sdd` — 1 project

**Membership test:** the project decides what a valid feature, spec,
architecture declaration, or contract *is*.

| Project | Name | Authority it carries |
|---|---|---|
| `/tooling/sdd-extension` | `@putnami/sdd` | Feature, spec, architecture, and contract validation, and the method documentation under `/tooling/sdd-extension/doc`. |

### `extension-sdk` — 1 project

**Membership test:** the project decides what an extension is *able to say* to
the orchestrator and in what shape.

| Project | Name | Authority it carries |
|---|---|---|
| `/tooling/extension-sdk` | `putnami-extension-sdk` | The JSONL emitter, the job-context parser, the lifecycle runner, result envelopes, and the ARC authoring builder. |

### `agent-workflows` — 1 project

**Membership test:** the project decides which agent workflows Putnami publishes
and what a published workflow archive contains.

| Project | Name | Authority it carries |
|---|---|---|
| `/tooling/contributor` | `@putnami/contributor` | The contributor skills and workers, their host adapters, and the content policy their extension ships under. The repository-root `.agents`, `.claude`, and `.codex` trees are generated from this source; the audit skill and host policy files stay outside this authority. |

### `observability` — 1 project

**Membership test:** the project decides what anonymous CLI usage means once it
is received — what is accepted, what is dropped, how it is bucketed, and how
long it is kept.

| Project | Name | Authority it carries |
|---|---|---|
| `/sites/telemetry.putnami.dev` | `telemetry.putnami.dev` | The usage ingest, its sanitizer, and the daily aggregates. |

### `public-docs` — 1 project

**Membership test:** the project decides the *published information
architecture* — which trees appear, at which section, in what order — while
owning none of their content.

| Project | Name | Authority it carries |
|---|---|---|
| `/sites/putnami.dev` | `putnami.dev` | Placement, navigation, and the site's own authored pages. Five documentation trees are copied in from five other domains, and none of them is edited here. |

## Excluded projects

Samples, templates, and template proofs consume the public API without owning a
fact another repository project depends on. The standalone agent-readiness CLI
is also outside the current architecture frontier: it collects an anonymous
repository payload, but no project in this repository consumes or depends on
that payload. None of the existing domains owns its assessment semantics.
Mapping it to one of them would imply authority that domain does not hold.
Each exclusion has an owner and a condition for joining a domain.

### Standalone Intelligence CLI — 1 project

**Why excluded:** the agent-readiness extension collects a repository-local
payload for an optional external report. It does not decide workspace planning,
ecosystem lifecycle results, or framework behavior, and no repository project
depends on its payload. Its extension packaging alone does not make it an
`extension-providers` member.

**What would change it:** if a public project consumes its payload as an
authoritative contract, or this repository starts owning the report's
assessment semantics, review a matching Intelligence domain and the exact
cross-domain permissions before adding it to the frontier.

| Project | Class | Owner |
|---|---|---|
| `/intelligence/agent-readiness` | Standalone repository collector | `intelligence` |

### Cloud runtime destinations — 2 projects

**Why excluded:** `go.putnami.dev/cloud/runtime` and `@putnami/cloud` are
client code of the hosted platform. They add config, secrets and, for
TypeScript, Event Server destinations to the Go and TypeScript frameworks
through each framework's registration seam. No repository project links them
from the workspace, and no existing domain owns the hosted platform's wire
contracts. Mapping them to `go-framework` or `typescript-framework` would give
that domain an authority it does not hold.

**What would change it:** if a repository project links one from the workspace,
review a Cloud domain and the exact cross-domain permissions before adding it
to the frontier.

| Project | Class | Owner |
|---|---|---|
| `/cloud/runtime/go` | Hosted-platform client library | `cloud` |
| `/cloud/runtime/typescript` | Hosted-platform client library | `cloud` |

### Samples — 28 projects

**Why excluded:** a sample is a *proof*. It exists to demonstrate that the public
API works and to fail loudly in the end-to-end suite when it stops working. It
declares no export, and nothing in the repository depends on it — the dependency
arrow points only inward.

**What would change it:** a sample that another project imports, or that starts
carrying a declaration the repository must keep true, has stopped being a proof.
It becomes a first-party project, joins a domain, and its dependencies become
reviewed permissions.

| Project | Class | Owner |
|---|---|---|
| `/go/samples/application` | Go sample | `framework` |
| `/go/samples/capabilities-proof` | Go sample | `framework` |
| `/go/samples/library` | Go sample | `framework` |
| `/go/samples/migrations-feature` | Go sample | `framework` |
| `/go/samples/service-to-service` | Go sample | `framework` |
| `/go/samples/service-to-service/clients/ts` | Nested cross-language client of a Go sample | `framework` |
| `/go/samples/service-to-service/consumer-ts` | TypeScript consumer of a Go sample | `framework` |
| `/go/samples/simple-api` | Go sample | `framework` |
| `/go/samples/task-api` | Go sample | `framework` |
| `/go/samples/unit-of-work-proof` | Go sample | `framework` |
| `/python/samples/application` | Python sample | `extension-providers` |
| `/python/samples/library` | Python sample | `extension-providers` |
| `/typescript/samples/01-hello-world` | TypeScript sample | `framework` |
| `/typescript/samples/02-rest-api` | TypeScript sample | `framework` |
| `/typescript/samples/03-web` | TypeScript sample | `framework` |
| `/typescript/samples/04-configuration` | TypeScript sample | `framework` |
| `/typescript/samples/05-dependency-injection` | TypeScript sample | `framework` |
| `/typescript/samples/06-database` | TypeScript sample | `framework` |
| `/typescript/samples/07-authentication` | TypeScript sample | `framework` |
| `/typescript/samples/08-real-time` | TypeScript sample | `framework` |
| `/typescript/samples/09-events` | TypeScript sample | `framework` |
| `/typescript/samples/10-service-to-service` | TypeScript sample | `framework` |
| `/typescript/samples/10-service-to-service/clients/go` | Nested cross-language client of a TypeScript sample | `framework` |
| `/typescript/samples/10-service-to-service/clients/ts` | Nested TypeScript client of a TypeScript sample | `framework` |
| `/typescript/samples/11-storage` | TypeScript sample | `framework` |
| `/typescript/samples/12-caching` | TypeScript sample | `framework` |
| `/typescript/samples/13-fullstack-app` | TypeScript sample | `framework` |
| `/typescript/samples/14-capabilities` | TypeScript sample | `framework` |

The three nested client projects are excluded for the same reason as their
parents. They exist to prove that a Go service and a TypeScript service can call
each other through generated clients. Their only consumer is the sample above
them.

### Templates — 7 projects

**Why excluded:** a template is a *scaffold input*. Its files are the starting
state of somebody else's project, not code this repository runs. The authority
over what a template may declare already sits in `protocols` (the
`putnami.template.json` contract) and over what a packaged template contains in
`extension-providers` (`/tooling/scaffold`). A template domain would restate
both and decide nothing.

**What would change it:** a template that stops being a copied starting state —
for example, one whose scaffolded output must satisfy a contract this repository
enforces on generated projects — has acquired an authority and needs a domain.

| Project | Class | Owner |
|---|---|---|
| `/go/templates/go-library` | Go template | `extension-providers` |
| `/go/templates/go-server` | Go template | `extension-providers` |
| `/python/templates/python-library` | Python template | `extension-providers` |
| `/python/templates/python-server` | Python template | `extension-providers` |
| `/typescript/templates/typescript-library` | TypeScript template | `extension-providers` |
| `/typescript/templates/typescript-server` | TypeScript template | `extension-providers` |
| `/typescript/templates/typescript-web` | TypeScript template | `extension-providers` |

### Template proofs — 2 projects

**Why excluded:** a template proof is a *test*. It renders each template of its
language into a throwaway workspace wired to this repository's framework
sources, then builds the result and runs the tests the template ships. It holds
a template to the framework's public API, the same thing a sample proves for
hand-written code. It declares no export, and nothing depends on it. It does not
make a template an authority either: the proof checks that the copied starting
state works, and no other project reads what it decides. The design is recorded
in `tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md`.

**What would change it:** a proof that other projects start to read — for
example, one that publishes the rendered output as a contract — has acquired an
authority and needs a domain.

| Project | Class | Owner |
|---|---|---|
| `/go/templates/proof` | Go template proof | `extension-providers` |
| `/typescript/templates/proof` | TypeScript template proof | `extension-providers` |

## ARC permissions and DARC flows are not the same thing

The 21 imports in this repository split into two kinds. Reading one as the other
is the mistake this section exists to prevent.

| | ARC project permission | DARC runtime flow |
|---|---|---|
| What it authorizes | One package or module dependency edge | One movement of data between domains at run time or build time |
| Where it shows up | `bindings` on a `reference` import | `transport`, `consistency`, `localModel`, `deletion` on a non-reference import |
| Count today | 13 imports, 526 bindings | 8 imports, 0 bindings |
| What it says about data | Nothing | Everything: freshness, failure, ordering, deletion |

**A binding is a compile-time fact.** `/go/framework/http` may import
`go.putnami.dev/protocol/http-routes`. That is all it says. No fact is copied, no
staleness bound applies, and nothing is enforced at run time. All 526 bindings in
this repository are of this kind, and 13 of the 21 imports carry them.

**A DARC flow is a data-movement fact.** It states what is copied, how fresh it
must be, what happens when it is missing or stale, and how the producer's
deletion reaches the copy. The eight are:

| Import | Mode | What moves |
|---|---|---|
| `cli.verification-observations.v1` | snapshot | Test-run observation artifacts into the executable-spec gate |
| `cli.workspace-probe-view.v1` | projection | Provider probe answers into the recorded workspace index |
| `cli.usage-telemetry.v1` | command | Anonymous usage records to the observability ingest |
| `public-docs.workspace-documentation.v1` | snapshot | `/tooling/doc` → `public/docs/08-tooling-&-workspace` |
| `public-docs.method-documentation.v1` | snapshot | `/tooling/sdd-extension/doc` → `public/docs/07-spec-driven-development` |
| `public-docs.typescript-framework-documentation.v1` | snapshot | `/typescript/doc/framework` → `public/docs/09-frameworks/01-typescript` |
| `public-docs.go-framework-documentation.v1` | snapshot | `/go/doc/framework` → `public/docs/09-frameworks/02-go` |
| `public-docs.python-surface-documentation.v1` | snapshot | `/python/doc/framework` → `public/docs/09-frameworks/03-python` |

The five documentation snapshots share one carrier contract,
`putnami.documentation-tree.v1`. It **names a carrier, not a published wire
schema**: the markdown tree that one `generate.assets` entry copies, addressed by
a deterministic digest over its sorted (relative path, content digest) pairs. Two
trees with the same digest are the same document, which is why `tree_digest`
serves as both the source version and the idempotency key. All five fail closed
on a missing tree and publish a stale one, because a documentation gap is worse
than documentation that is a day old.

`/sites/putnami.dev` consumes `typescript-framework` twice, once each way: a
`reference` import for the runtime packages it links, and a `snapshot` import for
the documentation tree it copies. Same producer domain, two unrelated contracts.
`putnami architecture sync` refuses those five edges with `ambiguous-import`: it
will not pick which of two declared contracts authorizes a dependency, and that
refusal is correct. A human decided instead — a package dependency can only
implement the `reference`, never the file copy — so the five bindings on
`public-docs.application-runtime.v1` are hand-authored. That decision is held to
its Go authoring by
`tooling/sdd-extension/architecture_repository_pin_test.go`.

## Candidate flows deliberately not modelled

These are real cross-domain movements. None is modelled today, and each one's
reason is recorded here so the next reader does not have to rediscover it.

### `/tooling/cli/scripts/install.sh` → `public/install.sh`

**Not modelled: it is a release artifact, not a documentation fact.** The site
copies the installer so `curl putnami.dev/install.sh` works. Dressing that up as
a documentation snapshot would attach the wrong contract to it — a staleness
bound and a tree digest say nothing useful about an installer, whose real
correctness question is which CLI version it resolves.

**What would change it:** the installer acquiring a versioned distribution
contract of its own, at which point the site's copy becomes a snapshot of that
contract rather than of a file.

### `/LICENSE.md` → `public/LICENSE.md`

**Not modelled: no domain owns it.** It is a workspace-root legal file. No
domain maps the workspace root, so there is no producer to declare the export,
and inventing one would mean creating a domain to hold a single file.

**What would change it:** a domain taking ownership of the repository's legal
texts. Until then the honest answer is that this copy has no owner, and saying so
is better than assigning one for the sake of a green matrix.

### `sites/putnami.dev` reading `protocols/*/schemas/*.json` at build time

**Not modelled: it is invisible to the project graph and the shape is not settled.**
`sites/putnami.dev/src/main.ts` globs every protocol's JSON schemas and republishes
them under `public/schemas/`. It is a genuine `public-docs` → `protocols` flow, but:

- it creates no project-dependency edge, because it is a filesystem glob rather
  than a package import, so `architecture validate` reports nothing today;
- it is a *set* of documents selected by pattern, with one deliberate exclusion
  (`protocols/infra/schemas/infra-aggregated.json`), and a snapshot import names
  one tree, not a filtered glob;
- the same reasoning covers the site's build-time read of the workspace-root
  `putnami.support.json`.

Modelling it needs a decision first: whether the exported fact is "every
published schema" as one tree, or one export per protocol project. That decision
belongs to `protocols`, not to this matrix.

**What would change it:** `protocols` declaring a published-schema export. The
consuming import then follows mechanically.

## Where each declaration is pinned

Every one of the 10 manifests is the projection of a Go authoring held by
`architecture.Pin`. A hand edit the program does not make fails a test run.

A pin must not create a new observed dependency edge, because the authoring runs
through `go.putnami.dev/sdk/extension/architecture` and any hosting project
acquires an edge to it. That rules out `protocols/*` (published modules that must
not depend on tooling), `go/framework/*` and `typescript/framework/*` (the
inversion would put the framework under the tooling SDK), `/sites/putnami.dev`
(no Go module), and `/sites/telemetry.putnami.dev` (a pin there cost seven
unrelated cross-domain permissions when it was tried). Those pins are **hosted**,
not adopted: each domain still owns its declaration, and the hosting file may not
change one without the owning team's review.

| Domain | Pinned in |
|---|---|
| `extension-sdk` | `tooling/extension-sdk/architecture_pin_test.go` (owned) |
| `cli` | `tooling/cli/internal/specgate/architecture_pin_test.go` (owned) |
| `extension-providers` | `go/extension/cmd/putnami-go/architecture_pin_test.go` (owned) |
| `sdd` | `tooling/sdd-extension/architecture_pin_test.go` (owned) |
| `protocols`, `observability` | `tooling/sdd-extension/architecture_pin_test.go` (hosted) |
| `go-framework`, `typescript-framework`, `public-docs`, `agent-workflows` | `tooling/sdd-extension/architecture_repository_pin_test.go` (hosted) |

## Related

- [ADR 0001 — declarations are authority, observations are evidence](adr/0001-declarations-are-authority-observations-are-evidence.md)
- [`protocols/architecture/README.md`](../README.md) — the wire contracts and the finding vocabulary
