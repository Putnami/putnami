# Dependency Injection

Container, scopes, and service composition.

## Features

- Explicit provider registration via `.provide()` on Application
- Singleton services (shared state, query counter)
- Request-scoped services (per-request context, request ID)
- `endpoint().inject()` for typed dependency injection in handlers
- `ContainerContext` + `scope()` for testing
- Service composition (service depending on other services)

## Services

| Service | Scope | Description |
|---------|-------|-------------|
| `DatabaseService` | singleton | Shared connection, tracks query count |
| `RequestContextService` | scoped | Unique per request, provides request ID |
| `UserService` | scoped | Injects both singleton and scoped deps |

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Scope demo showing singleton vs. request behavior |
| GET | `/users` | User list via injected UserService |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

**1. Call the scope demo endpoint:**

```bash
curl http://localhost:3905/
```

Expected response:

```json
{
  "message": "DI Container Demo",
  "scope": {
    "singleton": {
      "description": "DatabaseService is shared across ALL requests",
      "totalQueriesEver": 3
    },
    "request": {
      "description": "RequestContextService is unique to THIS request",
      "requestId": "abc123...",
      "elapsedMs": 1
    }
  },
  "operations": { "..." }
}
```

**2. Call it again and observe the difference:**

```bash
curl http://localhost:3905/
```

Expected result — `totalQueriesEver` increases (singleton state persists), but `requestId` changes (scoped service is fresh per request).

**3. List users via injected service:**

```bash
curl http://localhost:3905/users
```

Expected response — a list of users fetched through the injected `UserService`, which itself depends on `DatabaseService` and `RequestContextService`.

## What to Observe

- **Refresh the page**: `totalQueriesEver` increases (singleton), `requestId` changes (scoped)
- **Within a single request**: all operations share the same `requestId`

## Test

```bash
putnami test .
```

## What this sample proves

`DatabaseService` is a singleton, so its query counter survives across requests;
`RequestContextService` is scoped, so each request gets a fresh instance; and
`UserService` depends on both, which is why its `deps` are declared explicitly
rather than inferred. Requesting the routes twice is the observable difference
between the two lifetimes.

Contract: [`@putnami/runtime`](../../framework/runtime/README.md) —
[dependency-injection specification](../../framework/runtime/specs/dependency-injection.json).
