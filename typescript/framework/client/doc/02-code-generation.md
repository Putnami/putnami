# Code Generation

`@putnami/client` generates fully-typed API client classes from OpenAPI and Proto specs at build time. Consumers get native TypeScript types, auto-configured transport, and embedded spec metadata — with no manual configuration.

## Overview

The generator runs in the `application.postGenerate()` lifecycle — *after* every plugin's `generate()` has resolved, so the OpenAPI spec is fully written before it is read (reading it inside the parallel `generate()` pass would race that write). When you run `putnami build`, it reads the generated spec, passes it through a language-agnostic intermediate representation (IR), and emits a publishable TypeScript client package.

OpenAPI is the authoritative v1 contract. The generator reads the `openapi()` plugin's emitted asset path (`schema/openapi.json` by default, or `.gen/schema/openapi.json` when `output: false`). See [`openapi-ir-mapping.md`](./openapi-ir-mapping.md) for the canonical, fixture-backed OpenAPI→IR mapping rules.

**When to use it:** whenever a Putnami service exposes an HTTP or gRPC API that other services or frontend clients need to consume.

**What it does NOT handle:** generating clients for external (non-Putnami) services. For those, use the transport classes directly with a hand-written client class.

## Usage

### Adding the Plugin

Add `clientGenerator()` to your application after the API spec plugins:

```typescript
import { application, http, api, endpoint, openapi } from '@putnami/application';
import { clientGenerator } from '@putnami/client/generator';

const usersApi = api({
  autoScan: false,
  client: {
    service: { id: 'users.api', audience: 'api://users' },
    credentials: {},
  },
});
usersApi.register(
  '/users/[id]',
  endpoint()
    .params({ id: String })
    .returns({ id: String, name: String })
    .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } })
    .handle((context) => ({ id: context.params.id, name: 'Ada' })),
  'GET',
);

const app = application()
  .use(http({ port: 3000 }))
  .use(usersApi)
  .use(openapi({ title: 'Users API', version: '1.0.0' })) // OpenAPI spec (authoritative)
  .use(clientGenerator({ packageName: '@myorg/users-client' }));
```

### Generating the Client

```bash
putnami clientgen-sync --projects @myorg/users-api
```

A first-party client covers unary REST/JSON operations and server streams over
SSE. An operation reachable only over Connect or WebSocket, a WebSocket transport
declared `encoding: "proto"`, a body on `GET` or `HEAD`, and an integer field
without a declared width all fail generation, naming the operation, before any
file is written. Nothing partial is emitted, and nothing is degraded to a shape
the contract did not declare. The low-level `ConnectTransport` and
`WebSocketTransport` described in [Transports](./03-transports.md) are separate
hand-written APIs; they are not what a generated first-party client dispatches to.

The generator produces a publishable client package at `clients/ts/` (configurable):

```
clients/ts/
├── package.json          → { name: "@myorg/users-client", ... }
├── tsconfig.json
└── src/
    ├── index.ts          → Re-exports all client classes and types
    ├── users-client.ts   → Typed UsersClient class
    └── types.ts          → Request/response interfaces
```

The standalone `generateProjectClients()` path used by `putnami clientgen`
applies the provider's Biome formatting and lint fixes before writing the
generated TypeScript package. It uses each
file's final output path, so path overrides, inherited configuration and
EditorConfig rules apply as they do during `putnami lint`. Repeated generation
and lint therefore preserve the same bytes with the same contract and tools.

Run `putnami deps install` before generation. The generator resolves Biome from
the provider first, then from its own installed dependency; no formatter on
`PATH` is required. Package lookup only considers installed files. It reads
`biome.json` from the provider or its Putnami
workspace. Missing configuration or a formatter failure stops generation before
any generated client file is replaced. Diagnostics identify the failed phase
without including generated source or formatter output.
The normal `putnami lint` gate remains responsible for reporting lint-rule
violations; Biome's stdin writer applies fixes without reporting every violation.

