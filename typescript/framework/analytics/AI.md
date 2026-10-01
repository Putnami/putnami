# @putnami/analytics

Cookieless web analytics collection for Putnami web applications. Page views,
declared actions, form outcomes, sessions, and daily aggregates land in the
application's own PostgreSQL. No third party, no default egress, no consent
banner in the default mode.

## Browser / server boundary

The package publishes two entries and they never share code:

| Export condition | Entry | Contains |
|------------------|-------|----------|
| `browser` (and any browser-like loader) | `src/index.browser.ts` → `src/client/**` | The tracker, `track`, `useTrack`, `data-track`, the session, the queue, the transport |
| `default` (bun/node, and the `bin/*` executables) | `src/index.ts` → `src/server/**` | The plugin, the ingest route, the page-view middleware, the sanitizer, the sink |

The boundary is enforced by the packaging tooling, not by convention: the
TypeScript extension partitions entrypoints by export condition and runs one
`bun build` per graph. Consequences when working in this package:

- Anything importing `node:crypto`, `@putnami/database`, or a server-only
  `@putnami/runtime` API must stay reachable only from `src/index.ts`. Adding
  such an import under `src/client/**` breaks the browser build loudly instead
  of shipping a broken bundle.
- `useTrack` is exported from **both** entries on purpose: a component that
  calls it also renders on the server, where it is a no-op.
- `test/entrypoints.test.ts` mirrors the published shape and fails if a server
  symbol becomes reachable from the browser entry closure.
- The browser tracker is a **build output**. `bin/generate.ts` bundles
  `src/client/entry.ts` into `.gen/<publicFolder>/analytics/analytics.<hash>.js.gz`
  and writes `.gen/http-routes.d/analytics.json`. Adding
  `"@putnami/analytics": "workspace:*"` to a project's `package.json` is what
  makes that pre-build hook run for it.
- The published package ships no `.ts` under `src/`. The entry is exported as
  `./tracker` (browser condition only) so packaging emits
  `src/client/entry.js`, and the hook bundles that file when the source is
  absent. `test/generate-hook.test.ts` covers the published shape; a hook that
  only knew the source path would break every npm consumer's build.

## Setup

```ts
import { analytics, declareEvents } from '@putnami/analytics';
import { application, http, logger } from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';

export const events = declareEvents({ counter_click: { value: String } });

export const app = () =>
  application().use(http()).use(logger()).use(sql({ autoApply: true })).use(react()).use(analytics({ events }));
```

Two conditions **fail closed at warmup**, not per request:

- no `analytics.secret` and no `session.cookieSecret` — the visitor hash has no key;
- `mode: 'identified'` with no `consent(ctx)` callback — a persistent cookie nobody gated.

Without `sql()` the plugin logs `analytics: no sql() plugin — events are
counted in metrics only and not stored` once, uses the counting no-op sink, and
contributes no migration. The application still serves.

## `analytics(options)`

Everything expressible in YAML lives in the `analytics` config section; two
members are code, not configuration.

| Option | Type | Default | What it does |
|---|---|---|---|
| `enabled` | boolean | `true` | `false` registers no route and no middleware. The build still declares the tables. |
| `mode` | `'cookieless' \| 'identified'` | `'cookieless'` | `cookieless` is a daily-rotating server-side hash and needs no banner. `identified` writes a persistent first-party cookie and requires `consent`. |
| `datasource` | string | `'analytics'` | The datasource the tables live in. Falls back to `default` when the name is declared nowhere. |
| `schema` | string | — | The schema the tables live in. Unset, they follow the schema another SQL source or `sql({ datasource })` declares for the datasource, else `public`. |
| `secret` | string (sensitive) | — | The HMAC key. Falls back to `session.cookieSecret`. |
| `serverPageViews` | boolean | `true` | `false` stops recording server-rendered views; the bootstrap is still injected. |
| `respectGpc` | boolean | `true` | Treat `Sec-GPC: 1` as an opt-out from the persistent cookie. |
| `respectDnt` | boolean | `true` | Treat `DNT: 1` as an opt-out from the persistent cookie. |
| `countryHeader` | string | — | The trusted edge header carrying a two-letter country. There is no GeoIP lookup. |
| `trustedProxies` | string[] | — | Exact IPs and CIDR ranges allowed to set `X-Forwarded-For`. |
| `retentionRawDays` | int | `90` | How long a raw event row lives. |
| `retentionAggregateDays` | int | `760` | How long a daily aggregate lives. |
| `retentionMode` | `'sweep' \| 'pg_cron' \| 'off'` | `'sweep'` | `sweep` is best effort during ingest; `pg_cron` is the guarantee. |
| `maxPathKeysPerDay` | int | `2000` | Above this many distinct `path` keys in a day, new ones fold under `__overflow__`. |
| `rateLimitPerMinute` | int | `120` | Per-peer ingest budget. The 121st request in a minute is `429`. |
| `flushIntervalMs` | int | `5000` | Flush period of the write queue: one elected request per period pays for a batch, and the ticker runs on the same period. |
| `flushWaitMs` | int | `1000` | The most an elected request waits for the flush it started. |
| `queueCapacity` | int | `5000` | Rows held in memory; past it the **oldest** are dropped and counted as `analytics.ingest.dropped.overflow`. |
| `flushBatch` | int | `200` | The most rows one write carries. |
| `flushDeadlineMs` | int | `5000` | How long `stop()` drains before giving up. |
| `cookieName` | string | `'_pa'` | The `identified` cookie name. |
| `cookieMaxAgeDays` | int | `390` | The `identified` cookie lifetime. |
| `consent` | `(ctx) => boolean \| Promise<boolean>` | — | **Code only.** The consent gate; required in `identified` mode. A callback that throws counts as "no consent". |
| `events` | `Record<string, PropsSchema>` | `{}` | **Code only.** Declared action names and their property types. |

