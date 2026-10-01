# Getting started

You will add audience and product measurement to a Putnami web application, and
read the result out of your own database. It takes one dependency, one plugin,
and one configuration key.

## 1. Add the dependency

```bash
putnami deps add @putnami/analytics
```

Adding it to `package.json` is what registers the package's pre-build hook, so
`putnami build` starts bundling the browser tracker for this project.

## 2. Compose the plugin

```ts
import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';

export const events = declareEvents({ counter_click: { value: String } });

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(react()).use(analytics({ events }));
```

Order matters in one direction only: `analytics()` contributes its tables as a
migration source, so it needs `sql()` in the composition. Where it sits
relative to `react()` makes no difference.

Without `sql()` the plugin logs

```
analytics: no sql() plugin — events are counted in metrics only and not stored
```

once at warmup, uses a counting no-op sink, and contributes no migration. That
is a supported shape: it gives you the bootstrap, the tracker, and the ingest
route with no database at all.

## 3. Set the secret

```yaml
# conf/.env.yaml
analytics:
  secret: ${ANALYTICS_SECRET}
```

The visitor id is a keyed hash. Without a key there is no key rotation, so
there is no anonymization — only a hash anyone can recompute. The plugin
therefore refuses to start: set `analytics.secret` (any string of 32 characters
or more) or reuse `session.cookieSecret`.

A container with no `conf/` directory hands the same value in as inline YAML:

```ts
childEnv['CONFIG_DATA'] = 'analytics:\n  secret: sample-only-secret-do-not-reuse-0123456789\n';
```

## 4. Build once

```bash
putnami build .
```

The browser tracker is a build output, not a runtime file. The build writes
`.gen/<publicFolder>/analytics/analytics.<hash>.js.gz` and
`.gen/http-routes.d/analytics.json`. Until it exists the plugin logs

```
analytics: tracker bundle not found; run `putnami build` — collecting server-side page views only
```

and keeps serving: every rendered page still produces a `page_view` row.

Pre-rendered pages get the tracker too. A `page().static()` route answers with
HTML rendered at build time, which no request touched, so the middleware
injects the script tag and this request's page-view id into the response on the
way out. A fully pre-rendered site therefore collects the same sessions,
engagement, and declared actions as a server-rendered one — see
[ADR 0005](adr/0005-pre-rendered-pages-get-the-tracker-at-serve-time.md).

## 5. Track an action from the browser

Declare the name once, server-side, then use it in the markup with no handler
and no import:

```tsx
<button type='button' data-track='counter_click' data-track-value='1'>
  Clicked
</button>
```

`data-track` is the action name; each `data-track-*` attribute becomes a
property whose key has its dashes turned into underscores. Attribute values are
always strings, so declare those properties `String`. From a component, the
hook is the same thing with a call site:

```tsx
import { useTrack } from '@putnami/analytics';

export function SignupButton() {
  const track = useTrack();
  return <button onClick={() => track('signup_click', { plan: 'pro' })}>Sign up</button>;
}
```

## 6. Track an action from the server

`track()` needs the request context, and only the HTTP layer has one:

```ts
import { track } from '@putnami/analytics';
import { endpoint, type HttpRequestContext, json } from '@putnami/application';
import { Default, Int, useContext } from '@putnami/runtime';
import { TaskService } from '../task.service';

export const POST = endpoint()
  .body({ projectId: String, title: String, priority: Default(Int, 1) })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const task = await ctx.deps.taskService.createTask(body.projectId, {
      title: body.title,
      priority: body.priority,
    });
    await track(useContext<HttpRequestContext>(), 'task_created', { priority: body.priority });
    return json({ task }, { status: 201 });
  });
```

An `endpoint()` handler narrows its own `ctx`, so the full request context is
read from the async context — the same call the web action handler makes. In a
plain middleware or route handler, pass `ctx` straight through.

An undeclared name throws, synchronously, at the call site. That is a developer
mistake, not a runtime condition.

## 7. Look at the tables

```sql
-- Today's page views per route.
SELECT key AS route, count
FROM analytics_daily_counter
WHERE day = (now() AT TIME ZONE 'UTC')::date AND dimension = 'route'
ORDER BY count DESC;

-- Today's unique visitors.
SELECT count(*) FROM analytics_daily_visitor WHERE day = (now() AT TIME ZONE 'UTC')::date;

-- Today's declared actions.
SELECT action_name, count(*), props
FROM analytics_event
WHERE name = 'action' AND day = (now() AT TIME ZONE 'UTC')::date
GROUP BY action_name, props;
```

`analytics_daily_session` holds one row per client-owned session, with
`first_route`, `last_route`, `page_views`, and the recomputed `engagement_ms`.
Bounce, entry, and exit are read from those columns; nothing stores them twice.

## Next

- [Configuration](configuration.md) — every option, the datasource, the secret order.
- [Data model](data-model.md) — the four tables and what each column may hold.
- [Privacy](privacy.md) — the lawful basis, retention, and how to answer a rights request.
