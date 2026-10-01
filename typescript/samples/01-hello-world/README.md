# Hello World

The absolute minimum to get a Putnami server running.

## Features

- Single `main.ts` + `serve.ts` entry point
- One `GET /` route returning JSON
- Standard platform health endpoints, including `/healthz`
- File-based routing via `api()`

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | [`/`](http://localhost:3901/) | Welcome message with timestamp |
| GET | [`/healthz`](http://localhost:3901/healthz) | Aggregate health check |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

**1. Call the root endpoint:**

```bash
curl http://localhost:3901/
```

Expected response:

```json
{
  "message": "Hello from Putnami!",
  "timestamp": "2025-01-15T10:30:00.000Z"
}
```

**2. Check the health endpoint:**

```bash
curl http://localhost:3901/healthz
```

Expected response — HTTP 200 with health status.

## Test

```bash
putnami test .
```

## What this sample proves

`application()` composes four plugins and nothing else, and `bootstrapServe`
starts the graph inside a failure guard so a bad configuration is reported as
one structured log line instead of a stack trace. The test starts the same
plugins through `createTestApp` without a network listener, which is what makes
the routes testable in isolation.

Contract: [`@putnami/application`](../../framework/application/README.md) —
[application-lifecycle specification](../../framework/application/specs/application-lifecycle.json).
