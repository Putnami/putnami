# Configuration

`@putnami/analytics` reads one configuration section, `analytics`. Everything an
operator can change lives there. Two members are code, not configuration, and
are passed to `analytics(options)`.

## Startup fails closed without a secret

**Composing `analytics()` without a server-side key stops the application at
warmup.** The message is:

```
analytics: set analytics.secret (any string >= 32 chars) or session.cookieSecret;
the visitor hash needs a server-side key
```

The visitor id is an HMAC. Without a key there is no key rotation, so there is
no anonymization — only a hash anyone can recompute from an IP address and a
browser family. Refusing to start is the only honest answer.

Set one of:

```yaml
analytics:
  secret: ${ANALYTICS_SECRET}   # preferred
session:
  cookieSecret: ${SESSION_SECRET}   # reused when analytics.secret is absent
```

`mode: 'identified'` adds a second fail-closed check: it needs a `consent(ctx)`
callback and refuses to start without one.

## The `analytics` section

| Key | Type | Default | What it does |
|---|---|---|---|
| `enabled` | boolean | `true` | `false` registers no route and no middleware. The build still declares the tables. |
| `mode` | `cookieless` \| `identified` | `cookieless` | `cookieless` is a daily-rotating server-side hash and needs no banner. `identified` writes a persistent first-party cookie and requires `consent`. |
| `datasource` | string | `analytics` | The datasource the tables live in. Falls back to `default` when the name is not declared anywhere. |
| `schema` | string | — | The schema the tables live in on that datasource. Unset, they follow the schema the datasource already has, else `public`. See [the schema](#the-schema). |
| `secret` | string (sensitive) | — | The HMAC key. Falls back to `session.cookieSecret`; startup fails without either. |
| `serverPageViews` | boolean | `true` | `false` stops recording server-rendered views. The bootstrap is still injected, so the browser still reports. |
| `respectGpc` | boolean | `true` | Treat `Sec-GPC: 1` as an opt-out from the persistent cookie. |
| `respectDnt` | boolean | `true` | Treat `DNT: 1` as an opt-out from the persistent cookie. |
| `countryHeader` | string | — | The trusted edge header carrying a two-letter country (`cf-ipcountry`, `x-country`). Unset means no country is stored. There is no GeoIP lookup. |
| `trustedProxies` | string[] | — | Exact IPs and CIDR ranges allowed to set `X-Forwarded-For`. Empty means the socket peer is always used. |
| `retentionRawDays` | int | `90` | How long a raw event row lives. |
| `retentionAggregateDays` | int | `760` | How long a daily aggregate lives. |
| `retentionMode` | `sweep` \| `pg_cron` \| `off` | `sweep` | `sweep` deletes opportunistically during ingest. `pg_cron` contributes a scheduled job. See `doc/privacy.md`. |
| `maxPathKeysPerDay` | int | `2000` | Above this many distinct `path` counter keys in a day, new ones fold under `__overflow__`. |
| `rateLimitPerMinute` | int | `120` | Per-peer ingest budget. The 121st request in a minute is `429`. |
| `flushIntervalMs` | int | `5000` | How often the write queue is flushed: one request every this many milliseconds is elected to push a batch, and the background ticker runs on the same period. Lower means fresher rows and more requests paying; higher means more drift and fewer. |
| `flushWaitMs` | int | `1000` | The most an elected request waits for the flush it started. A flush that outruns it does not stop the response. |
| `queueCapacity` | int | `5000` | Rows held in memory before the **oldest** are dropped and counted as `analytics.ingest.dropped.overflow`. |
| `flushBatch` | int | `200` | The most rows one `INSERT` carries. |
| `flushDeadlineMs` | int | `5000` | How long `stop()` drains the queue before giving up. Half of the framework's 10 000 ms shutdown timeout, so a database that never answers cannot hang a shutdown. |
| `cookieName` | string | `_pa` | The `identified` cookie name. |
| `cookieMaxAgeDays` | int | `390` | The `identified` cookie lifetime. |

## Writes are asynchronous

**No response ever waits for the database.** A page view, a beacon, and a
server-side `track()` all return as soon as the row is accepted into a bounded
in-memory queue. Three things drain it:

- **One elected request every `flushIntervalMs`.** It starts the flush before
  the handler runs, lets it overlap the render, then waits for it — for at most
  `flushWaitMs`. Every other request pays one timestamp comparison.
- **A background ticker** on the same period, unref-ed so it never keeps a
  process alive. On request-based CPU (Cloud Run and its equivalents) the
  callback is throttled between requests and runs at the next CPU window, which
  is what the platform's keep-warm ping provides.
- **`stop()`**, which drains for up to `flushDeadlineMs`. SIGTERM arrives there,
  so a scale-to-zero instance writes what it was holding.

Two consequences to plan for:

- **Drift.** A row can sit in the queue for up to `flushIntervalMs` plus the gap
  to the next request. Freshness is a configuration choice, not a guarantee.
- **Bounded loss.** A crash, an overflow past `queueCapacity`, or a flush that
  outlives `flushDeadlineMs` loses rows. That is the accepted trade: analytics
  never perturbs serving. Every drop is counted under
  `analytics.ingest.dropped.*`, and `analytics.queue.size` is the depth to
  watch.

`flushAnalytics(deadlineMs = 5000)` writes what the queue is holding, right now:

```ts
import { flushAnalytics } from '@putnami/analytics';

await postSomething();
await flushAnalytics();   // the rows are in the database after this line
```

Use it in a test that asserts on rows it has just produced, or before a
maintenance stop. Never on a response path — that is the coupling the queue
exists to remove. See `doc/adr/0004-writes-are-asynchronous-and-lossy.md`.

## Code-only options

```ts
app.use(
  analytics({
    // Required when mode is 'identified'. Returning false keeps the request cookieless.
    consent: (ctx) => ctx.user?.['analyticsConsent'] === true,
    // Declared action events. The same map declareEvents() takes.
    events: { signup_click: { plan: String }, search: { hits: Number, empty: Boolean } },
  }),
);
```

| Option | Type | What it does |
|---|---|---|
| `consent` | `(ctx) => boolean \| Promise<boolean>` | The consent gate. Required in `identified` mode. A callback that throws is treated as "no consent". |
| `events` | `Record<string, PropsSchema>` | Declared action names and their property types (`String`, `Number`, `Boolean`). |

## Where the tables live

`datasource` names the datasource the four tables are created in and written
to. It defaults to `analytics`, and it **falls back to `default` when that name
is not declared anywhere** — in the `database` config section, in a merged
`database.databases` binding, or in the `DATABASE_BINDINGS` environment
binding. An application with one database therefore needs no configuration at
all: analytics lands beside its data.

Warmup logs the decision once:

```
analytics: datasource "default", mode cookieless
```

Give analytics its own database by declaring the name:

```yaml
database:
  default:
    host: localhost
    port: 5432
    database: app_db
    user: app
    password: ${DB_PASSWORD}
  analytics:
    host: analytics.internal
    port: 5432
    database: analytics_db
    user: analytics
    password: ${ANALYTICS_DB_PASSWORD}
```

The resolution never opens a connection: `database(name)` connects eagerly, so
it cannot be used as a presence check. A misspelled name is a silent fallback
to `default`, which the startup log is there to catch.

**Name it on `sql()` too when analytics is the only reason you composed it.**
`sql()` with no `datasource` makes `default` the workload's primary, and that
is the name the `/healthz` database probe pings. An application whose only
database is the analytics one would then report an unhealthy probe for a pool
it never opens:

```ts
application()
  .use(sql({ datasource: 'analytics' })) // what /healthz pings
  .use(analytics()); // datasource: 'analytics' by default
```

An application with its own tables has the opposite need: leave `sql()` alone
and set `analytics.datasource` to the database it already has, or the build
asks the deployment for a second one it will never use.

### The schema

The migration runner gives each datasource one schema, applied as the
`search_path` of every migration on it, and refuses two sources that put the
same datasource in two schemas. The analytics tables therefore follow the
datasource instead of imposing a schema on it. With `schema` unset, the schema
is, in order:

1. the schema another SQL migration source of the workload declares for the
   same datasource;
2. the schema `sql({ datasource: { name, schema } })` declares, when that
   primary datasource is the analytics one;
3. `public`.

An inherited schema is adopted only when it is a single lowercase name. A
`search_path` list such as `app, public`, or a mixed-case name, leaves the
tables in `public`.

A workload whose datasource declares no schema keeps `public`, and its
migration bundle does not change. The resolution runs when the build collects
the migration sources, after every plugin has contributed, so plugin order
does not matter.

The schema is fixed at the first apply. The runner records applied migrations
by datasource and name, not by schema, so a workload whose resolved schema
changes later finds `001` and `002` already applied and creates no tables in
the new schema. Its writes then fail, and the `analytics.sink.failed` line
names the schema that has no tables. To keep an existing workload where its
tables are, pin the schema before you add a schema to its datasource:

```yaml
analytics:
  schema: public
```

Set `schema` to choose one, for example to keep the four tables apart on a
datasource nothing else uses:

```yaml
analytics:
  datasource: analytics
  schema: analytics
```

The name must be a lowercase SQL identifier (`a`–`z`, `0`–`9`, `_`, at most 63
characters), and neither `pg_*` nor `information_schema`. The migration creates
the tables and, in `pg_cron` mode, the `analytics_expire()` function in that
schema, and every write runs with the schema as its transaction's
`search_path`. Outside `public`, the pg_cron job name ends with the schema, so
two workloads that share a database in two schemas keep one job each.

In `pg_cron` mode the job runs as the migration login, not as the owner role
that created the schema. Outside `public`, that login needs `USAGE` on the
schema, which it inherits when it is a member of the owner role with `INHERIT`,
as a managed database grants it. Without it, `003` fails at apply with
`function analytics_expire() does not exist`.

An explicit schema that contradicts another source on the same datasource fails
`putnami build` with the runner's message, naming both sources:

```
datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")
```

## What composing the plugin adds to the build

Three build artifacts follow from the composition alone, so a deployment needs
no restatement of any of it:

| Artifact | What lands in it | Why |
|---|---|---|
| `infra/requirements.json` | the datasource, as a `postgres` database requirement with the tables' schema | the deployment provisions the database and schema the four tables live in |
| `.gen/migration-bundle/` | the four tables | the platform applies the bundle before a new revision takes traffic, which is why `sql()` needs no `autoApply` |
| `schema/config.json` | the whole `analytics` block, `secret` marked sensitive | an operator sets the key where secrets belong |

Two of those are deliberately **not** conditioned on `enabled`. A build artifact
that changed with the environment the build ran in would make a deployment
depend on who built it: `analytics.enabled: false` stops collection at runtime,
it does not un-declare the infrastructure the composition asked for. The
datasource written into the requirement is the **configured** name, not the
resolved one — resolution falls back to `default` when the name is declared in
no binding, and at build time there is no binding, because this manifest is
what asks for one.

## Composition

```ts
application()
  .use(sql({ autoApply: true }))   // before analytics(): the tables are its migration
  .use(react())
  .use(analytics());
```

- **Without `sql()`** the plugin logs one warning at warmup, uses a counting
  no-op sink, and contributes no migration. The application still serves.
- **Behind a base path** (`module('x').path('/app').use(analytics())`) the
  ingest endpoint moves with the module and the bootstrap names the moved path.
