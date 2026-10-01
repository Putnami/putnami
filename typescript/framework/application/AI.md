# @putnami/application

HTTP application framework with routing, middleware, DI, and plugin lifecycle.

## Application Composition

```ts
import { application } from '@putnami/application';

export const app = () =>
  application()
    .use(http())          // HTTP server
    .use(logger())        // Request logging
    .use(api())           // File-based API routing
    .use(react())         // React SSR (from @putnami/web)
    .use(sql())           // Database (from @putnami/database)
    .provide(MyService)   // Register DI provider
    .use(myModule);       // Mount DI module
```

Lifecycle: `generate()` -> `postGenerate()` -> `warmup()` -> `migrate()` -> `start()` -> `stop()`

## Endpoint Builder

The `endpoint()` builder defines type-safe API route handlers. Chain methods for progressive disclosure — only declare what you need:

```ts
import { endpoint, Uuid, Optional, Int, Email, ArrayOf } from '@putnami/application';

// Simple — just a handler
export default endpoint((ctx) => ({ status: 'ok' }));

// Full builder chain
export default endpoint()
  .params({ id: Uuid })                          // URL params (coerced from strings)
  .query({ page: Optional(Int), q: Optional(String) }) // Query string
  .body({ name: String, email: Email })           // JSON body with validation
  .returns({ id: String, name: String })          // Response schema (OpenAPI)
  .throws(400, 'Validation failed')               // Error docs (OpenAPI)
  .description('Get user by ID')                  // OpenAPI description
  .secure({ roles: ['admin'] })                   // Auth guard
  .inject({ users: UserService })                 // DI injection
  .handle(async (ctx) => {                        // Single ctx — deps live on ctx.deps
    const body = await ctx.body();
    return { id: ctx.params.id, name: body.name };
  });
```

**Builder methods** (all optional, chain in any order before `.handle()`):

| Method | Purpose |
|--------|---------|
| `.params(schema)` | Path parameters — coerced from strings |
| `.query(schema)` | Query string parameters — coerced from strings |
| `.body(schema, options?)` | Request body — JSON (default) or form-encoded with `{ contentType: 'application/x-www-form-urlencoded' }` |
| `.body(Binary({ mediaType?, maxBytes }))` | Raw octet request body — the HTTP body *is* bytes, under a named media type, bounded. `await ctx.body()` returns a `Uint8Array` |
| `.returns(Binary({ mediaType?, maxBytes }))` | Raw octet success payload — return `new HttpResponse(bytes, { status, headers: { 'Content-Type': mediaType } })` |
| `.body(BinaryStream({ maxBytes }))` | Raw bounded HTTP upload — `await ctx.body()` returns a bounded `ReadableStream<Uint8Array>`; require the caller's concrete `Content-Type` |
| `.returns(BinaryStream({ maxBytes }))` | Bounded raw HTTP response — return `new HttpResponse(reader, { headers: { 'Content-Type': concreteType } })`; the request scope remains alive until EOF or cancellation |
| `.returns(schema)` | Primary response type — OpenAPI 200 default + dev-mode validation |
| `.response(status, ...)` | Additional status entries for OpenAPI |
| `.throws(status, desc, schema?)` / `.throws(...specs)` | Error response docs for OpenAPI; spread reusable bundles |
| `.mayThrowWith(code, { retryable })` | Declare one stable framework error code and whether generated clients may retry it |
| `.mayThrowDetails(code, schema)` | Declare one stable framework error code and the schema of its `details` body; generated clients expose it typed on that error |
| `.description(text)` | OpenAPI operation description |
| `.secure(options \| guard)` | Authentication / authorization |
| `.client(policy)` | First-party generated-client policy for this operation — see below |
| `.cors(options)` | CORS configuration |
| `.rateLimit(options)` | Rate limiting |
| `.cache(options)` | Response caching |
| `.inject(tokens)` | DI injection — resolved deps available on `ctx.deps` |
| `.handle(handler)` | **Required, must be last** — the route handler |

### Handler Context

Without `.inject()`: `handle((ctx) => ...)`
With `.inject()`: `handle((deps, ctx) => ...)`

