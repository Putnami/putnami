# Database

PostgreSQL with the full repository pattern.

## Features

- Table definitions with `Table()`, `Column()`, `Key()`
- Repository pattern: `get()`, `find()`, `save()`, `delete()`, `count()`
- Migrations: versioned schema changes
- Query options: filtering, pagination (`limit`/`offset`), sorting (`orderBy`)
- CRUD API endpoints backed by PostgreSQL
- Endpoint builder with validation
- Unit-of-work + consume-once proof (`test/unit-of-work.test.ts`): a device code
  redeemed exactly once via `consumeOnce`, and an all-or-nothing signing-key
  rotation (`rotateRow` + binding) inside `runInTransaction` / a `UnitOfWork` — the
  TypeScript twin of `go/samples/unit-of-work-proof`. See
  `@putnami/database`'s `doc/transactions.md`.

## Prerequisites

A running PostgreSQL instance. Set connection via environment or config:

```yaml
# conf/.env.local.yaml
database:
  default:
    host: localhost
    port: 5432
    database: sample_db
    user: postgres
    password: postgres
```

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | `/users` | List users (with pagination) |
| POST | `/users` | Create a user |
| GET | `/users/[id]` | Get a user |
| PUT | `/users/[id]` | Update a user |
| DELETE | `/users/[id]` | Delete a user |
| GET | `/healthz` | Aggregate health check |

## Run

```bash
putnami serve .
```

Migrations run automatically on startup, creating the `users` table.

## Try It

Once the server is running with PostgreSQL configured, open a new terminal:

**1. Create a user:**

```bash
curl -X POST http://localhost:3906/users \
  -H "Content-Type: application/json" \
  -d '{"name": "Alice", "email": "alice@example.com", "age": 32}'
```

Expected response (HTTP 201) — the created user with a generated `id`.

**2. List all users with pagination:**

```bash
curl "http://localhost:3906/users?limit=10&offset=0"
```

Expected response:

```json
{
  "users": [
    { "id": "...", "name": "Alice", "email": "alice@example.com", "createdAt": "...", "updatedAt": "..." }
  ],
  "total": 1
}
```

**3. Get a single user (replace the id):**

```bash
curl http://localhost:3906/users/<id>
```

Expected response — the user object.

**4. Update a user:**

```bash
curl -X PUT http://localhost:3906/users/<id> \
  -H "Content-Type: application/json" \
  -d '{"name": "Alice Smith"}'
```

Expected response — the updated user with `"name": "Alice Smith"`.

**5. Delete a user:**

```bash
curl -X DELETE http://localhost:3906/users/<id>
```

Expected response (HTTP 200):

```json
{ "deleted": true, "id": "<id>" }
```

**6. Verify the user is gone:**

```bash
curl http://localhost:3906/users
```

Expected response — empty array, `"total": 0`.

## Test

```bash
putnami test .
```

## What this sample proves

Tables declared in code are the schema: `src/tables/` drives both the migrations
in `src/migrations/` and the repositories the handlers use, and the project's
infra requirements are emitted from those declarations rather than restated.

The tests go past CRUD and into the transaction boundary this sample exists to
show:

- **consume-once** — a device code is redeemed exactly once and the outcome is
  typed, including when many redemptions race for the same code;
- **atomic rotation** — a revoke, its successor key, and the binding commit
  together inside `runInTransaction`, and the whole unit rolls back when the
  second write fails or the predecessor cannot be revoked;
- **request-scoped unit of work** — two repositories enrol in one `UnitOfWork`
  boundary and are finalized once, which is the shape a request handler gets for
  free;
- **conformance** — the shared cross-language transaction corpus runs against
  this sample, so the TypeScript and Go adapters cannot drift.

Contract: [`@putnami/database`](../../framework/database/README.md) —
[relational-persistence specification](../../framework/database/specs/relational-persistence.json)
and [SQL-migrations specification](../../framework/database/specs/sql-migrations.json);
[`@putnami/migration`](../../framework/migration/README.md) —
[migration-orchestration specification](../../framework/migration/specs/migration-orchestration.json).
