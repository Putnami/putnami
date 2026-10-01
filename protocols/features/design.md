# Feature evidence and Capability Manifest v2 RFC

## Provisional code-first design graph

The next authoring iteration keeps the durable Capability Manifest v2 wire but
removes it from the normal feature-authoring loop. Product intent is declared
once on its native module; framework APIs derive the technical graph from the
same declarations they already execute.

```go
app.NewModule("tasks").
    Feature(app.Feature{
        ID: "tasks/manage", Name: "Task management",
        Outcome: "Users can create, list, update and complete tasks",
        Owner: "samples",
    }).
    Use(taskAPI).
    Use(taskEvents)
```

```ts
module('tasks')
  .feature({
    id: 'tasks/manage',
    name: 'Task management',
    outcome: 'Users can create, list, update and complete tasks',
    owner: 'samples',
  })
  .use(api())
  .use(events());
```

No API, event, service, schema, migration, or generated-client relationship is
repeated in feature metadata. Native contributors emit a deterministic,
disposable `.gen/design/graph.json`; `putnami features inspect <id>` reads only
the requested feature subgraph. Each relationship says whether its authority is
`exact`, `derived`, `heuristic`, or `currently-unmodeled`, so tooling never
presents source-containment inference — or a deliberately unproven lineage — as
a runtime fact.

Generated clients carry an exact descriptor containing the spec hash and an
operation table. One generated client routinely spans several producer features,
so the producer project and feature are recorded on each operation rather than on
the client: an operation whose endpoint owner declares no feature is left
unattributed instead of inheriting the client generator's own feature. Each
operation is keyed by its canonical operation ID, which is preserved
independently from the language symbol a generator derives from it (canonical
`getV1_Operator_Cli-usage` against a TypeScript method `getV1_Operator_Cli_usage`).
Go and TypeScript runtimes resolve the invoked operation by that canonical ID and
attach its method, path, spec hash, project, and feature to the outgoing request.
This makes the chain
`runtime call -> generated client -> API operation -> producer feature`
available without guessing from a deployment URL or package name.

The graph compatibility marker remains `provisional`. The native code is the
source of truth; the graph is rebuilt atomically and is never hand-authored or
promoted as another corpus to synchronize. Durable v1 manifests and evidence
remain readable for human-authority claims, while framework technical
provenance is derived from native registrations and generated artifacts rather
than a parallel writer seam.

A technical fact earns a maturity requirement through one explicit association
declared beside the native feature — an authored feature, one of its authored
requirements, and one contribution the same producer publishes. The build
derives every other field and emits the record; nothing is inferred from
containment, naming, or graph proximity, and no design-graph authority value can
promote an edge into evidence. `BuildGeneratedEvidence` is the single resolver
both languages run, pinned against each other by the
`generated-evidence.golden.json` vector. See
[`doc/adr/0003-generated-feature-evidence.md`](doc/adr/0003-generated-feature-evidence.md).

### Provisional cross-runtime producer contract

The graph's node and edge kinds, authority values, and semantic relationships
are language-neutral. Node IDs are stable producer identities and consumers
must treat them as opaque strings rather than reconstructing facts by parsing
their segments. Where Go and TypeScript expose the same native identity, their
producers use the same shape:

- an API operation is `api.operation:<METHOD>:<canonical-path>` and carries
  `method`, `path`, and the derived or declared `operationId`;
- an event topic is `event.topic:<name>`;
- a transactional outbox is `event.outbox:<name>`;
- a generated client links to every operation in its embedded operation table
  with exact `generatedFrom` edges, each carrying the same canonical
  `operationId` the client descriptor and the runtime trace use, plus that
  operation's `producerProject` and `producerFeature` when it has one — the
  `client.generated` node itself never claims a feature;
- a hand-written typed client is `client.typed:<producerProject>:<clientName>`
  and is never emitted as `client.generated`; and
- event-handler identities use a stable declaration identity, never a registry
  position or discovery-order index.

Configuration, infrastructure, lifecycle, and test facts use the same bounded,
language-neutral contract:

- a configuration definition is `config:<path>`, carries `path`, and contains
  neither its default nor its resolved runtime value;
- an infrastructure requirement is `infra:<kind>:<name>` and carries only its
  capability-manifest infrastructure `kind` and logical `name`;
- a lifecycle registration is `lifecycle:<phase>:<moduleId>` and carries
  `phase`; every callback registered for the same module and phase deliberately
  folds into that one node; and
- a declared test is `test:<kind>:<name>` and carries its bounded test `kind`
  (`unit`, `integration`, `conformance`, or `e2e`), plus — when the test
  declares which authored executable-spec checks it protects — a
  `proves` property of sorted, space-separated
  `feature#requirement#check` triplets. That binding is discoverability only:
  it lets a reader (and `sdd.feature_context`) answer "which declared test is
  expected to prove this check", and can never itself prove a passing test —
  only the test adapter's observed verdict does.

Owner-scoped registrations attach all four to their module with an exact
`contains` edge. A root-owned TypeScript infrastructure registry instead uses a
derived `contains` edge with declaration provenance when source containment or
an explicit source selection proves the feature attribution; it emits no
ownership edge when neither does. All are observational: adding graph
production cannot change configuration, provision resources, register
callbacks, or execute tests. Their native seams are deliberately narrower than
`DesignContributor`: configuration reuses the existing configuration
contributor, infrastructure and tests expose typed descriptor lists, and
lifecycle is observed from the framework's existing registration interfaces. A
plugin does not need to implement the arbitrary graph-writer seam for any of
these facts. Names and kinds must be explicitly declared; producers never infer
a test from a filename, an infrastructure resource from an environment value,
or a lifecycle identity from registration order or a JavaScript constructor
name.

Provenance records the declaration source only where the native seam owns one.
Go configuration producers can recover the configuration-definition method
location and publish it; TypeScript's configuration interface carries no source
location today and therefore omits it. Infrastructure and test descriptors may
supply their exact declaration location in either runtime. The semantic node
stays provenance-free because several modules can require the same config path
or logical resource; each `contains` edge carries its own observable declaration
source instead. When several declarations on one module establish the same
edge, the producer keeps the lexically first normalized provenance so output is
independent of registration and scan order. Producers omit provenance when that
source is unavailable instead of inventing one. Lifecycle nodes have no
callback provenance because they are stable module-and-phase aggregates and may
represent several registrations. The exact authority of the ownership edge
asserts native registration on the module; it does not upgrade an absent source
location into a provenance claim.