`ctx` provides:
- `ctx.params` — typed path parameters
- `ctx.queryParams()` — typed query parameters
- `ctx.body()` — typed, validated request body (async)
- `ctx.req` — raw HTTP request
- `ctx.json(data, options?)` — JSON response helper
- `ctx.redirect(url, status?)` — redirect helper

### Schema Types

Primitives: `String`, `Number`, `Boolean`

Constrained: `Uuid`, `Email`, `Int`, `Url`, `DateIso`, `Min(n)`, `Max(n)`, `MinLength(n)`, `MaxLength(n)`, `Pattern(regex)`

Combinators: `Optional(T)`, `ArrayOf(T)`, `Default(T, value)`, `OneOf(...values)`, `Desc(text, T)`

Nested objects: `{ address: { street: String, city: String } }`

Shared schemas: `schema({ name: String, email: Email })` + `InferSchema<typeof MySchema>`

### First-party client contract

`api({ client })` declares the service identity and credential profiles a generated client resolves
(`ClientServiceContract`); `endpoint().client(policy)` declares one operation's client security
alternatives, idempotency, and resilience overrides (`ClientOperationPolicy`). Both project into the
OpenAPI document's `x-putnami-client` extension — the versioned, language-neutral contract the
TypeScript and Go client generators read (`protocols/clientcontract`). Generation fails rather than
emit a lossy or ambiguous client: an unsupported schema constraint, a security alternative weaker
than the route's own `.secure()` guard, or an idempotency key header that collides with a
framework-owned header (`Authorization`, `X-Client-Id`, …) all reject at spec-generation time.

`openapi()` documents every `api()` registered on the module it resolves (that module, else the
closest ancestor that uses `api()`): the routes each plugin registered and the routes it scanned,
at build time and at runtime. To publish a contract for a subset of a workload's routes, give that
subset its own module with its own `api({ client })` and `openapi()`. The `api()` plugins of one
document must declare the same client contract; a mix fails generation. Scanned routes are
documented under the plugin's `prefix`, else its module's `.path()`, as they are served.

Each `openapi()` of an application writes its own document. The one that publishes a client
contract takes `schema/openapi.json` (or `.gen/schema/openapi.json` with `output: false`), its
`.gz`, and `/_/openapi.json` — the one document `clientGenerator()`, workspace client discovery and
the capability manifest read; when none does, the first in module-tree registration order takes
them. The others, in module-tree registration order, take slot `n` from 1:
`.gen/schema/openapi-<n>.json` unless `output` names a path, the asset key `schema/openapi-<n>.json`,
and `/_/openapi-<n>.json`. An application whose documents publish two client contracts fails
generation.

An endpoint that answers anonymous and authenticated callers alike declares `.secure({ optional:
true })`: a request that presents no credential is served with `ctx.user` unset, the other
requirements apply to an authenticated caller only, and a request whose non-empty `Authorization`
header resolved no identity still gets 401. With `verify`, the verifier is the only identity source:
a request without a bearer token is anonymous even when another resolver set `ctx.user`. Its client security then lists the credential first and an
anonymous alternative (`{ allOf: [] }`) last; a generated client presents the credential when its
binding holds one and calls anonymously otherwise. An anonymous alternative on an endpoint that
requires authentication, or before a credentialed one, rejects at spec-generation time (ADR 0002 of
`go/framework/security`).

```ts
endpoint()
  .params({ name: String })
  .returns(resolvedSchema)
  .secure({ optional: true, scopes: ['packages:read'] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'user', scopes: ['packages:read'] }] }, { allOf: [] }] },
    idempotency: { kind: 'safe' },
  });
```

`ClientOperationPolicy` also decides how the operation travels:

- `transports` is the operation's preference order. It reorders and narrows the transports the bound
  server actually serves; it cannot add one. A protocol named twice, or one this provider does not
  serve for the route's shape, rejects at spec-generation time with the route named. Every generated
  client dispatches in this order.