The in-app `clientGenerator()` plugin still writes the renderer's source during
the cached build. Applying this formatting there requires the build cache to
track the full Biome and EditorConfig configuration dependencies first.

### Registering and Calling the Generated Client

The consumer supplies deployment values through framework configuration:

```json
{
  "clients": {
    "clientId": "orders-service",
    "services": {
      "users.api": { "url": "https://users.internal" }
    }
  }
}
```

Register the emitted binding, then resolve and call the typed client through DI:

```typescript
import { application } from '@putnami/application';
import { registerUsersClient, UsersClient } from '@myorg/users-client';

const consumer = application();
registerUsersClient(consumer);
await consumer.start();

const users = consumer.context.get(UsersClient);
const user = await users.getUsers_id({ path: { id: '123' } });
```

The example declares anonymous access explicitly. When the provider declares a
credential profile, the same binding resolves its URL, client identity,
audience and credential source from the `clients` configuration. The client
takes the first declared alternative its binding satisfies, so an operation
that ends its alternatives with an anonymous one (an optional credential,
`.secure({ optional: true })` on the provider) presents the credential when the
binding holds it and calls anonymously when it does not. `ClientBuilder`
and direct transports remain available for tests and external integrations.

## What Gets Generated

### Service Client Class

Each service in the spec generates a class that extends `BaseClient`:

```typescript
// Auto-generated src/users-client.ts
export class UsersClient extends BaseClient {
  readonly serviceName = 'users';
  static readonly specHash = 'a1b2c3d4e5f67890'; // for drift detection

  constructor(config: ClientConfig) {
    super({
      ...config,
      encoding: config.encoding ?? 'proto',
      protoMeta: config.protoMeta ?? { messageMeta: PROTO_META.messageMeta, ... },
    });
  }

  async getUser(params: GetUserParams): Promise<GetUserResponse> {
    return this.request('GET', '/users/{id}', { params: { id: String(params.id) } });
  }

  async listUsers(query?: ListUsersQuery): Promise<ListUsersResponse> {
    return this.request('GET', '/users', { query: { ... } });
  }

  async createUser(body: CreateUserBody): Promise<CreateUserResponse> {
    return this.request('POST', '/users', { body });
  }

  // Streaming methods (proto only)
  watchUser(params: WatchUserParams): StreamObserver<WatchUserResponse> {
    return this.stream('/users.v1.UsersService/WatchUser', { body: params });
  }
}
```

### Types File

All request/response interfaces are collected in `types.ts`:

```typescript
// Auto-generated src/types.ts
export interface GetUserParams { id: string; }
export interface GetUserResponse { id: string; name: string; email: string; }
export interface CreateUserBody { name: string; email: string; }
export interface CreateUserResponse { id: string; name: string; email: string; }
```

### Declared Error Guards

Every provider-declared error produces an operation-specific type and runtime
guard. The guard checks the service, operation, HTTP status, and stable provider
code together, so a `catch (unknown)` narrows to the right generated details
without a cast:

```typescript
import { ItemsClient, isGetItemsIdNotFoundError } from '@myorg/items-client';

try {
  await items.getItems_id({ path: { id: 'missing' } });
} catch (error: unknown) {
  if (isGetItemsIdNotFoundError(error)) {
    console.log(error.code); // exactly "not_found"
    console.log(error.details?.reason); // typed from the provider schema
  }
}
```

The code is the stable wire code both languages write — the dotted snake_case
vocabulary of the Go framework's errors, never the PascalCase name used to author
`.mayThrow('NotFound')`. A first-party endpoint answers with the envelope
`{code, error, message, details?}`, so the declared schema is validated against
`details` alone. A provider declares that schema with `.mayThrowDetails(code,
schema)` in TypeScript and `MayThrowDetails(code, api.Type[T]())` in Go.

The runtime validates the complete remote error body against that schema before
exposing details. It omits details when credential redaction would make the typed
value invalid, and reports malformed declared bodies as the local
`client.response` error. Raw response bodies and credentials are never attached
to generated first-party errors.

### Embedded Proto Metadata

