# Workload Types

Putnami classifies every project into one of four workload types. The workload type determines the runtime lifecycle, the applicable lifecycle phases, and the expected observable behavior.

## Types

### Service

A **service** is a long-running process that serves requests over a network protocol (HTTP, gRPC, WebSocket).

| Property | Value |
|----------|-------|
| Lifecycle | `start → ready → serve → stop` |
| Duration | Indefinite — runs until explicitly stopped |
| Health check | Supported (HTTP endpoint, TCP probe) |
| Graceful shutdown | Required — drains in-flight requests |
| Hot reload | Supported via `serve` phase |
| Examples | HTTP server, gRPC server, WebSocket gateway |

```json
{
  "workload": {
    "type": "service",
    "healthCheck": { "path": "/health", "intervalMs": 5000 },
    "gracefulShutdown": { "timeoutMs": 30000 }
  }
}
```

### Worker

A **worker** is a long-running background processor that consumes work from an external source.

| Property | Value |
|----------|-------|
| Lifecycle | `start → process → stop` |
| Duration | Indefinite — runs until explicitly stopped |
| Health check | Supported (liveness probe) |
| Graceful shutdown | Required — finishes current work item |
| Hot reload | Supported via `serve` phase |
| Examples | Queue consumer, file watcher, stream processor |

```json
{
  "workload": {
    "type": "worker",
    "gracefulShutdown": { "timeoutMs": 60000 }
  }
}
```

### Job

A **job** is a one-shot batch execution that runs to completion and exits.

| Property | Value |
|----------|-------|
| Lifecycle | `start → execute → complete` |
| Duration | Bounded — runs until work is done |
| Health check | Not applicable |
| Graceful shutdown | Optional — may support cancellation |
| Hot reload | Not applicable |
| Examples | Database migration, data import, build task, scheduled batch |

```json
{
  "workload": {
    "type": "job"
  }
}
```

### Command

A **command** is an interactive CLI tool that executes a single operation and exits.

| Property | Value |
|----------|-------|
| Lifecycle | `execute → exit` |
| Duration | Short — typically completes in seconds |
| Health check | Not applicable |
| Graceful shutdown | Not applicable |
| Hot reload | Not applicable |
| Examples | Code generator, formatter, scaffolding tool, CLI utility |

```json
{
  "workload": {
    "type": "command"
  }
}
```

## Workload Type vs. Lifecycle Phases

Not all lifecycle phases apply to all workload types:

| Phase | service | worker | job | command |
|-------|---------|--------|-----|---------|
| `generate` | yes | yes | yes | yes |
| `build` | yes | yes | yes | yes |
| `test` | yes | yes | yes | yes |
| `lint` | yes | yes | yes | yes |
| `format` | yes | yes | yes | yes |
| `serve` | yes | yes | — | — |
| `publish` | yes | yes | yes | yes |
| `package` | yes | yes | yes | yes |

The `serve` phase is only meaningful for long-running workloads (service and worker). For jobs and commands, `serve` is not activated.

## Schema

The formal definition is in [`schemas/workload.json`](../schemas/workload.json).