- `connectEncodings` is the operation's Connect payload encoding order. Connect is the one transport
  a provider serves in more than one encoding: the contract carries one Connect transport entry per
  encoding and a generated client dispatches the first it can carry, so this is where a provider
  states which encoding travels. Like `transports` it reorders and narrows what the mounted `grpc()`
  plugin serves and never adds an encoding. Absent, the plugin order applies.
- `resume: true` states that this server stream can be continued after a broken socket. It is
  published as `websocket.resume` and is accepted only on a **server** stream declared **safe**:
  continuing is re-reading a position the caller already consumed, which is only harmless there.
  `resilience.stream.reconnect` is the consumer half of the same agreement, and one without the
  other rejects.
- `sseContinuation` states how this operation's SSE transport continues after a broken connection
  (clientcontract ADR 0013). `{ mode: 'cursor', cursor: { outputField, queryParameter } }` declares
  **cursor mode**: every output message carries the provider's opaque position after it in
  `outputField`, a required plain-string property, and a reopened connection sends the position of
  the last message the consumer received in `queryParameter`, a declared plain-string query
  parameter; the provider continues exclusively after it, on any instance. `{ mode: 'best-effort' }`
  declares **best-effort mode**: a reopened connection sends the original query and no position, and
  messages produced while no connection was open may be missing or repeated. Only a **server** stream
  declared **safe** may declare one; both cursor references are checked against the route's own
  schemas at spec-generation time, and every other shape rejects with the route named.
  `resilience.stream.reconnect` is effective when the operation carries `resume` or
  `sseContinuation`, and rejects otherwise. A route that declares one also speaks the negotiated SSE
  wire: a request carrying `X-Putnami-Stream-Wire: putnami.sse.v1` is acknowledged with the same
  header on the response head and reads `event: complete` once the handler returns; a request
  without the marker reads the legacy framing. A cursor is a position, not a credential: every
  reopening runs the route's security chain, and the handler owns the typed refusal of a position it
  never issued. The drain belongs to the HTTP plugin: when the application stops, the HTTP plugin
  aborts its drain signal before it waits on in-flight requests, and every negotiated stream it
  serves — a scanned route folder included — ends without a terminal, so a consumer reads an
  interruption and continues on another instance. The request-context member that carries the
  signal (`__drain`) is internal; a handler has no drain option.
- `external: 'OCI Distribution Specification v1.1'` names the external authority that owns a route
  of an `api({ client })` provider — a standard protocol served beside the Putnami routes. The route
  stays served and published, with `x-putnami-external-contract` and no `x-putnami-client`; it has
  no RPC in the proto document or the published descriptor and no generated client method, and it
  answers errors and streams the way an API without a contract does. Its own parameters, body and
  responses are projected the way a document without a client contract projects them — the standard
  owns those schemas — and never become a shared component; contract components stay first-party.
  `external` stands alone: blank, beside a non-zero policy field, or declared without
  `api({ client })`, it is refused before the route is bound and again at spec-generation time. A
  zero value such as `resume: false` is not a declaration, exactly as in Go. See
  `protocols/clientcontract/doc/adr/0011-an-external-authority-can-own-an-operation.md`.

```ts
endpoint()
  .params({ id: String })
  .returns(Stream(itemRevisionSchema))
  .client({
    transports: ['websocket', 'sse'],
    resume: true,
    resilience: { stream: { reconnect: true } },
  })
  .handle(async (ctx) => {
    // A continuation is told where the consumer left; a fresh stream is 0.
    const from = ctx.resumeFrom ?? 0n;
    ctx.send({ id: ctx.params.id, revision: (from + 1n).toString() });
  });

// A change feed a broken connection continues after the last change the
// consumer received, on whichever instance answers. `cursor` is a required
// string of itemChangeSchema and a declared string query parameter.
endpoint()
  .query({ cursor: Optional(String), until: Number })
  .returns(Stream(itemChangeSchema))
  .mayThrow('NotFound')
  .client({
    transports: ['sse'],
    sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
    resilience: { stream: { reconnect: true } },
  })
  .handle(async (ctx) => {
    const { cursor, until } = ctx.queryParams();
    // Continue exclusively after the position the consumer handed back. A
    // position this provider never issued is the declared NotFound: the feed
    // never restarts from the beginning.
    let after = changes.position(cursor);
    if (after === undefined) throw new NotFoundException('the position is not in the retained change log');
    while (after < until) {
      const change = await changes.next(after, ctx.signal);
      // Drained or abandoned: no terminal, and the consumer continues elsewhere.
      if (!change) return;
      ctx.send(change);
      after = change.revision;
    }
  });
```