Current producer coverage is intentional and provisional. Go and TypeScript
both project native configuration definitions, typed infrastructure
requirements, explicit test descriptors, and the lifecycle phases their
application runtimes expose. Their database, event, and storage plugins publish
the logical datasource, topic, and bucket requirements they register without a
generic design contributor; Go schema contributors additionally publish their
declared databases. The shared equivalence fixture proves byte-identical graph
semantics for the common `configure`, `start`, and `stop` phases after removing
provenance alone. TypeScript additionally has native `generate` and `migrate`
phases; Go has no equivalent registration today and does not synthesize them.
Conversely, a producer with no native test descriptor emits no test node.
These are documented producer asymmetries, not permission to guess missing
facts.

TypeScript's database, event, and storage plugins are normally mounted once on
an unfeatured application root. Their bounded infrastructure descriptors retain
the table, handler/topic, or bucket declaration sources and resolve those
sources through the same feature-directory or explicit source-selection rules
as their other native facts. That produces one derived ownership edge per
feature that actually contains a declaration; two declarations in one feature
fold to one edge with canonical provenance, while declarations in two features
produce two edges to the shared semantic infra node. A root-level primary
datasource or other requirement with no declaration source gets no feature
ownership edge rather than being assigned to every feature.

This extension adds node kinds only and reuses `contains`; it does not change
the durable Capability Manifest v2 compatibility promise. The design graph and
its compact CLI/MCP projections remain `provisional`. Graduating them requires
an explicit compatibility decision for node properties, producer coverage, and
the lifecycle aggregation boundary; consumers must therefore use kind and
relationships rather than rely on producer-specific optional provenance.

Event production is three separate relationships because they are three
separate claims, and collapsing them would let the graph promise a delivery the
runtime has not performed:

- `module -enqueues-> event.outbox` is the commit-time write. It is exact: the
  outbox is registered on that module by a native declaration. A row is durable
  when its business transaction commits, and nothing has been published yet.
- `event.outbox -publishes-> event.topic` is the relay-time publication a
  claimed row produces. It is exact for every topic the outbox declaration
  names.
- `module -publishes-> event.topic` remains the direct transport publish. It is
  derived, because the producer attributes a publisher call site to the nearest
  enclosing feature source directory rather than to a registration.
- `event.handler -subscribes-> event.topic` is unchanged and stays exact.

The outbox declaration is observational: it owns no table, transaction, relay
loop, or retry policy, and a relay implementation remains application code. Its
node carries only bounded identifiers — the staging `table` and `datasource`
names — and never a payload, row, or credential value.

The `event.outbox` node and `enqueues` edge are part of the shared vocabulary
and every runtime's parser accepts them, but only TypeScript currently emits
them: Go has no native outbox declaration, and inventing one from names or
copied bindings is exactly the inference this graph refuses. This is the same
producer-specific latitude as `data.table` below.

#### Typed clients and unmodeled relationships

A `client.typed` node is a hand-written client that declares which producer it
calls. Its identity is derived only from declared metadata — never from a base
URL, host, deployment URL, package name, or import path — and its properties are
a bounded set: `producer`, `language`, the producer `feature` when it is
declared, and `operations`, the declared operation table rendered as sorted
`METHOD path` entries. Tokens, authorization headers, request bodies, and
environment values have no representation and must never be published.

Each module that composes the client contributes its own `calls` edge to that
node, so consumer attribution comes from actual composition rather than from
parsing URLs or imports. The producer's `api.operation` nodes exist only in the
producer project's graph: an in-project consumer links to them with exact
`calls` edges, while a cross-project consumer carries the same operations as
node properties instead of minting edges the producer's graph cannot resolve.

`currently-unmodeled` is the fourth and weakest authority, ordered after
`heuristic`. It means the framework holds a native declaration that the
relationship exists but cannot prove its producer lineage exactly and refuses to
guess it — a typed client that names its producer project but not its producer
feature publishes its consumer call edges at this authority. Absence of proof is
published as absence, never as inference. Consumers that weaken an authority
along a path must derive their ladder from the protocol's ordered vocabulary:
treating an unrecognized authority as `exact` would let an unmodeled step
present itself as a proven one.

#### Services, repositories, and data relationships

A `service` is one injectable dependency, identified by its dependency-injection
token label. Every producer that observes the same token mints a byte-identical
node, because a graph rejects two conflicting declarations of one id: an
endpoint that injects a token, the module registration that provides it, and a
data producer that knows what the token targets all describe one service. The
node therefore carries the token identity and nothing else; what a service *is*
comes from its relationships. A provider registration contributes
`module --contains--> service` for the module that declares it and
`service --injects--> service` for every dependency the registration declares.
Both are exact: the container resolves exactly those tokens. A registration
whose declared dependency list is incomplete omits edges; it never makes an
emitted one wrong. Identity and dependency edges exist for providers declared
outside any feature scope too — one application owns one container tree — while
ownership stays feature-scoped, so a provider that no feature reaches is simply
unreachable in every feature projection.

Data relationships follow the same declaration-only rule:

- `data.table` is emitted only by runtimes with a native table declaration API,
  and only for a relation that API names. Its identity carries the declared
  namespace as well as the datasource and the relation name, because a relation
  is unique only within its schema: two declarations that share a datasource and
  a name but declare different schemas are two relations, and collapsing them
  onto one identity is a conflicting declaration rather than a duplicate. A
  relation that exists solely because some migration's SQL creates it is not
  represented: recovering it would mean parsing statement text or a filename, and
  this graph never promotes that to an exact fact. Such a relation becomes
  visible when — and only when — the runtime gains native metadata that names it.
- `data.schema` is the datasource namespace. A migration source declares the
  namespace it writes into and a table declaration may declare the namespace it
  lives in; both mint the same node, including its `engine`, so the two
  declarations reconcile instead of conflicting, and the table declaration adds
  `data.schema --contains--> data.table`. A table that declares no namespace
  inherits one at deploy time, which is not a declaration and mints nothing.
- a repository is a `service` — its DI token is its identity — related to the
  table it targets by exact `reads` and `writes` edges. The declaration binds a
  repository to exactly one relation and grants both directions, so both edges
  follow from the declaration rather than from which methods a caller happens to
  reach. A repository node exists only where a native declaration minted the
  token; a table on its own declares a relation, not a gateway over it.

A contributor is normally native to the feature scope of the module it is
registered on. A contributor whose declarations each carry their own source
location is the exception and says so: it runs even when its owner sits outside
every feature scope, and containment attributes each declaration to the feature
directory that contains it. This is what an application-wide plugin needs — one
database runner and one primary datasource belong on the application root, which
declares no feature, so owner-scoping would drop every declaration it reports.
A declaration outside every feature directory remains an unreachable node that no
feature projection selects.

