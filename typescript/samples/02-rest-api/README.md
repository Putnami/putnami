# REST API

The complete API story: routing, validation, CRUD, and OpenAPI.

## Features

- Endpoint builder with `.params()`, `.query()`, `.body()`, `.handle()`
- Validation schemas: `Uuid`, `Optional()`, `Default()`, `MaxLength()`, `Min()`, `Int`
- File-based routing with `[id]` dynamic segments
- OpenAPI spec generation via `openapi()` plugin
- Full CRUD on a "tasks" resource (in-memory store)
- Error responses (400 validation, 404 not found)

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/tasks`](http://localhost:3902/tasks) | List tasks (optional `?status=` filter) |
| POST | [`/tasks`](http://localhost:3902/tasks) | Create a task |
| GET | [`/tasks/[id]`](http://localhost:3902/tasks/1) | Get a single task |
| PUT | [`/tasks/[id]`](http://localhost:3902/tasks/1) | Update a task |
| DELETE | [`/tasks/[id]`](http://localhost:3902/tasks/1) | Delete a task |
| GET | [`/healthz`](http://localhost:3902/healthz) | Aggregate health check |
| GET | `/openapi.json` | OpenAPI specification |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

**1. List all tasks (two seeded tasks are included):**

```bash
curl http://localhost:3902/tasks
```

Expected response:

```json
{
  "tasks": [
    { "id": "1", "title": "Learn Putnami", "description": "Read the getting started guide", "status": "in_progress", "priority": 1, "createdAt": "...", "updatedAt": "..." },
    { "id": "2", "title": "Build an API", "description": "Create a REST API with validation", "status": "todo", "priority": 2, "createdAt": "...", "updatedAt": "..." }
  ],
  "total": 2
}
```

**2. Filter tasks by status:**

```bash
curl http://localhost:3902/tasks?status=todo
```

Expected response — only tasks with `"status": "todo"`.

**3. Create a new task:**

```bash
curl -X POST http://localhost:3902/tasks \
  -H "Content-Type: application/json" \
  -d '{"title": "Write tests", "priority": 3}'
```

Expected response (HTTP 201):

```json
{
  "task": {
    "id": "...",
    "title": "Write tests",
    "description": "",
    "status": "todo",
    "priority": 3,
    "createdAt": "...",
    "updatedAt": "..."
  }
}
```

**4. Get a single task:**

```bash
curl http://localhost:3902/tasks/1
```

Expected response — the task with `id: "1"`.

**5. Update a task:**

```bash
curl -X PUT http://localhost:3902/tasks/1 \
  -H "Content-Type: application/json" \
  -d '{"status": "done"}'
```

Expected response — the updated task with `"status": "done"`.

**6. Delete a task:**

```bash
curl -X DELETE http://localhost:3902/tasks/2
```

Expected response (HTTP 200):

```json
{ "deleted": true, "id": "2" }
```

**7. Try invalid input (validation error):**

```bash
curl -X POST http://localhost:3902/tasks \
  -H "Content-Type: application/json" \
  -d '{}'
```

Expected response — HTTP 400 with validation error (title is required).

**8. Browse the OpenAPI spec:**

```bash
curl http://localhost:3902/openapi.json
```

Expected response — full OpenAPI specification describing all endpoints.

## Test

```bash
putnami test .
```

## What this sample proves

File-path routing produces the REST surface without a route table: five files
under `src/api/tasks/` become five operations, `[id]` becomes a path parameter,
and the OpenAPI document is generated from the same declarations rather than
written by hand. The tests drive every operation and the health endpoint through
the composed application, so the contract the document advertises is the
contract the tests exercise.

Contract: [`@putnami/application`](../../framework/application/README.md) —
[application-lifecycle specification](../../framework/application/specs/application-lifecycle.json).