### Connect

Connect is the protocol published at <https://connectrpc.com/docs/protocol/>, version 1, and this
provider is held to it by a corpus transcribed from that text
(`test/grpc/connect-conformance/corpus.json`). Facts to rely on:

- An error names one of the **sixteen lower-case codes** — `not_found`, not `NOT_FOUND` — under the
  HTTP status the specification pairs with it, with `Content-Type: application/json`.
- The stable framework code, the REST status and the declared `details` member ride in a
  `putnami.client.v1.FrameworkError` **detail** (binary protobuf, unpadded base64), so a declared
  error narrows to its generated type without overloading `code`. See ADR 0003.
- A validation failure also carries a real `google.rpc.BadRequest`; a detail always has a `value`.
- Streaming uses `application/connect+{json,proto}` — *not* `application/connect+streaming+…`, and
  not the bare unary types. A response stream ends with **exactly one** `EndStreamResponse`; a
  request stream is enveloped too, and must never set the end-stream bit.
- The deadline header is `Connect-Timeout-Ms` (whole milliseconds). `grpc-timeout` is still read,
  because gRPC-Web shares the route.
- Per-message compression on a stream is declared with `Connect-Content-Encoding`.

## File-Based Routing

API routes live in `src/api/` with one file per HTTP method. By default routes mount at the root — the `api` folder name is for file organisation only, not a URL prefix. Use `api({ prefix: '/api' })` or `module().path('/api')` to add a prefix.

```
src/api/
  health/get.ts           -> GET  /health
  users/get.ts            -> GET  /users
  users/post.ts           -> POST /users
  users/[id]/get.ts       -> GET  /users/:id
  users/[id]/put.ts       -> PUT  /users/:id
  items/route.ts          -> Named exports: GET, POST, PUT, DELETE
```

Dynamic segments use `[param]` brackets. Each file exports a default `endpoint()`.

Multi-method routes use `route.ts` with named exports:
```ts
// src/api/items/route.ts
export const GET = endpoint().handle((ctx) => ({ items: [] }));
export const POST = endpoint().body({ name: String }).handle(async (ctx) => { ... });
```

## Dependency Injection

### Registering providers

```ts
application()
  .provide(EmailService)                           // No-arg constructor
  .provide(UserService, { deps: [Database] })      // With dependencies
  .provide(DbUrl, () => process.env.DB_URL)        // Factory
```

### Modules

Group related providers with explicit contracts:

```ts
import { composeModules, module } from '@putnami/application';

const authModule = module('auth')
  .require(Database)                              // Must be provided by parent
  .provide(AuthService, { deps: [Database] })     // Public by default
  .provide(TokenValidator, { visibility: 'private' }); // Module-internal

application()
  .provide(Database)
  .use(authModule);
```

Use `composeModules()` when feature modules in the same domain need to satisfy
each other's `require()` contracts without making those providers app-level:

```ts
const auth = composeModules([authUsers(), authCodes(), authOAuthClients()], {
  name: 'auth.server',
});

application().use(auth);
```

### Injecting in handlers

```ts
endpoint()
  .inject({ users: UserService, auth: AuthService })
  .handle(async (ctx) => {
    const { users, auth } = ctx.deps; // resolved per request
  });
```

### Provider options

| Option | Default | Description |
|--------|---------|-------------|
| `deps` | `[]` | Constructor dependencies |
| `scope` | `'singleton'` | `'singleton'` or `'scoped'` (per-request) |
| `visibility` | `'public'` | `'public'` or `'private'` (module-internal) |
| `tags` | `[]` | Tags for multi-resolution via `list()` |
| `onClose` | — | Cleanup hook on shutdown |
| `lazy` | `false` | Defer instantiation until first access |
| `dynamic` | `false` | Allow runtime refresh via `context.refresh()` |