TypeScript emits the repository and registration projections today; Go's
adapters have no equivalent native declaration to read yet, and this stays a
producer-specific fact until they do.

The provisional graph also permits producer-specific facts where the native
framework models differ. Go has nominal request/response types, so its
`api.schema` identity uses the Go type and schema role and exposes `goType` and
`role`. TypeScript may receive anonymous schemas, so its schema identity uses
the operation and role and exposes `fields` and `role`. TypeScript topics expose
schema `fields` and an optional `version`; Go event registrations currently
retain the topic name but not that schema metadata. A Go event handler uses its
resolved function symbol, while a TypeScript handler uses its project-relative
declaration source. `data.table` is emitted only by runtimes with a native table
declaration API; schema and migration nodes remain available otherwise.

Properties not listed as common above are an open, producer-specific diagnostic
surface during the provisional phase. Consumers select by node/edge kind and
follow relationships; they do not require another runtime to synthesize facts
its native API does not own. Any future requirement for byte-identical
cross-runtime nodes is a compatibility change and needs an explicit protocol
decision before this marker can graduate.

#### Workspace projects and commands (tooling-minted)

`project` and `command` are workspace-level technical kinds, and `dependsOn` is
their project-to-project dependency edge. They are minted by tooling projections
from the loaded workspace model — never by a framework producer, which only
knows its own project. This is a correction made structural: a project
or a command is represented as its own node kind and can never be promoted to a
product feature, because only an authored feature declaration mints a `feature`
node.

- `project:<name>` is the semantic project identity the producer wrote into its
  graph. When the workspace model resolves it, the node carries `type` and
  `path` properties with putnami.json provenance; when it does not, the bare
  declared identity is published without invented properties.
- `project --contains--> module` is exact and attaches each root module of a
  scoped implementation (a module no other scoped module contains) to its
  owning project. Nested modules stay owned by their parent module.
- `project --dependsOn--> project` carries one direct declared workspace
  dependency between projects that implement the same feature. It is never
  transitive and never inferred: an indirect dependency is absent.
- `command:<name>` is one binary entrypoint declared by the project's
  putnami.json `bin` field — the workspace protocol's native command
  registration — exposed via an exact `project --exposes--> command` edge. The
  string form names the command after the project's unscoped name, exactly as
  the manifest format defines; the map form names each command explicitly. A
  project without a `bin` declaration exposes no commands: nothing is derived
  from project type, name, build output, or a language manifest.

Workspace nodes are containment context, not runtime routes: critical paths
never terminate on or travel through `project` or `command` nodes. The putnami
CLI's own command catalog is deliberately not projected into workspace graphs —
the tool describing a workspace is not part of that workspace's design. Commands
declared only in a language manifest (npm `bin`) are a language-extension fact
and remain unrepresented until that extension contributes them natively.

### Composition association (TypeScript only)

Descendant-only inheritance cannot express a feature that spans sibling modules
of a library composer plus file routes owned by a workload root. TypeScript
therefore accepts an optional design-time composition on the one declaration —
`module(...).feature(definition, { modules, sources })` — that selects existing
native owners instead of copying bindings, wrapping modules, or claiming a
common ancestor. Go has no equivalent today; parity is a separate decision and
is not required by this contract.

The association adds no wire vocabulary. A selected module is still
`feature -implementedBy-> module` with `exact` authority, because selecting a
module object is an explicit assertion with no inference between the declaration
and the module. A selected source path is still `module -exposes-> operation`,
but with `derived` authority and the matched declaration source as provenance,
because the framework — not the author — resolves which declarations live under
a path. Both carry the property `association: "selected"` so a reader can tell a
selection from a declaration or from source-directory containment. Consumers
that ignore the property see an ordinary containment edge.

- **Status:** Implemented
- **Scope:** repository-authored feature intent, repository evidence,
  Capability Manifest contribution references, and local tooling
- **Compatibility:** Capability Manifest v2 is durable; feature snapshot and
  delta payloads remain provisional and require a separate compatibility RFC

This RFC is the implementation contract for the slices that follow it. It turns
the bounded workspace feature-graph direction into one local,
framework-neutral seam:

```text
authored intent -> evidence -> exact technical contribution -> derived assessment
```

The sources of truth remain separate. Feature manifests own product intent;
code and Capability Manifests own technical facts; evidence records associate
the two; and snapshots are disposable projections. No feature field
participates in application composition or runtime behavior.

## Decisions

1. Capability Manifest v1 remains strict and readable. V2 is an explicit new
   wire version, not an additive change to v1.
2. Every v2 contribution carries one uniform semantic identity. Migration
   identity includes its migration kind.
3. Identity is timeless. Declaration and generated-artifact locations are
   precise provenance and never participate in identity.
4. Authored intent is discovered only from `putnami.features.json` at the
   workspace root and exact project roots. Evidence is stored in separate
   artifacts.
5. Current maturity is derived from evidence. Missing, stale, and contradicted
   evidence remain visible, and human authority is mandatory for product
   promises.
6. Local tooling lives under `putnami features`. The snapshot and delta data
   payloads remain explicitly provisional and require a separate compatibility
   decision.
7. Every parse, lookup, aggregation, and serialization path is strict,
   deterministic, contained to its declared root, and redaction-safe.
8. An adopter repository trials the protocol in parity stages. Adoption and
   merge are separate product decisions; feature metadata remains
   build-time-only throughout the trial.

## Adoption trials and authoring interpretation

An adoption trial is an experiment gate, not a delivery gate. Running the
protocol against a representative repository, recording immutable validation
and comprehension results, and publishing an explicit adoption decision
satisfies the gate. Merging the experiment branch, replacing an existing
renderer, or retaining its migrated corpus on `main` is not an acceptance
criterion. A maintainability NO-GO is a successful experimental result.
A decisive stop condition may end later adoption trials, but every unrun gate
must remain explicit and must never be reported as passed.

A hand-maintained JSON corpus exercises the strict wire and aggregation
behavior, but duplicating technical truth into hand-maintained JSON is not an
acceptable steady-state authoring model. A trial corpus stays experiment
evidence, never product source.

The resulting authoring boundary is:

1. For the native design graph, declare stable identity, outcome, and owner once
   with Go `Module.Feature(...)` or TypeScript `module(...).feature(...)`.
2. Keep technical truth in ordinary framework registrations. API, event, data,
   migration, DI, and generated-client facts are projected automatically; do
   not restate them in feature wrappers or copied evidence bindings.
3. Use a v1 `putnami.features.json` overlay only when a consumer explicitly
   needs durable maturity targets, relations, or requirements that the
   provisional graph does not model. It is not required for native inspection.
