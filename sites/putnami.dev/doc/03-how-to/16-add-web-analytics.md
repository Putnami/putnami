# Add web analytics

You will measure your own web application — which pages are read, which actions are taken, how forms end — and store the result in your own PostgreSQL. No third party receives it, and the default mode needs no consent banner.

Putnami's analytics plugin is a library, not a service. Composing it is the opt-in; there is no default endpoint and nothing leaves the process.

## Steps

### 1) Add the dependency

```bash
putnami deps add @putnami/analytics
```

Adding the package is what registers its pre-build hook for this project, so the next build starts bundling the browser tracker.

### 2) Compose the plugin after `sql()`

```typescript
import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';

export const events = declareEvents({ counter_click: { value: String } });

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(react()).use(analytics({ events }));
```

The plugin contributes its four tables as a migration source, so it needs `sql()` in the composition. Where it sits relative to `react()` makes no difference.

Without `sql()` it still composes: it logs one warning, counts events in metrics, and stores nothing. That is the shape to use for a site with no database.

### 3) Set the secret

```yaml
# conf/.env.yaml
analytics:
  secret: "${ANALYTICS_SECRET}" # any string of 32 characters or more
```

The visitor identifier is a keyed hash. Without a key there is no key rotation, so there is no anonymization — the application refuses to start rather than pretend otherwise. `session.cookieSecret` is reused when `analytics.secret` is absent.

A container with no `conf/` directory hands the same value in as inline YAML through `CONFIG_DATA`.

### 4) Build once

```bash
putnami build .
```

The browser tracker is a build output, not a runtime file. The build writes `.gen/<publicFolder>/analytics/analytics.<hash>.js.gz` and declares the two analytics routes in `.gen/http-routes.d/analytics.json`. Until it exists the plugin logs

```
analytics: tracker bundle not found; run `putnami build` — collecting server-side page views only
```

and keeps serving: every rendered page still produces a page view.

Pre-rendered pages are covered. A `page().static()` route answers with HTML rendered at build time, which no request touched, so the middleware injects the tracker and that request's page-view id into the response as it goes out. A fully pre-rendered site therefore collects the same sessions, engagement time, and declared actions as a server-rendered one, and no `script-src` change is needed: the injected tag is a same-origin script, not an inline one.

### 5) Declare an action, and track it

Names are declared up front. That is what keeps the payload bounded — the browser can only ever send a name that already exists:

```typescript
import { declareEvents } from '@putnami/analytics';

export const analyticsEvents = declareEvents({
  task_created: { priority: Number },
  project_archived: {},
});
```

From a button, with no handler and no import:

```tsx
<button type='button' data-track='counter_click' data-track-value='1'>
  Clicked
</button>
```

`data-track` is the action name; each `data-track-*` attribute becomes a property whose key has its dashes turned into underscores. Attribute values are always strings, so declare those properties `String`.

From a server handler — `track()` needs the request context, and only the HTTP layer has one:

```typescript
import { track } from '@putnami/analytics';
import { endpoint, type HttpRequestContext, json } from '@putnami/application';
import { Default, Int, useContext } from '@putnami/runtime';

export const POST = endpoint()
  .body({ projectId: String, title: String, priority: Default(Int, 1) })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const task = await ctx.deps.taskService.createTask(body.projectId, body);
    await track(useContext<HttpRequestContext>(), 'task_created', { priority: body.priority });
    return json({ task }, { status: 201 });
  });
```

An `endpoint()` handler narrows its own `ctx`, so the full request context comes from the async context. An undeclared name throws at the call site, not silently at ingest.

Form submissions need no call at all: the outcome of every web `action()` is recorded as a `form_submit` row with `ok`, `validation_error`, or `error`.

Writes are asynchronous: nothing on a response path waits for the database, so a row appears within a few seconds rather than before the response returns — call `await flushAnalytics()` in a test that reads a row back straight after producing it.

### 6) Check the tables

