# Fullstack App — Modular Projects + Tasks

This sample showcases a complex app built as two independent business modules:

- `projects` module
- `tasks` module

Think of each module as a separate package with its own persistence, API, events, stream, and webapp surface.  
The application composes them together without collapsing boundaries.

## What This Sample Demonstrates

- **Persistence per module**
- `projects` table + repository/service
- `tasks` table + repository/service
- **Independent APIs**
- Projects API under `/api/projects`
- Tasks API under `/api/tasks`
- **Typed event contracts + handlers**
- `project.created`, `project.status.changed`
- `task.created`, `task.status.changed`
- Cross-module orchestration via handlers in `src/events/*.on.ts`
- **Module activity streams (SSE)**
- `/api/projects/stream`
- `/api/tasks/stream`
- **Webapp surfaces**
- Projects pages (`/projects`, `/projects/new`, `/projects/[id]`)
- Tasks board (`/tasks`)
- **Progressive request security**
- Every mutating React form includes `<CsrfInput />`, so default action CSRF
  validation works before client JavaScript hydrates
- **First-party analytics**
- Page views, declared actions, and form outcomes in the app's own database

## Architecture

```text
src/
├── main.ts
├── events/                                # cross-module event handlers
│   ├── project-created.seed-task.on.ts
│   └── task-status.sync-project.on.ts
├── shared/
│   └── activity.feed.ts                   # in-memory activity feed + subscriptions
├── projects/                              # "package-like" module
│   ├── index.ts                           # public surface
│   ├── projects.module.ts                 # module wiring (path + api)
│   ├── projects.entity.ts                 # persistence
│   ├── project.service.ts                 # domain logic + publishers
│   ├── projects.topics.ts                 # typed topics
│   ├── web/                               # module-owned web pages
│   │   ├── page.tsx                       # /projects
│   │   ├── new/page.tsx                   # /projects/new
│   │   └── [id]/page.tsx                  # /projects/[id]
│   └── api/
│       ├── get.ts                         # GET  /api/projects
│       ├── post.ts                        # POST /api/projects
│       ├── events/get.ts                  # GET  /api/projects/events
│       ├── stream/get.ts                  # GET  /api/projects/stream
│       └── [id]/
│           ├── get.ts                     # GET  /api/projects/[id]
│           └── put.ts                     # PUT  /api/projects/[id]
├── tasks/                                 # "package-like" module
│   ├── index.ts                           # public surface
│   ├── tasks.module.ts                    # module wiring (path + api)
│   ├── tasks.entity.ts                    # persistence
│   ├── task.service.ts                    # domain logic + publishers
│   ├── tasks.topics.ts                    # typed topics
│   ├── web/                               # module-owned web pages
│   │   └── page.tsx                       # /tasks
│   └── api/
│       ├── get.ts                         # GET  /api/tasks
│       ├── post.ts                        # POST /api/tasks
│       ├── events/get.ts                  # GET  /api/tasks/events
│       ├── stream/get.ts                  # GET  /api/tasks/stream
│       └── [taskId]/
│           ├── get.ts                     # GET  /api/tasks/[taskId]
│           └── put.ts                     # PUT  /api/tasks/[taskId]
└── app/                                   # shared root pages/layout
    ├── layout.tsx
    ├── page.tsx
    ├── error.tsx
    └── not-found.tsx
```

React route generation is configured with multi-root scanning in `putnami.json`,
under `options."@putnami/web:generate".react.scanRoots`:

```json
{
  "options": {
    "@putnami/web:generate": {
      "react": {
        "scanRoots": [
          { "path": "src/app" },
          { "path": "src/projects/web", "routePrefix": "/projects" },
          { "path": "src/tasks/web", "routePrefix": "/tasks" }
        ]
      }
    }
  }
}
```

The generation hook rejects a React-only key one level higher instead of
silently ignoring it. For example,
`options["@putnami/web:generate"].scanRoots` reports the corrected path
`options["@putnami/web:generate"].react.scanRoots`.

## Event-Driven Feature Shipped

Two user-facing behaviors are implemented through module events:

1. **Project creation auto-seeds a kickoff task**
- `ProjectService.createProject()` publishes `project.created`
- `project-created.seed-task.on.ts` consumes it and creates:
- `Kickoff: define success criteria`

2. **Project status auto-syncs from task progress**
- `TaskService.updateTask()` publishes `task.status.changed` (with progress counters)
- `task-status.sync-project.on.ts` updates project status:
- `active` when open tasks remain
- `completed` when all tasks are done

## Analytics

`@putnami/analytics` is composed after `sql()`, so its four tables are created
by the same migrate phase as the application's own:

```ts
application()
  .use(sql({ autoApply: true }))
  .use(migrations)
  .use(analytics({ events: analyticsEvents }))
```

**Two declared events** (`src/analytics.ts`). Declaring up front is what keeps
the payload bounded — the browser can only ever send a name that already
exists:

```ts
export const analyticsEvents = declareEvents({
  task_created: { priority: Number },
  project_archived: {},
});
```

**Where the rows come from:**

| Row | Written by |
|---|---|
| `page_view` | every rendered page, enriched by the browser with the session, the referrer, the `utm_*` parameters, and engagement time |
| `action` `task_created` | `track()` in `src/tasks/api/post.ts`, from the HTTP handler that creates the task |
| `form_submit` | the outcome of the `/tasks` web action — no call needed |