For proto-generated clients, the field metadata is embedded directly in the client file. This enables zero-config binary encoding without any runtime configuration:

```typescript
const PROTO_META = {
  messageMeta: {
    GetUserRequest: [{ name: 'id', number: 1, type: 'string', optional: false, repeated: false }],
    GetUserResponse: [{ name: 'id', number: 1, type: 'string', optional: false, repeated: false }, ...],
  },
  enumTypes: ['UserStatus'],
} as const;
```

## Spec Priority

**The marked OpenAPI document is the authoritative contract.** When an `openapi.json` exists it always wins — a Proto artifact never silently takes priority over it. For a first-party provider the document carries the `x-putnami-client` contract, and the transports it declares — including Connect, with the protobuf descriptor travelling in the same contract — are what the emitted client dispatches on. A bare Proto artifact is read only as a legacy fallback when no OpenAPI spec was produced.

| Spec present | Transport | Encoding |
|---|---|---|
| Marked OpenAPI | the declared transports, in declared order | JSON, or protobuf on a declared `connect+proto` transport |
| Unmarked OpenAPI only | HTTP | JSON |
| Both | the OpenAPI document wins | as declared |
| Proto only (legacy fallback) | Connect | binary proto |

## API Reference

### `clientGenerator(config?): ClientGeneratorPlugin`

Factory function that creates a `ClientGeneratorPlugin` instance.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `config` | `ClientGeneratorConfig` | `{}` | Generator options |

### `ClientGeneratorConfig`

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `targets` | `('ts' \| 'go')[]` | `['ts']` | Code generation targets. `'ts'` emits now; `'go'` is wired up in Phase 2b |
| `output` | `string` | `'clients/ts'` | TS output directory relative to project root |
| `packageName` | `string` | Auto-computed | npm package name for the generated TS client. If omitted, appends `-client` to the service's own package name |
| `go` | `Partial<GoClientGenConfig>` | defaults | Go target options (`output`, `modulePath`, `packageName`, `clientName`, `omitOperations`) consumed by the Go emitter in Phase 2b |

**Auto-computed package name examples:**
- `@myorg/users-api` → `@myorg/users-api-client`
- `users-api` → `users-api-client`

