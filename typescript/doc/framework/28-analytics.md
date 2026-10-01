# Analytics

The `@putnami/analytics` plugin measures your own web application — page views, declared actions, form outcomes, sessions — and writes the result to your own PostgreSQL. No third party receives it, nothing leaves the process, and the default mode needs no consent banner.

> Analytics is opt-in: composing the plugin is the opt-in. There is no default endpoint and no default egress.

## Getting started

Add the dependency, then compose the plugin after `sql()`:

```bash
putnami deps add @putnami/analytics
```

```ts
import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';

export const events = declareEvents({ counter_click: { value: String } });

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(react()).use(analytics({ events }));
```

`analytics()` contributes its four tables as a migration source, so it needs `sql()` in the composition. Where it sits relative to `react()` makes no difference.

Set a key, or the application refuses to start:

```yaml
analytics:
  secret: ${ANALYTICS_SECRET}   # any string of 32 characters or more
```

`session.cookieSecret` is reused when `analytics.secret` is absent.

Then build once. The browser tracker is a build output, not a runtime file:

```bash
putnami build .
```

Until it exists the plugin logs a warning and keeps collecting server-side page views.

## What is collected

| Event | Written by | Carries |
|---|---|---|
| `page_view` | the server on every rendered page, enriched by the browser | route, path, referrer, the five `utm_*` parameters, engagement time, session |
| `action` | `track()` server-side; `track`, `useTrack`, or `data-track` in the browser | a declared name and its declared properties |
| `form_submit` | the server, from the action outcome | route and `ok` / `validation_error` / `error` |

Every row also carries the application, environment, version, a visitor id, the browser and operating-system families, the device type, the language, the viewport class, and an optional country from a trusted edge header.

Nothing else. There is no column for an IP address, a raw `User-Agent`, a query string, a page title, a form field, a token, or an e-mail — so nothing upstream can store one by accident.

### One page view, two halves

The server mints the page-view id before the handler renders, writes its row, and publishes the same id to the browser as `window.__putnamiBootstrap.analytics.pv`. The tracker re-sends **that id** with what only a browser knows: the session, the referrer, the `utm_*` parameters, the viewport class, the language, and the engagement time. The sink upserts on `event_id`, so the two halves become one row.

That is why a page view survives an ad blocker: the row exists whether or not the tracker ever loaded. It is also why a retry costs nothing — a batch that arrives twice enriches the rows it already wrote and moves no counter.

A pre-rendered page has no hydration script to publish the id in: `page().static()` HTML is rendered at build time, with no request. The middleware therefore injects the tracker into the response it serves, carrying the same id, on a `<script type="module" data-putnami-analytics="…">` tag. No CSP change is needed — the tag is a same-origin script, not an inline one — and a fully pre-rendered site collects the same sessions, engagement, and actions as a server-rendered one.

## Declaring and tracking actions

Action names are declared up front, which is what keeps the payload bounded: the browser can only ever send a name that already exists.

```ts
export const analyticsEvents = declareEvents({
  task_created: { priority: Number },
  project_archived: {},
});
```

From the browser, with no handler and no import:

```tsx
<button type='button' data-track='counter_click' data-track-value='1'>
  Clicked
</button>
```

`data-track-*` attribute values are always strings, so declare those properties `String`.

From a component:

```tsx
import { useTrack } from '@putnami/analytics';

export function SignupButton() {
  const track = useTrack();
  return <button onClick={() => track('signup_click', { plan: 'pro' })}>Sign up</button>;
}
```

From a server handler — `track()` needs the request context, and only the HTTP layer has one:

```ts
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

An `endpoint()` handler narrows its own `ctx`, so read the full request context from the async context. An undeclared name throws at the call site.

## Where the data lands

Four tables, in your database:

| Table | Grain |
|---|---|
| `analytics_event` | one row per event |
| `analytics_daily_counter` | `(day, dimension, key)` |
| `analytics_daily_visitor` | `(day, visitor_id)` |
| `analytics_daily_session` | `(day, session_id)` |

```sql
SELECT key AS route, count
FROM analytics_daily_counter
WHERE day = (now() AT TIME ZONE 'UTC')::date AND dimension = 'route'
ORDER BY count DESC;
```

Bounce, entry, and exit are derived at read time from `analytics_daily_session`; nothing stores them twice.

## Configuration highlights

| Key | Default | What it does |
|---|---|---|
| `mode` | `cookieless` | `identified` writes a persistent first-party cookie and requires a `consent(ctx)` callback. |
| `datasource` | `analytics` | Falls back to `default` when that name is declared nowhere. |
| `schema` | — | The schema the tables live in. Unset, they follow the schema the datasource already declares, else `public`. |
| `serverPageViews` | `true` | `false` collects from the browser only. |
| `retentionRawDays` | `90` | How long a raw event lives. |
| `retentionAggregateDays` | `760` | How long a daily aggregate lives. |
| `retentionMode` | `sweep` | `pg_cron` schedules the deletion in the database; `off` leaves expiry to you. |
| `rateLimitPerMinute` | `120` | Per-peer ingest budget. |

The full table, the code-only options, and the datasource and schema resolution are in the package's `doc/configuration.md`.

## Privacy summary

The default mode places no cookie and puts nothing on the identity path in the browser. The visitor id is a keyed hash of an address that is never itself stored, under a key that rotates at UTC midnight — so the same visitor produces unrelated ids on two days by construction. That is what removes the consent requirement, not a policy decision.

The tracker does keep two first-party keys in the browser, and your notice has to say so: the current visit id in `sessionStorage`, gone when the tab closes, and the not-yet-sent events in `localStorage`, cleared as they are acknowledged. Neither is an identity the server reads back, and neither survives a day — but both are storage on the visitor's terminal, so "we store nothing on your device" is a sentence to avoid.

`Sec-GPC: 1` and `DNT: 1` are honoured by default: either header keeps the request cookieless.

`mode: 'identified'` is the opposite trade: a persistent first-party cookie, minted only after your `consent(ctx)` callback returned `true`, and erased by `forget(ctx)`. The banner and the record of consent are your application's.

The package's `doc/privacy.md` carries the field-by-field table, the lawful basis per mode, the retention story — the opportunistic sweep is best effort, `pg_cron` is the guarantee — and how to answer an access or erasure request. `doc/privacy-notice-template.md` is the paragraph to paste into your own privacy page.

## Next: visualisation in Putnami Cloud

This plugin writes. It exposes no read API and ships no dashboard, on purpose: the tables are yours, and a query is the smallest possible read side. Visualisation and a hosted read API belong to Putnami Cloud. Until then, apply small-group suppression before publishing any aggregate outside the team that operates the application — a daily cell with a count of one is a single visitor.

## Related guides

- [Add web analytics](/docs/how-to/add-web-analytics)
- [Web](/docs/frameworks/typescript/web)
- [Persistence](/docs/frameworks/typescript/persistence)
- [Telemetry](/docs/frameworks/typescript/telemetry)
- [Configuration](/docs/frameworks/typescript/configuration)