## Public API

| Symbol | Side | What it does |
|---|---|---|
| `analytics(options)` | server | The plugin. Registers the page-view middleware, the tracker asset route, and `POST /_putnami/analytics/events`. |
| `declareEvents(events)` | server | Declares action names and property types (`String`, `Number`, `Boolean`). Validates at declaration; throws naming the offending name, key, or type. |
| `track(ctx, name, props)` | server | Records one declared action. Resolves on acceptance into the write queue, never on a commit. Throws **synchronously** on an undeclared name. |
| `flushAnalytics(deadlineMs?)` | server | Writes what the queue is holding, or gives up at the deadline. For tests and maintenance, never for a response path. |
| `forget(ctx)` | server | Erases the `identified` cookie on the response. |
| `useTrack()` | browser + server | React hook returning `track(name, props)`. Reads `window.__putnamiAnalytics` at call time, so it is a no-op during SSR and on a static page. |
| `track(name, props)` | browser | Records one declared action from the tracker. |
| `data-track="name"` | browser | Declarative click tracking, no handler needed. |
| `onNavigation(listener)` | browser + server | Subscribes to client-side route changes; re-exported from `@putnami/web`. |

### `declareEvents`

```ts
export const analyticsEvents = declareEvents({
  task_created: { priority: Number },
  project_archived: {},
});
```

Names match `^[a-z][a-z0-9_]{0,31}$`, property keys the same, at most 20
properties per event, string values truncated to 256 UTF-8 bytes. Declaring up
front is what makes the payload bounded: the browser can only move a name that
already exists here.

### `track` — server

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

`track()` takes the full `HttpRequestContext`. An `endpoint()` handler receives
a narrowed `EndpointRequestContext`, which is not assignable to it, so read the
request context from the async context — the same call the `@putnami/web`
action handler makes. A plain route handler or middleware passes `ctx` straight
through.

`track()` resolves as soon as the row is accepted into the write queue: no
response path in this package ever waits for Postgres, and a sink failure is
counted, not raised. A caller that must see the row — a test, a maintenance
script — calls `await flushAnalytics()`. See
`doc/adr/0004-writes-are-asynchronous-and-lossy.md` for what that costs: drift
of up to `flushIntervalMs`, and bounded loss under pressure.

### `useTrack` and `data-track` — browser

```tsx
import { useTrack } from '@putnami/analytics';

export function SignupButton() {
  const track = useTrack();
  return <button onClick={() => track('signup_click', { plan: 'pro' })}>Sign up</button>;
}
```

```tsx
<button type='button' data-track='counter_click' data-track-value='1'>
  Clicked
</button>
```

One capturing, passive listener on the document serves the whole page,
including markup React mounts later. `data-track` is the action name; each
`data-track-*` attribute becomes a property whose key has its dashes turned
into underscores (`data-track-seat-count` → `seat_count`). Attribute values are
**always strings**, so declare those properties `String` — a `Number`-typed
property is dropped on arrival.

### `onNavigation`