## Configuration

YAML-based with environment overlays:

```yaml
# conf/.env.local.yaml
putnami:
  port: 3000
database:
  url: postgres://localhost/mydb
```

Define typed config schemas:

```ts
import { Config, Int, Optional } from '@putnami/application';
const AppConfig = Config('app', { port: Int, debug: Optional(Boolean) });
```

Priority: env vars > `.gen/conf/` (generated) > `conf/` (source)

## Plugins

Built-in plugins: `http()`, `api()`, `logger()`, `config()`, `oAuth2()`, `sessions()`

Custom plugins implement: `generate?()`, `warmup?()`, `start?()`, `stop?()`

Plugins that require a complete provider set implement
`RequiredCapabilityContributor`. For example,
`requiredCapabilities: () => [{ name: 'sql', requires: ['datasource',
'migration', 'readiness'] }]` makes generation fail with a stable
`capabilities.missing_required_provider` diagnostic if any provider is absent.
Duplicate/conflicting logical providers fail with the corresponding
`capabilities.duplicate_provider` / `capabilities.conflicting_provider` code.
Every v2 contribution has the canonical identity
`(ownerProject, kind, subkind?, key)`; migration namespaces may therefore be
reused across kinds. Runtime-invalid names, provenance, enums, or empty
`requires` lists also fail structural validation before publication.

By default, `Application.build()` also emits `.gen/schema/capabilities.json` from composed
config, migration/infra, health/readiness, and lifecycle contributions. Plugins
with named lifecycle resources implement `LifecycleContributor` and return
`{ name, phase: 'starter' | 'stopper' }` entries.

The scheduler stamps the workload's transitive project graph into
`.gen/version.json` with resolved package versions, evidence paths,
`sourceRoot`/`sourceBinding`, and optional dependency capability-manifest paths.
The v2 producer validates that metadata, merges strict v2 dependency manifests
only within the stamped workspace root, and emits stable declaration and
artifact provenance without embedding resolved package versions or the volatile
source binding. Historical `provenance.version` and `packageVersions` values are
accepted on input; canonicalization removes their versions and migrates package
identity to the stable `packages` collection. This version-independent provider
keeps `package` requirements satisfiable. Indexers resolve package versions and
compute capability integrity from the selected build/revision; the scheduler
snapshot remains available for generated feature evidence. `CapabilityInventoryContributor`
supplies static route/OpenAPI/proto and discoverer metadata; OpenAPI/proto
entries are retained only when their actual artifacts exist. A complete legacy
stamp without source bindings emits only a protocol-v1 activation manifest and
no feature evidence, preserving bundled loader/config behavior across a rolling
CLI upgrade without inventing v2 provenance.

A stamp whose packages all carry `sourceBindingUnavailable: true` with an empty
`sourceBinding` comes from a workspace root Git does not manage. It means the
build makes no source claim: the producer emits the same v2 manifest as for a
bound stamp, writes no feature evidence, and removes stale scratch evidence.
Partial or malformed binding metadata still fails closed: a marked package that
carries a binding, a mix of marked and bound packages, an unmarked package
without a valid binding, and a package without `sourceRoot`.

### Native feature authoring

Declare a product outcome once with `module(...).feature(...)`, then compose its
API, event, migration, data, DI, and generated-client registrations normally.
The build derives `.gen/design/graph.json` from those native registrations; do
not repeat their identities, source paths, artifacts, or feature associations in
wrapper plugins. The [full-stack sample](../../samples/13-fullstack-app) shows
the complete route/event/data vertical, and the
[capabilities sample](../../samples/14-capabilities/src/main.ts) proves that
Capability Manifest v2 retains precise provenance under the same scope.

A feature normally covers its declaring module and everything below it. When the
real composition is wider, pass a second design-time argument instead of adding a
wrapper module, a second declaration, or a claim on a shared ancestor:

```ts
module('opaque-tokens')
  .feature(
    { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: '…', owner: 'auth' },
    { modules: [googleOauth], sources: ['src/api/auth/token'] },
  );
```