The plugin always writes the resolved generation contract to `.gen/clientgen/config.json` — the single source of truth every per-language emitter reads. See [`openapi-ir-mapping.md`](./openapi-ir-mapping.md#genclientgenconfigjson) for the schema and defaults.

### `generateTypeScriptClient(specIR, options): GeneratedFile[]`

Low-level generator function. Exposed for custom generation pipelines.

| Parameter | Type | Description |
|-----------|------|-------------|
| `specIR` | `SpecIR` | Language-agnostic spec IR |
| `options` | `TsGeneratorOptions` | Generation options |

**Returns:** Array of `{ path: string; content: string }` objects representing generated files.

This low-level renderer returns unformatted source and does not load a formatter
or write files. `generateProjectClients()` owns formatting at its file-writing
boundary.

### `readOpenApiSpec(doc): SpecIR | undefined`

Parses an OpenAPI document into the generator IR. Returns `undefined` if the spec has no paths.

### `readProtoSpec(doc): SpecIR | undefined`

Parses a Proto document (from `@putnami/application`'s proto plugin) into the generator IR.

## Intermediate Representation (IR)

The IR is language-agnostic — the same IR can produce TypeScript, Go, or Python clients. It is exposed for advanced use cases:

```typescript
interface SpecIR {
  transport: 'http' | 'connect';
  packageName?: string;        // Proto package name (Connect only)
  services: ServiceIR[];
  specHash?: string;           // SHA-256 prefix for drift detection
  protoMeta?: {
    messageMeta: Record<string, ProtoFieldMeta[]>;
    enumTypes: string[];
  };
}

interface ServiceIR {
  name: string;                // e.g. 'UsersService'
  className: string;           // e.g. 'UsersClient'
  methods: MethodIR[];
}

interface MethodIR {
  name: string;                // generated TS symbol, e.g. 'getUser' (normalized)
  operationId: string;         // canonical spec ID, never normalized
  httpMethod: string;          // 'GET', 'POST', 'PUT', 'DELETE', 'PATCH'
  path: string;                // URL path, e.g. '/users/{id}'
  params?: FieldIR[];          // Path parameters
  query?: FieldIR[];           // Query parameters
  body?: FieldIR[];            // Request body fields
  response?: FieldIR[];        // Response fields
  streaming?: 'server' | 'client' | 'bidirectional';
}

interface FieldIR {
  name: string;                // camelCase field name
  tsType: string;              // TypeScript type string
  optional: boolean;
  array: boolean;
}
```

## Cross-language clients (TS ↔ Go)

The IR is shared, so one OpenAPI spec can emit **both** a TypeScript and a Go client. Add the `go` target:

```typescript
.use(
  clientGenerator({
    packageName: '@myorg/users-client', // the TypeScript client
    targets: ['ts', 'go'],
    go: { modulePath: 'github.com/myorg/users-client-go' },
  }),
);
```

- The **same-language** client (the TypeScript one here) is emitted in-app during `putnami build`.
- The **cross-language** Go client is emitted by **`putnami clientgen`** — a neutral, scheduler-mediated command that runs the Go emitter (`go.putnami.dev/api`) against the provider's own `.gen/schema/openapi.json`. No extension shells out to the other; the shared spec is the only seam. A provider opts into `clientgen` by listing `/tooling/clientgen-extension` in its `putnami.json` `extensions`; a fresh provider then bootstraps its first client (no committed client required).

Both land under `clients/` (`clients/ts`, `clients/go`), are committed like a `.pb.go`, and a `clientsync`-style test catches drift from the spec. The symmetric direction works too: a Go provider (`api.Clients(...)`) emits its Go client in-app and a TypeScript `clients/ts` via the same `putnami clientgen` command. `targets` defaults to `['ts']`; `go` accepts `{ output, modulePath, packageName, clientName, omitOperations }`.

`go.omitOperations` names operations, by `operationId`, that the Go target leaves out instead of failing the whole provider — for an operation the Go emitter cannot represent yet, such as one declaring typed response headers. Without it such an operation fails Go generation. Each operation left out is named in the generated Go client's doc comment and in `omittedOperations` of its `client.putnami.json`, and a name the contract does not declare fails. The TypeScript client keeps every operation.

## Producer attribution

`.gen/clientgen/config.json` carries a `design.operations` table attributing each
endpoint to the feature that produced it. Ownership is the module that owns the
`ApiPlugin` the endpoint was registered on — never the client generator's module
— so an endpoint whose owner declares no feature is absent from the table and is
generated **unattributed** instead of inheriting a neighbouring feature.

The generated `static readonly design` therefore records `producerProject` and
`producerFeature` per operation, keyed by the canonical `operationId`. That id is
kept separate from the generated method symbol: a canonical
`getV1_Operator_Cli-usage` becomes the method `getV1_Operator_Cli_usage`, and each
method passes the canonical id to `this.request(...)` so `BaseClient` resolves
`featureTrace` (method, path, spec hash, canonical operation, project, feature)
from the invoked operation rather than reverse-matching the URL. Two operation
ids that normalize onto the same symbol abort generation instead of emitting a
class with a duplicated member.

## Boundaries

- **Scope:** Generating typed TypeScript **and Go** clients from the marked Putnami OpenAPI document — unary REST/JSON, raw octets, Connect, SSE and WebSocket streams, in-language and cross-language
- **Out of scope:** Generating clients from external specs (Swagger 2.0, gRPC reflection) — an external contract is inventoried and adapted by hand, never generated from a contract Putnami does not own
- **Dependencies:** Requires the `openapi()` plugin; the client generator runs in `postGenerate()`, after the spec is written
- **Extension points:** Use `generateTypeScriptClient()` / `generateProjectClients()` directly with a custom `SpecIR`; the Go emitter lives in `go.putnami.dev/api`