4. Hand-author evidence only for human authority or for a bounded integration
   that has no producer. Do not use repeated broad attestations or one generic
   artifact to simulate independently verified maturity stages.
5. Treat graphs, source bindings, digests, snapshots, maps, and promoted producer
   files as generated projections. Authors and agents do not repair their fields
   individually.
6. Consume the graph through scoped CLI or MCP projections; do not make
   whole-corpus JSON the default agent context.

Feature protocol v1 defines durable discovered repository bytes, not the
routine framework authoring language. Its readers and human-evidence semantics
remain compatible. Stable feature IDs may also be shared with future flag,
entitlement, or policy systems, while both the durable protocol and provisional
graph remain observational and runtime-inert.

## Non-goals

- hosted Intelligence ingestion, persistence, indexing, or history;
- feature flags, entitlements, rollout evaluation, or experiments;
- policy, release, or promotion gates;
- automatic design-partner or GA declarations;
- a universal completion percentage;
- copying route, config, migration, or infrastructure facts into feature
  manifests;
- changing application activation, dependency injection, startup, or request
  handling based on feature metadata.

## 1. Capability Manifest v2

### Version transition

Capability Manifest v1 is frozen. Its schema, strict parser, canonical bytes,
and fixtures do not change. In particular, a v1 reader continues to reject
unknown fields; v2 fields must never be emitted under `protocolVersion: 1`.

V2 uses `protocolVersion: 2`, a distinct v2 schema, distinct wire types, and a
strict v2 parser. A reader selects its parser from the top-level version and
then rejects every unknown field. A missing or non-integer version is invalid;
it is not guessed from the document shape.

In-tree consumers retain explicit v1 and v2 read paths. Producers continue to
write v1 until their consumer path accepts v2 and parity tests pass. A v1
document is never rewritten or silently labeled as v2 in memory: compatibility
adapters may expose normalized lookup results, but must retain the source
version in diagnostics and derived output.

### Uniform contribution identity

Every concrete v2 contribution has this required identity block, serialized
before its contribution-specific fields:

```json
{
  "identity": {
    "ownerProject": "go.putnami.dev/example/billing",
    "kind": "migration",
    "subkind": "sql",
    "key": "billing"
  }
}
```

`ownerProject` is the stable semantic owner that originally declares the
contribution. It is independent of `Manifest.project`: the latter identifies
the transport/container being described and may aggregate entries owned by
dependencies. A local entry normally has `identity.ownerProject ==
Manifest.project`; a verbatim dependency entry intentionally does not.

| Container | Entry owner/provenance project | Reference owner | Result |
| --- | --- | --- | --- |
| `example/dependency` | `example/dependency` | `example/dependency` | original |
| `example/workload-a` | `example/dependency` | `example/dependency` | same copied contribution |
| `example/workload-b` | `example/dependency` | `example/dependency` | same copied contribution |
| `example/workload-a` | `example/workload-a` | `example/workload-a` | distinct local contribution |

Identity equality is the tuple
`(ownerProject, kind, subkind-or-empty, key)`. Every component is compared as
exact UTF-8; no case folding, Unicode normalization, path cleaning, aliases,
container identity, or display-name fallback is allowed.

`kind` and the requirement for `subkind` are closed in v2:

| V2 `kind` | `key` | `subkind` |
| --- | --- | --- |
| `config` | configuration path | absent |
| `schema` | schema/module semantic name | required: schema kind |
| `discoverer` | discoverer/registry semantic name | required: discoverer kind |
| `migration` | migration namespace | required: migration kind, for example `sql` |
| `infra` | logical resource name | required: infra kind |
| `health` | contributor name | required: probe kind |
| `lifecycle` | hook name | required: lifecycle phase |
| `package` | canonical package name | absent; resolved version is mutable fact |
| `requiredCapability` | logical capability name | absent |

`ownerProject` and `key` are non-empty semantic identifiers, not generated
UUIDs, source paths, line ranges, registry array indexes, digests, or display
labels. `identity.ownerProject` must exactly equal `provenance.project`.
Contribution-specific fields that also express identity must agree with the
identity block. For example, a migration's namespace must equal
`identity.key`, and its wire-level migration kind must equal
`identity.subkind`. Any mismatch is invalid rather than an alias.

The migration kind is an open, non-empty framework migration kind such as
`sql`; it is not the Capability Manifest's v1 `SourceKind`. Two migration
namespaces with different migration kinds are distinct. Within one manifest,
two entries with the same complete tuple are duplicates even if their payload
or provenance is byte-identical.

Every v2 contribution collection sorts by
`(ownerProject, kind, subkind-or-empty, key)` using bytewise UTF-8 order before
serialization. Collection-specific payload fields are not identity tie-breakers:
a tuple collision is diagnosed before canonical output.

Package versions, content digests, datasource targets, route paths, generated
module paths, and provenance never enter the tuple. They may change at a new
revision without breaking an authored feature association.

### Canonical contribution reference

Feature evidence uses one structured reference; no shorthand string form is a
wire contract:

```json
{
  "contribution": {
    "ownerProject": "go.putnami.dev/example/billing",
    "kind": "migration",
    "subkind": "sql",
    "key": "billing"
  }
}
```

`contribution` is exactly a v2 identity. References never name a containing
manifest: a reference to a dependency resolves identically against the
dependency's own manifest or a verbatim copy transported by any workload.

Resolution searches all discovered manifest containers and groups candidates
by the complete owner-scoped identity. For v2, canonical copy bytes are the
canonical JSON bytes of the entry itself; the containing manifest project/path
is excluded. The rules are:

1. a duplicate identity inside one manifest is invalid;
2. byte-identical copies from different manifests coalesce into one logical
   contribution, with container paths retained only as sorted inspection data;
3. copies with the same identity but different canonical copy bytes are a
   conflict and make lookup ambiguous, with every container reported; and
4. after coalescing, zero logical candidates are unresolved and exactly one
   resolves. Discovery order never chooses a winner.

The capabilities protocol package owns construction, comparison, canonical
sorting, and lookup helpers for this reference. Feature tooling must call those
helpers instead of rebuilding collection-specific keys.

For v1, `ownerProject` derives only from the entry's required
`provenance.project`, never from the containing manifest. The helper may derive
the remaining tuple only where the v1 wire contains every component: config,
schema, discoverer, infra, health, lifecycle, package, and required-capability
entries qualify. Canonical v1 copies use their strict parsed fields plus the
derived identity and follow the same cross-container coalescing/conflict rules.
When v1 and v2 candidates share an identity, the unique v2 candidate is the
logical result only if every v1 candidate equals its **v1 projection**:
contribution-specific v1 fields plus `project`, `package`, `version`, and
`sourceKind`; v2-only identity, migration-kind,
declaration/artifacts, and v1 `evidencePath` are omitted because they have no
faithful cross-version equivalence. Otherwise the group conflicts. This rule is
version- and discovery-order-independent.

