# Data model

Four tables in the application's own database, created by the plugin's
migration source under the `putnami-analytics` migration namespace. JSON keys
are camelCase on the wire and snake_case in Postgres.

The tables live in the datasource's schema: `analytics.schema` when it is set,
otherwise the schema the workload's other SQL sources or `sql({ datasource })`
declare for that datasource, otherwise `public`
([configuration.md](configuration.md#the-schema)). The DDL below is
unqualified; the migration runner applies the schema as its `search_path`, and
the sink sets it for each write transaction.

## The schema, verbatim

```sql
-- 001_create_analytics_event
CREATE TABLE IF NOT EXISTS analytics_event (
    event_id       uuid        PRIMARY KEY,
    received_at    timestamptz NOT NULL DEFAULT now(),
    ts             timestamptz NOT NULL,
    day            date        NOT NULL,
    name           text        NOT NULL,
    source         text        NOT NULL,
    app            text        NOT NULL,
    env            text        NOT NULL,
    app_version    text,
    visitor_id     text        NOT NULL,
    visitor_kind   text        NOT NULL,
    session_id     uuid,
    seq            integer,
    user_id        text,
    route          text        NOT NULL,
    path           text,
    referrer       text,
    referrer_type  text        NOT NULL,
    utm_source     text,
    utm_medium     text,
    utm_campaign   text,
    utm_content    text,
    utm_term       text,
    status_code    smallint,
    render_ms      integer,
    engagement_ms  integer     NOT NULL DEFAULT 0,
    action_name    text,
    outcome        text,
    props          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    browser        text        NOT NULL,
    browser_major  smallint,
    os             text        NOT NULL,
    device_type    text        NOT NULL,
    language       text,
    viewport_class text,
    country        text
);
CREATE INDEX IF NOT EXISTS idx_analytics_event_day ON analytics_event (day);
CREATE INDEX IF NOT EXISTS idx_analytics_event_session ON analytics_event (session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_analytics_event_user ON analytics_event (user_id, day) WHERE user_id IS NOT NULL;

-- 002_create_analytics_daily
CREATE TABLE IF NOT EXISTS analytics_daily_counter (
    day       date   NOT NULL,
    dimension text   NOT NULL,
    key       text   NOT NULL,
    count     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day, dimension, key)
);
CREATE TABLE IF NOT EXISTS analytics_daily_visitor (
    day        date NOT NULL,
    visitor_id text NOT NULL,
    PRIMARY KEY (day, visitor_id)
);
CREATE TABLE IF NOT EXISTS analytics_daily_session (
    day           date        NOT NULL,
    session_id    uuid        NOT NULL,
    visitor_id    text        NOT NULL,
    first_route   text        NOT NULL,
    last_route    text        NOT NULL,
    page_views    integer     NOT NULL DEFAULT 0,
    engagement_ms bigint      NOT NULL DEFAULT 0,
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz NOT NULL,
    PRIMARY KEY (day, session_id)
);
```

`003_schedule_analytics_retention` is contributed only when
`retentionMode: 'pg_cron'`. See [privacy.md](privacy.md#retention).

## `analytics_event` — one row per event

| Column | Type | Notes |
|---|---|---|
| `event_id` | uuid | Primary key, UUID v7. The same id from the server and from the browser, which is what makes a retry idempotent. |
| `received_at` | timestamptz | Server default. |
| `ts` | timestamptz | Canonical event time. Server events: the receive instant. Client events: the client instant re-based on the batch send time, clamped into the last 24 hours. |
| `day` | date | UTC day of `ts`, computed in TypeScript. |
| `name` | text | `page_view` \| `action` \| `form_submit`. |
| `source` | text | `server` \| `client`. |
| `app`, `env`, `app_version` | text | Envelope. |
| `visitor_id` | text | 22 base64url chars. See below. |
| `visitor_kind` | text | `daily` \| `cookie`. |
| `session_id`, `seq` | uuid, integer | Client-owned; null on server rows. |
| `user_id` | text | `ctx.user.sub`, or null. Never sent to the browser. |
| `route` | text | `NOT NULL`. See the sentinels below. |
| `path` | text | Root-relative, query and fragment stripped, 512 bytes max. Null on actions and form submits. |
| `referrer`, `referrer_type` | text | `origin + pathname`; type is `direct` \| `internal` \| `search` \| `social` \| `other`. |
| `utm_source` … `utm_term` | text | 128 chars each. |
| `status_code`, `render_ms` | smallint, integer | Server rows only. |
| `engagement_ms` | integer | Visible time, from the browser. `GREATEST` on conflict, so a re-send only ever raises it. |
| `action_name`, `outcome` | text | Actions and form submits. `outcome` is `ok` \| `validation_error` \| `error`. |
| `props` | jsonb | Declared properties only. |
| `browser`, `browser_major`, `os`, `device_type` | text, smallint | Classified families, never the raw `User-Agent`. |
| `language`, `viewport_class`, `country` | text | Bounded vocabularies. |

**There is no column for an IP address, a raw `User-Agent`, a query string, a
page title, a form field, a token, or an e-mail.** The column list is the
privacy boundary: what a schema has no column for cannot be stored by accident
upstream.

## The three `route` sentinels

`route` is `NOT NULL` and is a counter dimension, so it can never carry a value
a caller chose freely — that would turn a 404 sweep into an unbounded series.

| Sentinel | Means | Written when |
|---|---|---|
| `__unmatched__` | A server view whose request matched no route pattern. | The middleware records a view and `ctx.route` is undefined. |
| `__unknown__` | A client route outside the set the build declared. | The browser named a route that is not in `.gen/http-routes.d/web.json`. |
| `__none__` | Not applicable. | Actions: the wire `action` event carries no page. |

The known-route set is read once at warmup. When the fragment is absent — a
test, or a dev run that never generated — the set is empty and every client
route is recorded as `__unknown__`.

## Visitor identity

- **`daily` (the default).** `visitor_id = base64url(HMAC(dayKey, app + "\n" + ip + "\n" + browser/os))[0..22]`,
  where `dayKey = HMAC(secret, "putnami-analytics/v1/" + utcDay)`. The key
  rotates at UTC midnight, so the same visitor yields unrelated ids on
  different days *by construction*, not by a deletion policy. The IP address
  exists only inside that function: it is never returned, logged, or stored.
- **`cookie`.** A random 16-byte id signed with the same secret, minted only
  after `consent(ctx)` returned `true` and only when neither `Sec-GPC: 1` nor
  `DNT: 1` is present. `forget(ctx)` erases it.

## Aggregates

| Table | Grain | Filled from |
|---|---|---|
| `analytics_daily_counter` | `(day, dimension, key)` | Newly inserted rows only, so a retried batch moves no counter. |
| `analytics_daily_visitor` | `(day, visitor_id)` | Newly inserted rows only. |
| `analytics_daily_session` | `(day, session_id)` | Page views with a session, pre-folded per session before the upsert. |

Counter dimensions are a closed set: `event`, `route`, `path`, `referrer_host`,
`referrer_type`, `utm_source`, `utm_medium`, `utm_campaign`, `country`,
`device_type`, `browser`, `os`, `language`, `action`, `form_submit`. A missing
value folds under `__none__`; `path` folds under `__overflow__` past
`maxPathKeysPerDay`.

### The path cap

`path` is the only dimension a visitor can enumerate: a crawler walking
generated URLs would otherwise mint a counter key per URL. Before writing a
batch that carries page views, the sink reads how many distinct `path` keys
each UTC day in the batch already holds. Past `maxPathKeysPerDay` (2000 by
default) **every** path for that day folds under `__overflow__`, existing keys
included — distinguishing them would need the day's whole key set in memory.
Each day's answer is cached independently for one minute, so a UTC rollover or
a late event never reuses another day's state. The cap is a safety valve, not a
feature: a day that hits it has an unbounded URL space, and the `route`
dimension is the one to read.

### Dedup and enrichment on `event_id`

One page view is one `event_id`, minted by the server before the handler
renders and published to the browser as `pv`. The tracker re-sends **the same
id** with what only a browser knows, and the raw upsert takes its conflict
branch:

- `session_id` and `seq` are filled when they were null, never overwritten.
- `engagement_ms` takes the `GREATEST` of the two, so a re-send only raises it.
- `referrer`, the five `utm_*` columns, `language`, and `viewport_class` are
  filled when they were null.
- Nothing else moves. `route`, `path`, `source`, `ts`, and `props` are the
  server's, and a browser cannot rewrite them.

The statement returns `(xmax = 0) AS inserted`, which is the whole dedup story:
only rows the statement *created* move a counter, a visitor, or a session. Post
a batch twice and the second delivery changes no aggregate — the sample proof
in `typescript/samples/13-fullstack-app/test/analytics.test.ts` compares all
four tables before and after a replay and asserts they are byte-identical.

A per-instance cache of recently written event ids spares the database the
round trip for an obvious retry. It is an optimization, never the truth, and it
deliberately lets any row carrying engagement through — an engagement re-send
repeats the event id on purpose.

### The daily fold

Each committed batch is folded in memory before it reaches the three daily
tables, because `ON CONFLICT … DO UPDATE` cannot touch one row twice in a
single statement. Two page views of one session in one batch therefore arrive
as a single pre-aggregated row with `page_views = 2`, the earliest
`started_at`, and the latest `ended_at`.

Session engagement is not accumulated: after the fold, the total is recomputed
from the session's raw page-view rows. Recomputing is idempotent, so a
duplicate delivery of an engagement re-send is harmless.

A session row is opened by a page view the upsert **inserted**. Enriching a
server-rendered view fills its `session_id` but does not open a session, so
`analytics_daily_session.page_views` counts client-minted views. Read total
page views from `analytics_daily_counter` where `dimension = 'route'`.

### Bounce, entry, and exit

None of the three is stored. Each is derived at read time from
`analytics_daily_session`, because storing them would be a second copy of a
fact a late-arriving event could contradict:

| Metric | Read as |
|---|---|
| entry route | `first_route` |
| exit route | `last_route` |
| bounce | `page_views = 1` |
| session duration | `ended_at - started_at` |
| engagement | `engagement_ms` |

## What a read side must do

This package writes. It exposes no read API, and a dashboard built on these
tables owes the visitor two things:

- **Small-group suppression.** A `(day, dimension, key)` cell with a count of
  one is a single visitor. Hide or bucket any cell below a threshold you choose
  before publishing an aggregate outside the operating team.
- **Aggregate before exposing.** `analytics_event` is per-event and carries
  `user_id`. Expose the daily tables, not the raw one.

Visualisation and any read API belong to Putnami Cloud, not here.

## Rows are written asynchronously, at most once

**A row is not in the database when the response returns.** Every accepted row
goes into a bounded in-memory queue, and no response path waits for the sink:
the page-view middleware, the ingest route, and `track()` all return on
acceptance into the queue, not on a commit. `202` has always meant "accepted",
never "stored"; that is now true of a rendered page and of `track()` as well.

The queue is drained by one elected request every `flushIntervalMs`, by an
unref-ed ticker on the same period, and by `stop()` — which is where a
scale-to-zero instance lands what it was holding, inside `flushDeadlineMs`.

Collection is therefore **at-most-once with bounded loss**:

- a burst past `queueCapacity` drops the **oldest** rows, counted as
  `analytics.ingest.dropped.overflow`;
- a failed write drops the rows it carried, counted as
  `analytics.ingest.dropped.sink_error`, and never retries them;
- a crash, or a drain that outlives its deadline, loses what was queued.

Nothing above is a bug: analytics is not allowed to perturb serving, and a gap
in a dashboard is cheaper than a slow page
(`doc/adr/0004-writes-are-asynchronous-and-lossy.md`). What is *delivered* is
still exactly once — `ON CONFLICT (event_id)` makes a retried event idempotent.

Anything that reads rows back immediately after producing them — a test, a
maintenance script — calls `await flushAnalytics()` first.

## The ingest response is not an oracle

`POST /_putnami/analytics/events` answers `202` with an empty body whether every
event was stored, some were, or all were dropped. `400` means the body was not
JSON or the content type was neither `application/json` nor `text/plain`; `413`
means over 65 536 bytes; `429` means the per-peer budget is spent. A sender that
could tell acceptance from a silent drop could enumerate the declared action
names and the known-route set by observation. Drops are counted as
`analytics.ingest.dropped.<reason>` metrics instead.
