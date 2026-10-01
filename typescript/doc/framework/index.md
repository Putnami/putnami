# TypeScript

TypeScript is Putnami's most complete application surface: React SSR, API routes, forms, auth, data, storage, events, health, telemetry, and build tooling in one workspace-native stack.

> Use it when you want a full-stack app or API where frontend, backend, data access, and runtime behavior share one set of conventions.

## Choose your path

| Goal | Start here | Then read |
|------|------------|-----------|
| Create your first TypeScript project | [Getting Started](/docs/frameworks/typescript/getting-started) | [How To / Guides](/docs/how-to), [Extension phases](/docs/frameworks/typescript/extension), [Overview](/docs/frameworks/typescript/overview) |
| Build a full-stack web app | [Web](/docs/frameworks/typescript/web) | [React routing](/docs/frameworks/typescript/react-routing), [Forms & actions](/docs/frameworks/typescript/forms-and-actions), [Static files](/docs/frameworks/typescript/static-files) |
| Build an API service | [API](/docs/frameworks/typescript/api) | [HTTP & middleware](/docs/frameworks/typescript/http-and-middleware), [Errors & responses](/docs/frameworks/typescript/errors-and-responses) |
| Add users and sessions | [Auth](/docs/frameworks/typescript/auth) | [Sessions](/docs/frameworks/typescript/sessions), [Configuration](/docs/frameworks/typescript/configuration) |
| Store application data | [Persistence](/docs/frameworks/typescript/persistence) | [Document storage](/docs/frameworks/typescript/document), [Storage](/docs/frameworks/typescript/storage) |
| Add async and realtime behavior | [Events](/docs/frameworks/typescript/events) | [WebSockets & streaming](/docs/frameworks/typescript/websockets), [Caching](/docs/frameworks/typescript/caching) |
| Make it production-readable | [Health checks](/docs/frameworks/typescript/health-checks) | [Telemetry](/docs/frameworks/typescript/telemetry), [Logging](/docs/frameworks/typescript/logging), [Platform endpoints](/docs/frameworks/typescript/platform-endpoints) |
| Measure your own audience | [Analytics](/docs/frameworks/typescript/analytics) | [Web](/docs/frameworks/typescript/web), [Persistence](/docs/frameworks/typescript/persistence), [Add web analytics](/docs/how-to/add-web-analytics) |

## The application model

Putnami TypeScript apps are composed from small runtime plugins:

1. **Application** owns lifecycle and startup.
2. **Modules** group related capabilities such as auth, API, or data.
3. **Plugins** register servers, routes, providers, hooks, health checks, and generated assets.

That model lets a small app stay small while still having a clear upgrade path to a larger service:

```ts
application()
  .use(http())
  .use(api())
  .use(react())
  .use(sql());
```

## Capability map

| Capability | Read | Why it matters |
|------------|------|----------------|
| Toolchain | [Extension phases](/docs/frameworks/typescript/extension) | Detection, dependencies, generate, build, test, lint, serve, publish, Docker, and caching |
| Composition | [Overview](/docs/frameworks/typescript/overview), [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle) | How apps are assembled and started |
| Runtime wiring | [Dependency injection](/docs/frameworks/typescript/dependency-injection), [Configuration](/docs/frameworks/typescript/configuration) | Explicit dependencies, typed config, request scopes |
| Contracts | [Schema](/docs/frameworks/typescript/schema), [Proto & gRPC](/docs/frameworks/typescript/proto-grpc), [Smart client](/docs/frameworks/typescript/smart-client) | Typed boundaries between services and clients |
| Operations | [Testing](/docs/frameworks/typescript/testing), [Health checks](/docs/frameworks/typescript/health-checks), [Telemetry](/docs/frameworks/typescript/telemetry) | Confidence before deploy and signals after deploy |
| Audience measurement | [Analytics](/docs/frameworks/typescript/analytics) | Cookieless page views, declared actions, and daily aggregates in your own database |
| Domain boundaries | [Domain access contracts](/docs/frameworks/typescript/domain-access-contracts) | A declared DARC import enforced at runtime: freshness bounds, ordering, one writer, rebuild (experimental) |

## Recommended first hour

1. Start with [Getting Started](/docs/frameworks/typescript/getting-started) to create a web app, API service, or library.
2. Use [How To / Guides](/docs/how-to) for the first concrete task.
3. Read [Extension phases](/docs/frameworks/typescript/extension) when build, test, serve, publish, or Docker behavior matters.
4. Read [Overview](/docs/frameworks/typescript/overview) to understand the package map.
5. Pick [Web](/docs/frameworks/typescript/web) or [API](/docs/frameworks/typescript/api) depending on your first app shape.
6. Add [Configuration](/docs/frameworks/typescript/configuration) and [Dependency injection](/docs/frameworks/typescript/dependency-injection) before the app grows implicit wiring.