A normalized candidate derives its current source binding at index time by
applying the algorithm below to its provenance owner's project root, or exact
package root when provenance identifies an external package. The derived value
is lookup metadata, never written back into any capability document. If the
root is unavailable, the reference may still resolve but cannot provide current
evidence and is reported stale/unavailable. V1 migrations do not qualify because the wire has
a namespace but no migration kind; lookup returns
`capabilities.v1_unreferenceable` and must not guess `sql`, key by namespace
alone, or consult runtime state.

### Index-time source integrity without manifest churn

V2 replaces the overloaded v1 `evidencePath` meaning with explicit producer,
declaration, and artifact concerns:

```json
{
  "provenance": {
    "project": "go.putnami.dev/example/billing",
    "package": "go.putnami.dev/database",
    "version": "1.4.0",
    "sourceKind": "framework",
    "declaration": {
      "root": "project",
      "path": "migrations.go",
      "symbol": "BillingMigrations"
    },
    "artifacts": [
      {
        "root": "project",
        "path": "schema/migrations/billing.json",
        "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
      }
    ]
  }
}
```

- `project`, optional `package`/`version`, and `sourceKind` retain their v1
  producer meanings. `project` is the semantic owner and must equal
  `identity.ownerProject`, including on dependency copies.
- Capability provenance deliberately carries no source binding. The external
  system selecting and indexing a repository revision computes the binding for
  each referenced root and owns freshness/integrity assessment. This keeps the
  durable manifest stable across unrelated source commits.
- `declaration` is required and points to the source declaration or generator
  registration that caused the contribution. A package descriptor is not a
  declaration unless the contribution is literally declared in that file.
- `artifacts` is an optional list of generated or lowered outputs. Generated
  activation modules and committed schema artifacts belong here, never in
  `declaration` merely because they are easy for a producer to find.
- `root` is one of `workspace`, `project`, or `package`. `package` locations
  require `package` and resolved `version`; they resolve within that exact
  package source. Paths are relative to the selected root.
- `symbol`, and optional positive line/column spans when the implementation
  adds them, are inspection hints within the selected index revision. They do not affect
  identity.
- Artifacts sort by `(root, path, digest-or-empty)` and must not duplicate the
  declaration location.

V2 producers must name the most precise declaration they know. If an emitter
cannot identify a declaration, publication fails with a diagnostic; falling
back to `go.mod`, `package.json`, a generated module, or the Capability
Manifest itself would recreate the v1 ambiguity.

#### `source-v1` binding algorithm

The indexing system computes the binding over the declaration's selected
`root`: workspace root; the root of `identity.ownerProject`; or the exact
`package`/`version` source root. In the example, `migrations.go` is therefore
relative to the billing project root.

1. The input set is every Git-tracked file below that root plus every untracked,
   non-ignored worktree file. Working bytes win over index/HEAD bytes; deleted
   files are absent. For an immutable Git tree, the input set is its blobs and
   gitlinks. For an immutable package archive, it is every archive entry.
2. At any depth, exclude `.git/**`, `.gen/**`,
   `schema/capabilities.json`, and `schema/feature-evidence/**`. These are the
   complete v1 exclusions. Other tracked files remain inputs even if a local
   ignore rule would hide them; ignored untracked files are absent.
3. Reject paths that violate this RFC's containment rules. For each remaining
   entry emit `{path, mode, digest}`: canonical root-relative slash path; Git
   mode `100644`, `100755`, `120000`, or `160000`; and `sha256:<lower-hex>` of
   regular-file bytes or symlink-target bytes. A gitlink instead uses
   `git:<full-object-id>` as its digest. Directories have no records.
4. Sort records by bytewise UTF-8 `path`. Serialize
   `{ "bindingVersion": 1, "files": [...] }` with the canonical JSON rules in
   this RFC, prepend the ASCII domain separator
   `putnami-source-binding-v1\n`, hash the resulting bytes with SHA-256, and
   format the value as `source-v1:sha256:<64-lower-hex>`.

The output artifacts are excluded at both their scratch and promoted paths, so
promoting or committing them cannot change the binding they contain. A clean
worktree, the corresponding immutable Git tree, and a historical reader of a
commit that differs only by those outputs must compute identical bindings. A
dirty authored/source change changes the binding immediately; regenerating the
outputs does not. Dependency aggregation copies provenance without persisting a
binding; the index snapshot records the computed value separately.

A root that Git does not manage has no `source-v1` binding. That is a root
with no Git program on `PATH`, or a root outside every Git repository. No
other input set stands in for it: an implementation must not walk the file
system to compute one, because without Git it cannot tell an ignored file from
an input.

- The scheduler stamps each capability package of such a root with an empty
  `sourceBinding`, `sourceBindingUnavailable: true`, and its `sourceRoot`. It
  stamps the marker for no other reason: a binding that fails inside a
  repository stays an error.
- A producer that reads the marker makes no source claim. It emits the same
  Capability Manifest bytes as for a bound root, writes no feature evidence
  document, and removes one a previous build left in its scratch output. The
  marker is all or none across one stamp; a marked package that also carries a
  binding, or a mix of marked and bound packages, is malformed.
- A reader has no evidence document for such a build, so it reports the
  evidence as unavailable with `capabilities.source_binding_unavailable` or
  `features.source_binding_unavailable`. It never treats the absence as a
  current binding.
- A publication or deployment needs the commit it ships, so it refuses such a
  root and names Git.

A repository evaluation revision is separate metadata. A live snapshot records
`{kind: "worktree", head: "git:<full-object-id>" | null}` and per-root source
bindings; `sourceDirty` is true exactly when any root binding differs from the
same root at HEAD. Generated-output-only changes therefore do not make it
source-dirty. Historical snapshot and both sides of `diff` record
`{kind: "git", commit: "git:<full-object-id>"}`. This metadata selects the tree
and that tree's workspace/project membership, but never determines freshness
and is never a required value in promoted output.

Locations are interpreted in the selected evaluation tree. Evidence that
claims a particular source state is current only when its source selector
matches the indexer's recomputed root binding; otherwise it remains stale and a
reader must not pretend current line/symbol coordinates are exact. If a declared
owner project/package root cannot be loaded, readers emit
`capabilities.source_binding_unavailable` rather than falling back to the
container or a machine-global cache.

## 2. Repository protocol

### Authored feature intent

