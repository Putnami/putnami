# Code Generation

`@putnami/client` generates fully-typed API client classes from a provider's committed OpenAPI contract. Consumers get native TypeScript types, auto-configured transport, and embedded spec metadata — with no manual configuration.

## Overview

Generation has two halves. The provider declares itself a client contract with
`clientGenerator()`: the plugin names, in each operation's `x-putnami-client`,
the project and feature that produce it, and the provider commits the contract
as `schema/openapi.json`. A client project then generates itself from that
committed contract with `putnami client-generate`. The emitter passes the
contract through a language-agnostic intermediate representation (IR) and
writes the client's `src` tree. See
[`openapi-ir-mapping.md`](./openapi-ir-mapping.md) for the canonical,
fixture-backed OpenAPI→IR mapping rules.

**When to use it:** whenever a Putnami service exposes an HTTP or gRPC API that other services or frontend clients need to consume.

**What it does NOT handle:** generating clients for external (non-Putnami) services. For those, use the transport classes directly with a hand-written client class.

## Usage

### Adding the Plugin

Add `clientGenerator()` to the provider application after the API spec plugins:

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
  .use(clientGenerator());
```

### Declaring the Client Project

The client is a project of its own. Its `putnami.json` names the provider's
service in `generatedFrom`, and its `package.json` name is the client's npm
package name:

```json
{
  "name": "@myorg/users-client",
  "type": "library",
  "generatedFrom": { "service": "users.api", "language": "ts" }
}
```

### Generating the Client

```bash
putnami client-generate --projects @myorg/users-client
```

The `@putnami/typescript` `build`, `test`, `lint`, `serve` and `package`
commands run `client-generate` first, so building the client, or a project
that depends on it, generates it too.

A first-party client covers unary REST/JSON operations and server streams over
SSE. An operation reachable only over Connect or WebSocket, a WebSocket transport
declared `encoding: "proto"`, a body on `GET` or `HEAD`, and an integer field
without a declared width all fail generation, naming the operation, before any
file is written. Nothing partial is emitted, and nothing is degraded to a shape
the contract did not declare. The low-level `ConnectTransport` and
`WebSocketTransport` described in [Transports](./03-transports.md) are separate
hand-written APIs; they are not what a generated first-party client dispatches to.

The client project holds its authored files and the generated `src` tree:

```
users-client/
├── package.json          → Authored: { name: "@myorg/users-client", ... }
├── putnami.json          → Authored: names the provider in generatedFrom
├── tsconfig.json         → Authored
└── src/                  → Generated, ignored by Git
    ├── index.ts          → Re-exports all client classes and types
    ├── users-client.ts   → Typed UsersClient class
    └── types.ts          → Request/response interfaces
```

`client-generate` replaces the `src` tree whole and never reads, writes or
removes the authored files beside it. Git holds no generated file. A contract
with no operation removes `src`. A contract with no document-level
`x-putnami-client` fails.

The emitter applies the client project's Biome formatting and lint fixes before
it writes the `src` tree. It uses each file's final output path, so path
overrides, inherited configuration and EditorConfig rules apply as they do
during `putnami lint`. Repeated generation and lint therefore preserve the same
bytes with the same contract and tools.

Run `putnami deps install` before generation. The generator resolves Biome from
the client project first, then from its own installed dependency; no formatter
on `PATH` is required. Package lookup only considers installed files. Missing
configuration or a formatter failure stops generation before any generated
client file is replaced. Diagnostics identify the failed phase without
including generated source or formatter output. The normal `putnami lint` gate
remains responsible for reporting lint-rule violations; Biome's stdin writer
applies fixes without reporting every violation.

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

**The marked OpenAPI document is the authoritative contract.** `client-generate`
reads the provider's committed `schema/openapi.json` and nothing else. For a
first-party provider the document carries the `x-putnami-client` contract, and
the transports it declares — including Connect, with the protobuf descriptor
travelling in the same contract — are what the emitted client dispatches on.
`readProtoSpec()` stays available to a custom pipeline that starts from a Proto
document.

| Contract | Transport | Encoding |
|---|---|---|
| Marked OpenAPI | the declared transports, in declared order | JSON, or protobuf on a declared `connect+proto` transport |
| Unmarked OpenAPI | refused: first-party generation requires `x-putnami-client` | — |

## API Reference

### `clientGenerator(): ClientGeneratorPlugin`

Factory function that creates a `ClientGeneratorPlugin` instance. It takes no
options and names no target, output or package: the client project names its
provider in `generatedFrom`. The plugin writes no file. It makes the
`openapi()` document record each operation's `producer`.

### `generateClient(options): GenerateClientResult`

Renders the TypeScript client of a committed contract into a client project.
`client-generate` runs it through the `putnami-client-generate` binary.

| Option | Type | Description |
|--------|------|-------------|
| `contractPath` | `string` | Absolute path of the provider's committed `openapi.json` |
| `outputDir` | `string` | Absolute directory of the client project |
| `packageName` | `string` | npm package name, as the client's `package.json` declares it |
| `formatProjectRoot` | `string` | Directory whose Biome configuration and EditorConfig format the output. Defaults to `outputDir` |

It returns `{ generated, files }`: `generated` is false when the contract
declares no operation.

### `generateTypeScriptClient(specIR, options): GeneratedFile[]`

Low-level generator function. Exposed for custom generation pipelines.

| Parameter | Type | Description |
|-----------|------|-------------|
| `specIR` | `SpecIR` | Language-agnostic spec IR |
| `options` | `TsGeneratorOptions` | Generation options |

**Returns:** Array of `{ path: string; content: string }` objects representing generated files.

This low-level renderer returns unformatted source and does not load a formatter
or write files. `generateClient()` owns formatting at its file-writing
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

The IR is shared, so one committed contract can emit **both** a TypeScript and a
Go client. Each client is its own project that names the provider in
`generatedFrom`; only the `language` differs:

```json
{
  "name": "github.com/myorg/users-client-go",
  "generatedFrom": {
    "service": "users.api",
    "language": "go",
    "packageName": "usersclient",
    "clientName": "UsersClient"
  }
}
```

The Go client's import path is its own `go.mod` module. Its `client-generate`
runs the Go emitter (`go.putnami.dev/api`) against the provider's committed
`schema/openapi.json`, and writes `client.gen.go` at its root. No extension
shells out to the other; the committed contract is the only seam. The
symmetric direction works the same way: a Go provider (`api.Clients(...)`)
commits its contract, and a TypeScript client project names it in
`generatedFrom`.

`omitOperations` in a Go client's `generatedFrom` names operations, by
`operationId`, that the Go client leaves out instead of failing generation —
for an operation the Go emitter cannot represent yet, such as one declaring
typed response headers. Without it such an operation fails Go generation. Each
operation left out is named in the generated Go client's doc comment, and a
name the contract does not declare fails. The TypeScript client keeps every
operation.

## Producer attribution

Each operation's `x-putnami-client` carries a `producer` — `{ project, feature }`
— attributing it to the feature that produced it. Ownership is the module that
owns the `ApiPlugin` the endpoint was registered on — never the client
generator's module — so an endpoint whose owner declares no feature carries no
`producer` and is generated **unattributed** instead of inheriting a
neighbouring feature.

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
- **Dependencies:** The provider requires the `openapi()` plugin and commits its contract as `schema/openapi.json`; the client project requires `generatedFrom`
- **Extension points:** Use `generateTypeScriptClient()` directly with a custom `SpecIR`, or `generateClient()` with a contract file; the Go emitter lives in `go.putnami.dev/api`