```sql
-- Today's page views per route.
SELECT key AS route, count
FROM analytics_daily_counter
WHERE day = (now() AT TIME ZONE 'UTC')::date AND dimension = 'route'
ORDER BY count DESC;

-- Today's unique visitors.
SELECT count(*) FROM analytics_daily_visitor WHERE day = (now() AT TIME ZONE 'UTC')::date;

-- Today's declared actions.
SELECT action_name, count(*)
FROM analytics_event
WHERE name = 'action' AND day = (now() AT TIME ZONE 'UTC')::date
GROUP BY action_name;
```

`analytics_daily_session` holds one row per visit, with `first_route`, `last_route`, `page_views`, and `engagement_ms`. Bounce, entry, and exit are read from those columns; nothing stores them twice.

### 7) Optional: identified mode, `pg_cron` retention, a separate database

**Identified mode** trades the cookieless default for a persistent first-party cookie. It refuses to start without a consent gate, and it mints the cookie only when that gate returned `true`:

```typescript
app.use(
  analytics({
    mode: 'identified',
    events,
    consent: (ctx) => ctx.user?.['analyticsConsent'] === true,
  }),
);
```

The banner and the record of consent are your application's. `forget(ctx)` erases the cookie.

**`pg_cron` retention** is the difference between a policy and a guarantee. The default `sweep` deletes opportunistically inside an ingest transaction, so an application that stops receiving traffic stops expiring data. `pg_cron` schedules the deletion in the database itself:

```yaml
analytics:
  retentionMode: pg_cron
  retentionRawDays: 90
  retentionAggregateDays: 760
```

The migration fails when `pg_cron` is not admin-installed, on purpose: silently skipping the job would report a retention guarantee nothing enforces.

**A separate database** is a matter of declaring the name. `datasource` defaults to `analytics` and falls back to `default` when that name is declared nowhere, so a single-database application needs no configuration:

```yaml
database:
  default:
    host: localhost
    database: app_db
    user: app
    password: "${DB_PASSWORD}"
  analytics:
    host: analytics.internal
    database: analytics_db
    user: analytics
    password: "${ANALYTICS_DB_PASSWORD}"
```

Warmup logs which one it resolved: `analytics: datasource "analytics", mode cookieless`.

**The schema follows the datasource.** When the application's own migrations, or `sql({ datasource: { name, schema } })`, put that datasource in a named schema, the analytics tables go there too; otherwise they go to `public`. Set `analytics.schema` to choose one. An explicit schema that contradicts another migration source on the same datasource fails `putnami build`, before anything is deployed. The schema is fixed at the first deploy: to keep an existing workload's tables in place, set `analytics.schema: public` before you give its datasource a schema.

### 8) Deploy it

Three things follow from composing the plugin, and the build writes all three:

- `infra/requirements.json` gains the database the tables live in, so the deployment provisions it. It is emitted from the composition, not from the environment the build ran in — `analytics.enabled: false` stops collection at runtime, it does not un-declare the infrastructure.
- `.gen/migration-bundle/` carries the four tables. Keep `sql()` on its default `autoApply: false` in a deployed workspace: the platform applies the bundle in its own migrate job before the new revision takes traffic, and a local run with no database then still starts.
- `schema/config.json` gains the whole `analytics` block, `secret` included and marked sensitive. That is what lets an operator set the key where secrets belong, rather than in a file. The plugin refuses to start without it, so set it before the first deploy.

## What is never collected

No IP address, no raw browser identification string, no query string beyond the five `utm_*` campaign parameters, no page title, no form field, no token, no e-mail. There is no column for any of them, so nothing upstream can store one by accident. The browser is never told who the visitor is.

`Sec-GPC: 1` and `DNT: 1` are honoured by default: either header keeps the request cookieless.

Before you publish an aggregate outside the team that operates the application, apply small-group suppression — a daily cell with a count of one is a single visitor.

You now have first-party audience and product measurement in your own database, with no third party, no default egress, and no consent banner in the default mode. Read [Analytics](/docs/frameworks/typescript/analytics) for the full data model, and the package's privacy notice template for the paragraph to paste into your privacy page.