The durable repository document read by feature protocol v1 is
`putnami.features.json`. It is the canonical discovery and wire boundary, not a
mandate to restate technical facts by hand. Feature protocol v1 is independent
of Capability Manifest v2 and begins with this minimum shape:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-features.json",
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [
    {
      "id": "billing/invoice-export",
      "type": "feature",
      "name": "Invoice export",
      "outcome": "Customers can export issued invoices",
      "owner": "billing",
      "target": "wired",
      "relations": [
        { "kind": "dependsOn", "target": "billing/invoicing" }
      ],
      "requirements": [
        {
          "id": "implementation",
          "stage": "coded",
          "evidenceKinds": ["capability"]
        },
        {
          "id": "integration-test",
          "stage": "wired",
          "evidenceKinds": ["artifact"]
        }
      ]
    }
  ]
}
```

Feature IDs are stable, repository-local semantic IDs: lower-case ASCII
segments separated by `/`, with `-` allowed inside a segment. The first segment
must equal `namespace`. IDs do not derive from filenames, project paths, names,
or owners and survive moves and display-name changes. A workspace aggregation
must contain exactly one declaration per ID.

`type` is `feature` or `journey`. Initial relation kinds are the closed set
`parent`, `dependsOn`, and `includes`; relations are unordered. `includes` may
originate only from a journey. Relation targets must resolve in the same
workspace aggregation, self-relations and duplicates are invalid, and cycles
in `parent` or `dependsOn` are invalid. Ordered journey steps are outside v1.

`target` uses the closed maturity ladder below. A requirement ID is stable
within its feature; its full identity is `(feature id, requirement id)`. Every
stage from `coded` through `target` must declare at least one requirement.
Requirements at later stages may cite earlier evidence as an additional input,
but the resolver never infers a requirement from prose or a contribution name.

The manifest declares intent and a target. It never authors `current`, health,
activation, rollout, experiment, or completion-percentage fields.

Keep the manifest as a minimal product-intent overlay. Capability identities,
routes, configuration, migrations, schemas, infrastructure, source bindings,
and artifact digests remain owned by code and producer outputs. The v1
framework integrations do not currently generate this manifest; a future
code-first declaration adapter must emit these same canonical bytes rather than
introducing a second feature identity.

### Evidence artifacts

Evidence is not embedded in `putnami.features.json`. Each discovery root may
contain zero or more strict evidence documents under:

```text
schema/feature-evidence/*.json
```

Build producers write the corresponding scratch files under
`.gen/schema/feature-evidence/*.json`; the existing schema promotion convention
makes them committed artifacts. Generated evidence remains owned by its
producer and must not be hand-edited after promotion. Authored attestations use
the committed path directly. Filenames identify transport fragments only and
never enter evidence or feature identity.

An evidence document carries `protocolVersion: 1`, issuer metadata, and an
`evidence` array. Each record has:

- a stable `id`, unique across the workspace aggregation;
- the target `feature` ID and `requirement` ID;
- the exact `stage`, which must match that requirement;
- `outcome: supports | contradicts`;
- `issuer: { kind, id }`, where kind is one of `framework`, `build`, `test`,
  `delivery`, `runtime`, or `human`;
- a source selector: `{ root, ownerProject?, package?, version?, binding }`,
  with the same root-selection rules as capability provenance, plus optional
  environment;
- one typed subject: `capability` (the canonical reference above), `artifact`
  (contained path plus SHA-256 digest), or `attestation` (a bounded claim code,
  not arbitrary payload);
- provenance sufficient to inspect the evidence producer; and
- an observation timestamp only when the subject is intrinsically temporal.

Capability evidence references the technical fact; it does not copy route,
config, migration, infra, or provenance fields. A non-persistent record is
current only when `source.binding` equals the resolver's recomputed binding for
the selected root. Capability support additionally requires the resolved
contribution to carry that same owner binding; artifact support additionally
requires its content digest to match. A mismatch is `stale`, not unresolved.
Human attestations may declare persistent validity, but only human issuers may
do so and a later active contradiction still wins. An optional
`observedRepositoryRevision` may record a commit known when evidence is created;
it is provenance only, is omitted by pre-commit generated evidence, and never
participates in freshness.

Evidence records contain assertions and locations, never config values,
credentials, tokens, tenant/customer payloads, request bodies, log bodies, or
source excerpts. An LLM-produced association has no special authority: it is
ordinary evidence only after a named issuer accepts and persists it.

A human attestation is not a fallback for technical evidence that a framework,
build, or test producer can emit. Repeating the same broad artifact or
attestation across several features or maturity stages does not create
independent proof; each requirement must make a meaningful, reviewable claim.

### Discovery

The loader obtains project roots from the workspace protocol; it never performs
a recursive repository scan.

1. Read `<workspace>/putnami.features.json` when present.
2. For each exact workspace project root, read
   `<project>/putnami.features.json` when present.
3. At those same roots only, read the lexically sorted direct children matching
   `schema/feature-evidence/*.json`.
4. Load each project's committed `schema/capabilities.json` through the
   capabilities v1/v2 reader when references or unclassified-fact reporting
   require it.

The workspace-root manifest owns cross-project journeys and may own workspace
features. Project-root manifests own local features. Absence is valid. A file
under fixtures, samples, nested source directories, dependency caches, `.gen`,
or an undeclared project is invisible unless that directory is itself an exact
workspace/project root.

All discovered paths are converted to canonical workspace-relative slash
paths before sorting or diagnostics. Duplicate roots, nested projects, and the
same file reached through more than one root are de-duplicated by canonical
path, then validated for ownership; they are never parsed twice.

Capability resolution is owner-based, not container-based. A workload's copied
dependency entry and the dependency project's original entry feed the same
logical candidate group. A feature owned by either project therefore uses the
same reference shape and result; the resolver never prefers the nearest
manifest or the manifest whose `project` matches the feature owner.

## 3. Maturity and evidence resolution

The closed maturity order is:

```text
modeled < coded < wired < default-on < live-verified
        < design-partner-proven < ga
```

`modeled` is earned by the valid authored declaration itself. Every later stage
is earned only when all requirements at that stage have at least one active
supporting record, all earlier stages are earned, and no active contradiction
targets the stage or its requirements. The current assessment is the highest
contiguous earned stage, capped at the human-authored target. Evidence above the
target is shown as unclaimed and does not raise current maturity.

For every requirement the resolver reports one verification state:

| State | Rule | Effect |
| --- | --- | --- |
| `verified` | at least one active support and no active contradiction | satisfies the requirement |
| `missing` | no supporting record exists | does not satisfy |
| `stale` | support exists, but every support fails source-binding and/or digest validity | does not satisfy; stale records remain inspectable |
| `contradicted` | at least one active contradiction exists | does not satisfy and wins over active supports |

Conflicting active supports do not cancel a contradiction. The resolver never
chooses evidence by discovery order, issuer priority, timestamp, or majority.
A human must supersede or remove the contradiction in a later reviewable
artifact.

`design-partner-proven` and `ga` always require at least one active human-issued
support at that exact stage, even if technical requirements also pass. Any
attestation classified as a product promise (adoption, support, availability,
or customer proof) likewise requires a human issuer. Framework, build, test,
delivery, runtime, and LLM-derived records cannot make those declarations.

Broken or ambiguous references are structural errors, not `missing` evidence.
They block validation and snapshot publication because treating a typo as an
honest absence would hide the association that needs repair. A valid technical
contribution with no feature evidence is emitted separately as `unclassified`;
the resolver never invents a feature or relation for it.

Snapshot/delta evaluation receives an explicit repository evaluation revision,
recomputes `source-v1` bindings from that tree/worktree, and never reads the
wall clock. The repository revision selects inputs; source-binding/digest and
persistent-human validity decide freshness. Temporal observations may be
displayed only. A future time-expiry policy requires a new protocol decision
with an explicit `asOf` input.

## 4. Diagnostics

Diagnostics use `go.putnami.dev/protocol/diagnostic`, include a canonical
workspace-relative file/field location when available, and sort by severity,
code, path, field, then subject identity. Messages may improve; codes and
severity are the automation surface.

Capability v2/reference resolution adds these diagnostic families:

- `capabilities.missing_contribution_identity`
- `capabilities.invalid_contribution_identity`
- `capabilities.missing_owner_project`
- `capabilities.owner_project_mismatch`
- `capabilities.identity_mismatch`
- `capabilities.duplicate_contribution`
- `capabilities.conflicting_contribution_copy`
- `capabilities.missing_migration_kind`
- `capabilities.missing_declaration`
- `capabilities.invalid_source_binding`
- `capabilities.source_binding_mismatch`
- `capabilities.source_binding_unavailable`
- `capabilities.invalid_path`
- `capabilities.path_escape`
- `capabilities.unresolved_reference`
- `capabilities.ambiguous_reference`
- `capabilities.v1_unreferenceable`

The feature protocol/engine reserves these families:

- parse/wire errors: `features.parse_error`, `features.unknown_field`,
  `features.invalid_protocol_version`;
- identity/model errors: `features.invalid_id`, `features.duplicate_feature`,
  `features.invalid_type`, `features.invalid_stage`,
  `features.invalid_relation`, `features.dangling_relation`,
  `features.relation_cycle`, `features.invalid_requirement`;
- evidence errors: `features.duplicate_evidence`,
  `features.unknown_feature`, `features.unknown_requirement`,
  `features.stage_mismatch`, `features.invalid_subject`,
  `features.invalid_authority`, `features.invalid_source_binding`,
  `features.source_binding_unavailable`;
- discovery/security errors: `features.invalid_path`,
  `features.path_escape`, `features.symlink_escape`,
  `features.outside_discovery_root`, `features.sensitive_content`;
- assessment warnings: `features.missing_evidence`,
  `features.stale_evidence`, `features.contradicted_evidence`,
  `features.missing_human_authority`,
  `features.unclassified_contribution`,
  `features.unresolved_feature_authority`.

Parse, model, reference, authority-shape, and containment findings are errors
and block publication. Assessment findings are warnings and remain present in
successful inspect/snapshot output: incompleteness is a product fact, not a
malformed document. Commands return one diagnostic per independent subject;
they do not stop at the first file or collapse candidates from an ambiguous
reference.

Missing/malformed owner or binding fields, owner/provenance mismatch,
intra-manifest duplicates, conflicting copies, and unresolved/ambiguous
references are errors. A syntactically valid stamped binding that differs from
the recomputed root, or a root unavailable to the reader, is a warning that
makes dependent evidence `stale`; it does not rewrite the identity or fall back
to a container root.

## 5. Determinism, containment, and redaction

### Canonical order and bytes

- Discover roots and files in bytewise UTF-8 order after path normalization.
- Sort features by ID; relations by `(kind, target)`; requirements by maturity
  rank then ID; evidence by `(feature, stage rank, requirement, id)`;
  references by `(ownerProject, kind, subkind-or-empty, key)`; manifest-copy
  candidates by canonical container path; and diagnostics by the tuple defined
  above.
- Never rely on map iteration, filesystem enumeration, extension order, cache
  temperature, locale, or concurrent completion order.
- Canonical JSON uses declared field order, two-space indentation, v1-style
  omission of empty optional fields, UTF-8-compatible escaping, and one
  trailing newline.
- Durable source artifacts contain no generation timestamp or required Git
  commit. Derived snapshots contain their evaluation revision and recomputed
  source bindings, never a CLI version, machine hostname, temp directory, or
  absolute path.
- Repeated aggregation of the same logical inputs must be byte-identical with
  cold/warm caches and shuffled readers.

### Path rules

Every protocol path is a non-empty, slash-separated, relative path. It must be
its own lexical clean form and may not contain `.`, `..`, an empty segment, a
backslash, NUL, a URL scheme, a drive prefix, or a leading slash. Readers join
it to its declared root, resolve symlinks for files they open, and verify the
result remains inside that root. Lexical containment alone is insufficient.

Package-root locations resolve only through the exact package/version already
named by provenance; tooling does not search global package caches by guess.
An unavailable package declaration remains an inspectable unresolved location,
not an excuse to expose a machine-local cache path.

### Redaction

Human and structured output includes semantic identities, bounded metadata,
relative paths, digests, and diagnostic codes. It excludes config values,
environment values, secrets, credentials, tokens, customer identifiers,
tenant/runtime payloads, log bodies, source contents, home directories, and
temporary/cache locations. Sensitive config fields may be named and marked
`sensitive`; their values are never collected. A producer that cannot separate
bounded metadata from uncontrolled content must omit the content or fail
publication with `features.sensitive_content`.

## 6. Local CLI surface

The accepted namespace and command names are:

```text
putnami features validate
putnami features inspect <feature-id>
putnami features snapshot
putnami features diff <base-revision> <head-revision>
```

- `validate` discovers all source artifacts, strict-parses and semantically
  validates them, resolves every contribution reference, and emits all sorted
  diagnostics. Assessment warnings do not make the command fail; structural
  errors do.
- `inspect` performs the same validation, then shows authored outcome/owner,
  target and derived current maturity, every requirement state, missing/stale/
  contradicted evidence, exact semantic-owner contribution identities,
  indexed declarations/artifacts, their transport containers, and related
  unclassified facts. Unknown or ambiguous feature IDs fail with candidate
  diagnostics.
- `snapshot` derives the complete current-workspace view. Human output is a
  concise summary; `--output=json` wraps the data in the stable CLI ResultV2
  envelope. The `data` payload carries `compatibility: "provisional"` and is
  not a public long-term schema.
- `diff` resolves both arguments to immutable repository revisions, reads each
  revision's own workspace/project membership and artifacts without checkout
  or worktree mutation, and reports added, removed, promoted, regressed, stale,
  contradicted, and newly unclassified features. It never mixes either side
  with the current filesystem. Its ResultV2 `data` payload is also provisional.

The human renderers are not machine protocols. Automation uses ResultV2 plus
typed diagnostics. Snapshot and diff output must visibly say `provisional`;
their payload fields or delta categories may change before a separate RFC
declares compatibility. This does not make the durable feature,
evidence, Capability v2, or diagnostic contracts provisional.

## 7. Runtime-inert producer seam

Go, TypeScript, and future Python helpers may let a build-time contributor
associate feature and requirement IDs with a canonical contribution reference.
That metadata is read only by describe/generate/evidence publication paths.

This is the preferred authoring seam for capability-backed technical evidence:
the association stays beside the code that declares the contribution, while
the strict evidence JSON is derived. The seam attaches evidence to an existing
feature and requirement; it does not declare product intent or remove the v1
manifest discovery requirement.

Application configure/start/stop, dependency injection, route and loader
selection, config registration, migration execution, authorization,
entitlements, and deployment promotion must produce the same behavior when
feature metadata is absent, present, malformed, or reordered. Malformed
metadata fails evidence publication; it does not fail or modify application
runtime startup. TypeScript's bundled activation path must prove identical
loader keys and module ordering before and after its v2 migration.

## 8. Compatibility and migration order

1. Land this RFC without protocol or runtime changes.
2. Add Capability Manifest v2 types, schema, strict parser/validator, canonical
   reference helpers, fixtures, and Go/TypeScript byte-parity vectors. Keep the
   v1 implementation and fixtures unchanged. Update every manifest reader to
   accept both versions before any producer changes its default.
3. Implement the feature protocol and conformance corpus. Keep snapshot and
   delta out of the durable protocol package.
4. Switch the Go producer to v2 and emit optional build-time feature evidence;
   pin runtime-inertness and dependency-contribution tests.
5. Switch the TypeScript producer and its Go extension reconciler together.
   Byte-compare with Go and prove bundled activation/config behavior unchanged.
6. Add the Python-authored conformance fixture/producer seam without inventing
   a Python runtime lifecycle that does not exist.
7. Add the internal deterministic resolver, then `validate`, `snapshot`, and
   `inspect`; add checkout-free `diff` only after single-revision aggregation is
   correct.
8. In an adopter repository's experiment branch, dual-read and shadow-generate
   first.
   Compare the generated inventory and mind-map views against the existing
   hand-maintained source, including every domain, feature, relation, maturity
   state, and evidence link. Record validation, comprehension, corpus size, and
   maintenance findings before deciding adoption. Switch the renderer and
   delete the old inventory only after a separate GO decision; a NO-GO closes
   the experiment without merging its corpus. This stage performs no cloud
   infrastructure mutation and does not require hosted Intelligence.
9. Retain v1 reads through adopter migrations and at least one published
   compatibility window. Removing v1 reads or freezing snapshot/delta requires
   a separate explicit decision; completing these steps does neither
   automatically.

Rollback during steps 4-7 means returning producers to v1 while retaining
dual-read consumers. A branch-only adopter NO-GO requires no rollback on `main`:
retain the immutable results, close the experiment branch without merge, and leave
the existing renderer/source untouched. There is no lossy v2-to-v1 migration:
v2 migration kind and precise provenance cannot be represented faithfully in
v1.

## 9. Acceptance tests for implementation slices

Later slices are not complete until their relevant subset of these tests is
machine-checked:

1. Existing v1 valid/invalid fixtures and canonical golden remain byte-for-byte
   unchanged and both Go and TypeScript still read them.
2. V2 valid fixtures cover every contribution kind. Same-named migrations with
   different kinds, and same-key contributions with different owner projects,
   resolve independently.
3. A dependency's original entry and verbatim copies in two workload manifests
   coalesce to one owner-scoped result. Changing container/discovery order does
   not affect it; a differing copy reports every sorted container and fails.
   The same reference resolves whether only the original, only a copy, or both
   are present.
4. V1 owner derives from `provenance.project`. Equal v1 copies coalesce; an
   equal v1 projection coexists with v2 independent of order; divergent mixed
   copies conflict. V1 migration references remain unreferenceable and never
   assume SQL.
5. Changing declaration/artifact paths, lines, package versions, digests, or
   source bindings does not change identity. Missing/mismatched owners,
   duplicate tuples, missing migration kind/declaration, and unknown fields
   fail with pinned diagnostics.
6. A shared `source-v1` golden covers byte ordering, modes, executable files,
   symlink-target bytes, gitlinks, ignored/untracked files, deletion, and every
   exclusion. Go and TypeScript compute identical bindings and canonical v2
   bytes across repeated/shuffled runs.
7. Generating, promoting, modifying, or committing only `.gen`/capability/
   evidence outputs leaves the binding unchanged. A clean worktree equals its
   Git tree; an authored dirty change changes the binding; a historical reader
   recomputes the producer's value without reading current-worktree state.
8. Stamped/recomputed binding equality makes fresh evidence active. A mismatch
   or unavailable owner/package root is visible and stale, never silently
   rebound to a container; repository commit identity does not affect
   freshness.
9. Discovery reads only workspace/exact project roots. Nested fixtures,
   undeclared projects, lexical/symlink escapes, shuffled enumeration,
   concurrency, and cold/warm caches produce the specified stable result.
10. Maturity fixtures pin missing, stale, support, contradiction-wins,
    non-contiguous stages, target capping, and human authority for
    design-partner/GA. Orphan technical contributions remain unclassified.
11. Redaction fixtures prove output contains no secret values, absolute/home/
    temp/cache paths, runtime payloads, or source bodies.
12. Go and TypeScript runtime-inertness tests prove metadata cannot alter
    activation, ordering, config, migrations, startup, or authorization.
13. Revision diff independently loads each tree's membership, recomputes each
    root binding, handles deleted/renamed projects, never checks out either
    revision, and reports every delta deterministically.
14. An adopter's shadow output reaches inventory/renderer parity before source
    switch/deletion; the comprehension exercise needs no cloud infrastructure
    or hosted Intelligence access.