`modules` selects sibling modules composed elsewhere — for example other members
of the same `composeModules()` library — by object identity. The selected module
keeps its own `implementedBy` edge from the feature with `exact` authority and
`association: 'selected'`, because the author named it with no inference step.
`sources` selects project-relative source paths, typically the file routes a
workload root `api()` plugin scans for modules it does not own; the longest
matching selection wins, and the framework still has to resolve which
declarations live there, so those edges are `derived` and carry the matched
source as provenance. Anything unselected stays out: an unselected sibling never
enters the graph, and a contributor outside every feature scope still commits
only the subgraph a selection reaches.

Selection is read at build time from the already-composed module tree. It mounts
nothing and reorders nothing — DI registration, plugin collection, and lifecycle
are untouched. Selecting a module selects its subtree, so a feature may not
select its own declaring module or any ancestor of it: that would pull the
declaring module and every unrelated sibling in with it, which is exactly the
broad ancestor claim this replaces. That, two features selecting the same module,
and a selection on a module that already declares or inherits a feature all fail
the build as contradictory claims; a selection that matches no declaration is
only warned about, because scanned-route discovery is itself best-effort. Association is a TypeScript-only vocabulary: the Go declaration has no second argument, and none of it changes the graph's wire vocabulary.

Configuration definitions, infrastructure requirements, lifecycle phases, and
declared tests are projected without `DesignContributor`. Configuration reuses
`ConfigContributor`; database/events/storage expose the bounded
`InfraContributor`; explicit test registries may expose `TestContributor`; and
generate/configure/migrate/start/stop are observed from existing plugin methods.
Semantic config/infra/test nodes carry no registration provenance: provenance
belongs on the owning `contains` edge. A root-owned infrastructure plugin links
a resource only through its native table/topic/bucket declaration sources; two
sources in one feature fold canonically, while sources in two features produce
two derived edges to the shared resource. No source means no invented owner.

Capability provenance is derived from native registrations and generated
artifacts; there is no parallel metadata writer. When the framework cannot
observe a technical fact through a native
registration, implement the build-only `DesignContributor` escape hatch on the
plugin that owns it. Human attestations remain separate protocol evidence and
must not substitute for facts the build can derive.

### Proving a maturity requirement

To let a technical fact earn maturity, declare the association beside the
feature and let the build emit the evidence:

```typescript
module('cli-usage')
  .feature({
    id: 'telemetry-putnami-dev/cli-usage-receiver',
    name: 'Anonymous CLI-usage telemetry receiver',
    outcome: '…',
    owner: 'telemetry.putnami.dev',
    proves: [
      { requirement: 'expiry-runs-without-the-service', contribution: { kind: 'migration', subkind: 'sql', key: 'cliagg' } },
    ],
  })
```

The requirement must already exist in the project's `putnami.features.json` and
accept `capability` evidence; the producer reads its stage from there. Everything
else on the record — ID, issuer (`build`, `@putnami/application`), source
binding, provenance, and subject — is computed by the build. The contribution
reference carries no owner project, so a workload cannot claim a dependency's
contribution.

An unknown feature or requirement, an unpublished or dependency-owned
contribution, or two proofs that disagree fails the build and publishes neither
the manifest nor the evidence. `proves` is inert: it is read only during
describe and never reaches composition or lifecycle.

A contribution nobody proves stays `unclassified` in `putnami features validate`,
which is the intended visible state — see
[ADR 0003](../../../protocols/features/doc/adr/0003-generated-feature-evidence.md).
Never hand-write an evidence fragment to clear one.

A plugin that composes another plugin privately, instead of mounting it on the
module tree, implements the build-only `DesignDelegate` seam (`designDelegates()`)
so the inner plugin's native contributions are forwarded under the same owning
module. Use it rather than a wrapper that restates those facts.

Dropping the wrapper trades one thing away knowingly: TypeScript has no
equivalent of Go's `runtime.FuncForPC`, so a v2 `declaration` derived from a
plain plugin method records the owning project file with no `path`/`symbol`
pair. Identity, source binding, and artifacts stay exact. Accept the coarser
declaration rather than adding a metadata writer to hand-write a
symbol the build cannot verify — a wrong symbol is worse than an honest file.

