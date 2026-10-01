# @putnami/analytics

Cookieless web analytics collection for Putnami web applications. Page views,
declared actions, form outcomes, sessions, and daily aggregates land in the
application's own PostgreSQL. No third party, no default egress, and no consent
banner in the default mode.

## Compose it

```ts
import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';

export const events = declareEvents({ counter_click: { value: String } });

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(react()).use(analytics({ events }));
```

`analytics()` after `sql()`: the four tables are the plugin's own migration
source. Without `sql()` the plugin logs one warning, counts events in metrics,
and stores nothing — the application still serves.

Set `analytics.secret` (any string of 32 characters or more) or
`session.cookieSecret`. Startup fails closed without one.

## What is collected

| Kind | Written by | Carries |
|---|---|---|
| `page_view` | the server on every rendered page, enriched by the browser | route, path, referrer, the five `utm_*` parameters, engagement time, session |
| `action` | `track()` server-side, `track`/`useTrack`/`data-track` in the browser | a declared name and its declared properties |
| `form_submit` | the server, from the action outcome | route and `ok` / `validation_error` / `error` |

Every row also carries the application, the environment, the version, a visitor
id, the browser and operating-system families, the device type, the language,
the viewport class, and an optional country from a trusted edge header.

Pre-rendered pages count. A `page().static()` route is served from build output
the renderer never touched, so the middleware injects the tracker and that
request's page-view id into the response — a fully pre-rendered site collects
sessions, engagement, and declared actions like any other.

## What is never collected

No IP address, no raw `User-Agent`, no query string, no page title, no form
field, no token, no e-mail address. The `analytics_event` column list is the
privacy boundary: what a schema has no column for cannot be stored by an
accident upstream. The browser is never told who the visitor is.

## Where the data lands

Four tables in the application's own database: `analytics_event` and the daily
`analytics_daily_counter`, `analytics_daily_visitor`, and
`analytics_daily_session` folds. Nothing leaves the process.

## Documents

- [Getting started](doc/getting-started.md) — install, compose, run, read the tables.
- [Configuration](doc/configuration.md) — every option and the two fail-closed startup checks.
- [Data model](doc/data-model.md) — the tables, the sentinels, the dedup rule, the daily fold.
- [Client API](doc/client-api.md) — the browser tracker, sessions, the queue, the retry rules.
- [Privacy](doc/privacy.md) — lawful basis, retention, opt-out signals, access and erasure.
- [Privacy notice template](doc/privacy-notice-template.md) — the paragraph to paste into your own privacy page.

Contract: [web-analytics-collection specification](specs/web-analytics-collection.json).
The wire contract belongs to `go.putnami.dev/protocol/analytics`.