```ts
import { onNavigation } from '@putnami/analytics';

const stop = onNavigation((detail) => console.log(detail.route, detail.pathname));
```

It does **not** fire for the initial load: the server bootstrap is the initial
view, and a listener that assumed otherwise would count the landing page twice.

## How the bootstrap reaches the browser

Two paths, and which one runs depends on what produced the document.

| Document | Carries the bootstrap as | Written by |
|---|---|---|
| Server-rendered (`react()` renderer) | `window.__putnamiBootstrap.analytics` in the hydration script | The middleware writes the request slots; the renderer serializes them |
| Pre-rendered (`page().static()`), or any other HTML a handler returns as a string | `data-putnami-analytics` on an injected `<script type="module">` | The middleware, after `next()`, in `src/server/http/inject.ts` |

`@putnami/web` marks the request context (`markClientBootstrapEmitted`) when
its renderer emits the slots; the middleware injects only when that mark is
absent. Nothing is injected into a streamed body, into a response with no
`</body>` (counted as `analytics.page_view.tracker_not_injected`), or when no
tracker bundle was built. The injected id is the id the server row was written
under, so the browser enriches that row instead of minting a second view.

The attribute exists because a pre-rendered page has no CSP nonce: an inline
script would need `'unsafe-inline'`. The literal is duplicated in
`src/client/entry.ts` — nothing in the browser bundle may import from
`src/server/**` — and `test/static-page-tracker.test.ts` pins the two copies.
See [ADR 0005](doc/adr/0005-pre-rendered-pages-get-the-tracker-at-serve-time.md).

## Route sentinels

`route` is `NOT NULL` and is a counter dimension, so it never carries a value a
caller chose freely — that would turn a 404 sweep into an unbounded series.

| Sentinel | Means | Written when |
|---|---|---|
| `__unmatched__` | A server view whose request matched no route pattern. | The middleware records a view and `ctx.route` is undefined. |
| `__unknown__` | A client route outside the set the build declared. | The browser named a route absent from `.gen/http-routes.d/web.json`. |
| `__none__` | Not applicable. | Actions: the wire `action` event carries no page. |

The known-route set is read once at warmup. An absent fragment yields the empty
set, which is the safe direction: every client route folds under `__unknown__`.

## The ingest response is not an oracle

`POST /_putnami/analytics/events` answers `202` with an empty body whether
every event was stored, some were, or all were dropped. `400` means the body
was not JSON or the content type was neither `application/json` nor
`text/plain`; `413` means over 65 536 bytes; `429` means the per-peer budget is
spent. The route is CSRF-exempt by contract — a `sendBeacon` cannot carry a
token — and is bounded instead by the per-peer rate limit, the body cap, the
50-event batch cap, and the closed vocabulary the sanitizer enforces. Drops are
counted as `analytics.ingest.dropped.<reason>` metrics, where only the operator
reads them.

## What is never stored

No IP address, no raw `User-Agent`, no query string beyond the five `utm_*`
keys, no page title, no form field, no token, no e-mail. The `analytics_event`
column list is the boundary. The browser is never told `user_id`: the bootstrap
carries only `pv`, `route`, `endpoint`, `app`, `env`, `version`, `declared`.

## Documents

- `README.md` — what it is, in thirty lines.
- `doc/getting-started.md` — install, compose, run, read the tables.
- `doc/configuration.md` — every option, the datasource resolution, the fail-closed checks.
- `doc/data-model.md` — the DDL, the sentinels, the dedup rule, the daily fold, the read side.
- `doc/client-api.md` — the browser tracker, sessions, the queue, the retry rules.
- `doc/privacy.md` — lawful basis, retention, opt-out signals, access and erasure.
- `doc/privacy-notice-template.md` — the paragraph an application owner pastes into its privacy page.

## Support and feature ownership

`@putnami/analytics` owns the modeled
[`typescript/web-analytics-collection`](putnami.features.json) feature. Its
[specification](specs/web-analytics-collection.json),
[identity decision](doc/adr/0001-cookieless-by-default.md),
[sink decision](doc/adr/0002-app-owned-sink-and-daily-fold.md), and
[protocol-scope decision](doc/adr/0003-shared-protocol-typescript-only-runtime.md)
are canonical. The wire contract itself belongs to
`go.putnami.dev/protocol/analytics`; visualisation and any read API belong to
Putnami Cloud. The project verifies its spec under `enforce`, so a requirement
declared here needs a `specTest` binding before it can be added. No support
status, default-framework claim, or cross-language parity is implied.