The graph is disposable and atomically republished; Capability Manifest v2
remains the durable technical identity/provenance contract. Runtime application
composition and lifecycle code never consult feature metadata.

For maintenance and review, use `putnami features validate` and
`putnami features inspect <feature-id>` instead of loading the full evidence
corpus. See the protocol's
[authoring boundary](../../../protocols/features/README.md#authoring-boundary)
for the split between product intent, technical evidence, and human authority.

Generated route-owning `api-loader`, `react-loader`, and `static-loader`
modules are emitted as `schemas` entries with `kind: 'route'` and
project-relative `path` values. Other generated server loaders are emitted as
`discoverers.kind: 'source'`; both forms identify their generated module in
`provenance.artifacts`. Each `api()` that scans a route folder, each
StaticPlugin without `skipLoading`, and each `events()` that scans a handler
folder exports its own loader: `<family>-loader` for the first of its family in
module-tree registration order, `<family>-<n>-loader` (a `source` discoverer) for
the others, and each plugin resolves only its own key in the packaged binary
(ADR 0006). A StaticPlugin at slot `n` stages its files under
`.gen/public/.static-<n>/`. The TypeScript extension reconciles the merged
exports of every pre-build hook into the manifest, so loaders the producer
never saw — such as `@putnami/web`'s `react-loader` — are included; conflicts
fail closed and the canonical v1/v2 manifest is atomically republished. This hook
declares `hooks.preBuild.order: 100` so it runs after every producer: it imports
the workload entry point, and that entry point is free to import a module
another extension generates into `.gen`. The extension uses only those manifest
entries to generate the packaged serve entrypoint's static module
registrations. The extension first normalizes v1 or v2 into that activation-only
view; feature evidence and declaration metadata cannot select or reorder
loaders, config registration, or startup. Loader imports and
`ConfigContributor` registration run before container construction, and every
manifest config path must correspond to a real registered `ConfigDefinition`;
the flattened manifest fields are not executable validators.

Both hook scripts activate the workload's complete config registry before they
read it, through the shared `bin/_activate-config.ts`: import every generated
`*-loader` (which runs the route handlers' `configToken()` calls), then walk the
plugin tree for `ConfigContributor` blocks. `bin/generate.ts` needs it because
its `.gen/infra/secrets.json` fragment is what `build-generate` reconciles into
the committed `infra/requirements.json` — the only task that ever writes that
file — and `bin/config-extract.ts` needs it because `schema/config.json` must
declare the same blocks. `build()` already imports the loaders itself (the
capability producer does it in `postGenerate`); the `ConfigContributor` walk is
what `bin/generate.ts` used to skip, which published a deployability manifest
short every secret a dependency declares while the config schema listed it, with
nothing failing. Manifests carry canonical names, never values.

The walk is probed, not called outright. `isApplicationLike` accepts any object
with `build`/`use`/`getPlugins`/`start`, so a workload bundling its own
`@putnami/application` can hand the hook a sibling-realm app that predates
`registerContributedConfigs()`. That shape is supported, and the preBuild hook
runs on every build, so an app without the method gets a warning naming the cause
and the remedy rather than a `TypeError` — its own and its loader-registered
config still land.

Non-scheduler callers can use
`app.build({ publishCapabilityManifest: false })` to run generation and import
generated loaders without mutating the scheduler-owned manifest. `createTestApp` uses this mode so test
execution cannot overwrite the extension-reconciled build artifact.

## Testing

```ts
import { createTestApp } from '@putnami/application/testing';

const app = await createTestApp(() =>
  application().provide(UserService).use(api())
);
const res = await app.fetch('/users');
expect(res.status).toBe(200);
```

## Detailed Documentation

See `doc/` folder for comprehensive guides:
- `endpoint-builder.md` — full builder API reference
- `file-based-routing.md` — routing conventions
- `configuration.md` — config system
- `plugins.md` — plugin lifecycle and custom plugins
- `security.md` — auth guards, CORS, rate limiting
- `sessions.md` — session management
- `oauth.md` — OAuth2 integration
- `websockets.md` — WebSocket and SSE streaming
- `testing.md` — test utilities
- `telemetry.md` — observability

## First-Party Service Streams

A stream endpoint registered on an `api({ client: … })` plugin serves the
published `putnami.service.v1` WebSocket wire, not raw JSON. Facts to rely on:

- The upgrade carries **no** credential. Admission is the first client frame
  (`init`): identity, declared credential profiles, ordinary headers,
  propagation context, deadline and budget. The provider rebuilds the request
  from that frame and runs the endpoint's own security chain on it.
- `resilience.stream` declares the budgets: `handshakeTimeoutMs`,
  `idleTimeoutMs`, `heartbeatMs` (**0 by default — no heartbeat**),
  `maxFrameBytes`, `maxBufferedMessages`.
- Exactly one terminal reaches the caller. A server stream's `result` carries no
  payload; a client or bidirectional stream's `result` carries its single
  declared value. A handler throw becomes a typed `error` frame with the stable
  wire code — `not_found`, never `NotFound`.
- Every frame the provider emits is re-read by the published strict parser
  before it is written, so this provider cannot emit bytes a conforming consumer
  must refuse.
- **Resume is declared, not assumed.** An endpoint that declares
  `.client({ resume: true })` mints a single-use, rotating grant on every
  admission and carries it in `ready`. The grant is bound to the operation and
  the client identity that earned it, records the highest sequence this provider
  put on the wire, and is bounded by a time to live, a per-stream continuation
  budget and a per-endpoint capacity (`RESUME_BOUNDS`). Redeeming it runs the
  endpoint's own security chain again, continues the sequence the stream left,
  and gives the handler `ctx.resumeFrom`. A token that is unknown, spent,
  presented by another client, or ahead of what this provider delivered buys
  nothing. Grants live in memory only: a restarted provider refuses the
  continuation rather than letting a consumer receive a second copy.
- Not supported: `proto` payloads, resume on a client or bidirectional stream,
  and a JSON `null` inside an application payload (the published wire refuses
  `null` anywhere in a frame).

`parseWebSocketServiceFrameV1`, `nextWebSocketStateV1` and
`WebSocketConversationV1` are exported: a runtime drives the conversation rather
than restating the phase rules.

## Provider-Owned WebSocket Wires

A route that speaks a wire the provider owns — not the first-party
`putnami.service.v1` conversation — declares it on a bidirectional stream:

```typescript
// Raw octets in binary messages; ctx.messages() yields Uint8Array, ctx.send() takes one.
endpoint().query({ database: String }).body(ByteStream()).returns(ByteStream()).handle(async (ctx) => { ... });

// One JSON value of the declared type per text message, under the provider's token.
endpoint().body(Stream(In)).returns(Stream(Out)).subprotocol('putnami.events.v1').handle(async (ctx) => { ... });
```

The upgrade request is the admission: middleware and params/query validation
run on it and refuse with an ordinary HTTP status. The route speaks exactly its
declared token (or none), the handler ends the stream by returning, and the
dispatcher owns the frame and idle bounds, RFC 6455 heartbeat pings and close
codes. The contract publishes one `websocket` transport with `wire: "provider"`.
The builder throws on a token on a unary or one-way stream, a malformed or
`putnami.service.*` token, octets one way only, and `.client({ resume })`.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[application-lifecycle specification](specs/application-lifecycle.json), the
[server-streams specification](specs/server-streams.json), the
[api-contracts specification](specs/api-contracts.json), the
[build/runtime phase ADR](doc/adr/0001-build-and-runtime-are-disjoint-phases.md),
the
[server-stream admission ADR](doc/adr/0002-server-stream-admission-is-the-response-head.md)
and the
[WebSocket admission ADR](doc/adr/0003-websocket-admission-runs-the-security-chain-on-the-rebuilt-request.md).
Two rules govern every change here: `build()` never opens a runtime resource and
`start()` never generates a file, and a failed `start()` stops only what it
started before re-throwing to its caller. Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
