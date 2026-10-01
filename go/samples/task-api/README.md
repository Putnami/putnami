# Go Tasks API Example

A task tracker REST API demonstrating the Putnami Go framework modules working
together:

- [`api`](../../framework/api) — one endpoint declaration per operation, dispatched onto the HTTP transport
- [`app`](../../framework/app) — application lifecycle and plugin orchestration
- [`http`](../../framework/http) — routing and middleware (recovery, request ID, logging)
- [`platform`](../../framework/platform) — operational endpoints (`/healthz`, `/livez`, `/readyz`, `/version`)
- [`schema`](../../framework/schema) — struct-tag request validation
- [`parallel`](../../framework/parallel) — bounded fan-out for the batch lookup
- [`events`](../../framework/events) — typed event publishing and subscription
- [`inject`](../../framework/inject) — constructor-based dependency injection
- [`config`](../../framework/config) — environment-aware configuration
- [`logger`](../../framework/logger) — structured logging

The task module declares the product outcome `tasks/manage` once. Its API,
schemas, injected handler, and event surfaces are derived automatically and can
be inspected after a build:

```bash
putnami build --projects go.putnami.dev/examples/task-api
putnami features inspect tasks/manage
```

## Running

```bash
# From the workspace root
putnami serve go.putnami.dev/examples/task-api

# Or directly
cd go/samples/task-api && go run .
```

`putnami serve` binds port 3801 through the `PORT` environment variable.

```bash
curl -s localhost:3801/healthz
curl -s -XPOST localhost:3801/tasks -d '{"title":"write the spec"}'
curl -s localhost:3801/tasks
curl -s -XPOST localhost:3801/tasks/batch -d '{"ids":["task-1","missing"]}'
curl -s -XPUT localhost:3801/tasks/task-1 -d '{"status":"completed"}'
curl -s localhost:3801/version
```

## API

| Method   | Path             | Description                                   |
|----------|------------------|-----------------------------------------------|
| `GET`    | `/tasks`         | List all tasks                                |
| `POST`   | `/tasks`         | Create a task                                 |
| `POST`   | `/tasks/batch`   | Look up many tasks concurrently, capped at 8 workers |
| `GET`    | `/tasks/{id}`    | Get a task by ID                              |
| `PUT`    | `/tasks/{id}`    | Update a task                                 |
| `DELETE` | `/tasks/{id}`    | Delete a task                                 |

Operational endpoints come from the platform plugin: `/livez`, `/healthz`,
`/readyz`, and `/version`. They are excluded from the access log.

## Validation

Request bodies are validated against `validate:` struct tags rather than
hand-rolled checks:

```go
type CreateTaskInput struct {
    Title       string `json:"title"       validate:"required,minlen=1,maxlen=200"`
    Description string `json:"description" validate:"maxlen=2000"`
}
```

A partial update supplies only the fields it changes; `schema` skips constraints
on absent fields, so `UpdateTaskInput` needs no separate patch type. A failure
answers `400` with the failing fields.

## Events

Creating a task publishes `task.created`; a status change to `completed`
publishes `task.completed`. Both are broadcast through the in-memory broker and
logged by the subscribed handlers.

## Configuration

Environment variables (prefix `TASK_`):

| Variable            | Default | Description        |
|---------------------|---------|--------------------|
| `TASK_SERVER_PORT`  | `8080`  | HTTP listen port   |

## Verifying

```bash
putnami lint,test,build --projects go.putnami.dev/examples/task-api
```

`capabilities_conformance_test.go` additionally certifies that the committed
`schema/capabilities.json` is byte-canonical and scoped to this project.
