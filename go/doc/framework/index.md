# Go

Go is Putnami's backend service surface: small packages, explicit wiring, fast builds, and operational primitives that match the same workspace model as TypeScript.

> Use it for APIs, workers, platform-adjacent services, and infrastructure-facing components where predictable runtime behavior matters more than framework magic.

## Choose your path

| Goal | Start here | Then read |
|------|------------|-----------|
| Create your first Go project | [Getting Started](/docs/frameworks/go/getting-started) | [How To / Guides](/docs/how-to), [Extension phases](/docs/frameworks/go/extension), [Overview](/docs/frameworks/go/overview) |
| Create a service | [HTTP & Middleware](/docs/frameworks/go/http) | [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle), [Platform endpoints](/docs/frameworks/go/platform-endpoints) |
| Structure the application | [Plugins & Lifecycle](/docs/frameworks/go/plugins-and-lifecycle) | [Dependency Injection](/docs/frameworks/go/dependency-injection), [Configuration](/docs/frameworks/go/configuration) |
| Protect boundaries | [Security](/docs/frameworks/go/security) | [Signing key rotation](/docs/frameworks/go/signing-key-rotation), [Validation](/docs/frameworks/go/validation), [Errors](/docs/frameworks/go/errors) |
| Add data and messaging | [Persistence](/docs/frameworks/go/persistence) | [Migrations](/docs/frameworks/go/migrations), [Events](/docs/frameworks/go/events), [Storage](/docs/frameworks/go/storage), [Caching](/docs/frameworks/go/caching) |
| Expose service contracts | [API Endpoints](/docs/frameworks/go/api-endpoints) | [OpenAPI](/docs/frameworks/go/openapi), [gRPC & Connect](/docs/frameworks/go/grpc), [Service Clients](/docs/frameworks/go/service-clients) |
| Operate in production | [Platform endpoints](/docs/frameworks/go/platform-endpoints) | [Logging](/docs/frameworks/go/logging), [Telemetry](/docs/frameworks/go/telemetry), [Testing](/docs/frameworks/go/testing) |

## The service model

Putnami Go services are built around the same architectural shape as the rest of the workspace:

1. **Application lifecycle** starts and stops plugins predictably.
2. **Plugins** own capabilities such as HTTP, health, events, storage, or telemetry.
3. **Dependency injection and config** keep wiring explicit instead of hidden in package globals.
4. **Platform endpoints and telemetry** make the service inspectable from the first deploy.

That gives Go projects a clear path from a small HTTP service to a production backend without changing the repo model.

## Capability map

| Concern | Go docs | What to expect |
|---------|---------|----------------|
| HTTP | [HTTP & Middleware](/docs/frameworks/go/http) | Routing, middleware chains, handlers, responses |
| API contracts | [API Endpoints](/docs/frameworks/go/api-endpoints) | One endpoint declaration, served plus published as OpenAPI, Protobuf, and typed clients |
| Runtime wiring | [Dependency Injection](/docs/frameworks/go/dependency-injection), [Configuration](/docs/frameworks/go/configuration) | Typed construction, scopes, environment-driven config |
| Data | [Persistence](/docs/frameworks/go/persistence), [Migrations](/docs/frameworks/go/migrations), [Storage](/docs/frameworks/go/storage), [Caching](/docs/frameworks/go/caching) | PostgreSQL, deterministic migrations, object storage, local and layered caches |
| Security | [Security](/docs/frameworks/go/security), [Signing key rotation](/docs/frameworks/go/signing-key-rotation) | Fail-closed identity and authorization, durable overlap rotation |
| Communication | [Events](/docs/frameworks/go/events), [gRPC & Connect](/docs/frameworks/go/grpc), [Service Clients](/docs/frameworks/go/service-clients) | Async workflows, RPC, outbound clients |
| Operability | [Logging](/docs/frameworks/go/logging), [Telemetry](/docs/frameworks/go/telemetry), [Platform endpoints](/docs/frameworks/go/platform-endpoints) | Structured logs, traces, health and readiness endpoints |
| Bounded work | [Context timeouts](/docs/frameworks/go/context-timeouts), [Bounded parallel work](/docs/frameworks/go/bounded-parallel-work) | Request deadlines, cancellation, ordered fan-out |
| Domain boundaries | [Domain access contracts](/docs/frameworks/go/domain-access-contracts) | A declared DARC import enforced at runtime: freshness bounds, ordering, one writer, rebuild (experimental) |

## If you are coming from TypeScript

The concepts are intentionally familiar, but the implementation is Go-native.

| TypeScript concern | Go equivalent |
|--------------------|---------------|
| `application().use(...)` | Application lifecycle plus plugins |
| API routes and middleware | [HTTP & Middleware](/docs/frameworks/go/http) |
| DI providers | [Dependency Injection](/docs/frameworks/go/dependency-injection) |
| Typed config | [Configuration](/docs/frameworks/go/configuration) |
| Health and telemetry | [Platform endpoints](/docs/frameworks/go/platform-endpoints), [Telemetry](/docs/frameworks/go/telemetry) |

Next: start with [Getting Started](/docs/frameworks/go/getting-started), use [How To / Guides](/docs/how-to) for concrete tasks, then read [Extension phases](/docs/frameworks/go/extension) when you need to understand build, test, lint, serve, packaging, or cache behavior.