`track()` needs the request context and an `endpoint()` handler narrows its
own, so the handler reads it from the async context:

```ts
await track(useContext<HttpRequestContext>(), 'task_created', { priority: body.priority });
```

**Where the rows land:** `analytics_event`, plus the daily
`analytics_daily_counter`, `analytics_daily_visitor`, and
`analytics_daily_session` folds — in `fullstack_db`, beside the application's
own tables.

```sql
SELECT key AS route, count
FROM analytics_daily_counter
WHERE day = (now() AT TIME ZONE 'UTC')::date AND dimension = 'route'
ORDER BY count DESC;
```

**The datasource.** `analytics.datasource` defaults to `analytics` and falls
back to `default` when no `database.analytics` block exists, which is this
sample's case — warmup logs `analytics: datasource "default", mode cookieless`.
Give analytics its own database by declaring the name:

```yaml
database:
  default:
    host: localhost
    port: 6543
    database: fullstack_db
    user: postgres
    password: postgres
  analytics:
    host: localhost
    port: 6543
    database: analytics_db
    user: postgres
    password: postgres
```

**Retention.** `conf/.env.test.yaml` sets `retentionMode: off` so no sweep runs
inside the test assertions. In production, `sweep` (the default) deletes
opportunistically during ingest and is best effort; `pg_cron` schedules
`analytics_expire()` in the database itself and is the guarantee:

```yaml
analytics:
  retentionMode: pg_cron
  retentionRawDays: 90
  retentionAggregateDays: 760
```

The `pg_cron` migration fails when the extension is not admin-installed, on
purpose.

`conf/.env.yaml` carries `analytics.secret` for every environment: the visitor
hash needs a server-side key and the plugin fails closed without one — under
`putnami qualify --target local` too, which runs the sample production-mode.
The value is a sample-only placeholder; a real workload keeps its key out of
the tree.

## Routes

### Web Pages

| Path | Description |
|------|-------------|
| `/` | Home |
| `/projects` | Projects list |
| `/projects/new` | Create project |
| `/projects/[id]` | Project detail + inline task management |
| `/tasks` | Tasks board (module web surface) |

### Projects API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/projects` | List projects |
| POST | `/api/projects` | Create project |
| GET | `/api/projects/[id]` | Get project |
| PUT | `/api/projects/[id]` | Update project status |
| GET | `/api/projects/events` | Recent project activity |
| GET | `/api/projects/stream` | Live project activity stream |

### Tasks API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/tasks` | List tasks (`?projectId=` optional) |
| POST | `/api/tasks` | Create task |
| GET | `/api/tasks/[taskId]` | Get task |
| PUT | `/api/tasks/[taskId]` | Update task |
| GET | `/api/tasks/events` | Recent task activity |
| GET | `/api/tasks/stream` | Live task activity stream (`?projectId=` optional) |

## Run

```bash
putnami serve .
```

Default port is `3913`.

## Prerequisites

PostgreSQL must be reachable from `conf/.env.local.yaml`:

```yaml
database:
  default:
    host: localhost
    port: 6543
    database: fullstack_db
    user: postgres
    password: postgres
```

## Quick Checks

```bash
# create project (triggers project.created)
curl -X POST http://localhost:3913/api/projects \
  -H "Content-Type: application/json" \
  -d '{"name":"Platform Revamp","description":"Q2 scope"}'

# list tasks for project (includes seeded kickoff task)
curl "http://localhost:3913/api/tasks?projectId=<project-id>"

# stream project activity
curl -N "http://localhost:3913/api/projects/stream?replay=10"

# stream task activity for one project
curl -N "http://localhost:3913/api/tasks/stream?projectId=<project-id>&replay=10"
```

## Test

```bash
putnami test .
```

Database-backed tests require PostgreSQL running at the configured host/port.

`test/analytics.test.ts` is the full analytics loop against a real PostgreSQL:
a server page view and its counters, the browser re-sending the same event id
with its engagement, the **same batch posted twice** with all four tables
byte-identical afterwards, a client-minted view of a second route in the same
session, a tracked API action, a form outcome, and a bot that leaves no row. It
deletes every analytics row in `afterAll` — `fullstack_db` is shared across
runs.

The database-backed API/event suite skips when PostgreSQL is unavailable. The
separate `fullstack web boundary` test never skips: it runs the real tasks loader
and action through a request-scoped DI container with in-memory service doubles,
proving that the web layer resolves services and mutates through the server
handler without opening a database.

## What this sample proves

The projects and tasks pages are discovered from separate scan roots yet share
one React route graph. Their loaders resolve module services from request DI,
their actions perform server-side mutations, and every rendered mutation form
contains the no-JavaScript CSRF token field. Persistence, events, and streaming
remain module-owned boundaries; the web package does not absorb those features.

Web contract: [`@putnami/web`](../../framework/web/README.md) —
[web-application-delivery specification](../../framework/web/specs/web-application-delivery.json).
Relational persistence and event delivery are owned by
[`@putnami/database`](../../framework/database/README.md) and
[`@putnami/events`](../../framework/events/README.md), respectively.
Measurement is owned by
[`@putnami/analytics`](../../framework/analytics/README.md) —
[privacy](../../framework/analytics/doc/privacy.md),
[data model](../../framework/analytics/doc/data-model.md).
