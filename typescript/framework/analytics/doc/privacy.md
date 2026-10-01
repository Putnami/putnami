# Privacy

This page is written for the person who has to answer for the processing: the
application owner. `@putnami/analytics` is a library, not a service. It stores
what you compose it to store, in your database, and it never sends anything
anywhere else.

Terms used once and then reused: **RGPD** (Règlement Général sur la Protection
des Données, the European General Data Protection Regulation), **CNIL**
(Commission Nationale de l'Informatique et des Libertés, the French data
protection authority), **HMAC** (Hash-based Message Authentication Code, a
keyed hash), **UTM** (Urchin Tracking Module, the five `utm_*` campaign
parameters), **GPC** (Global Privacy Control, the `Sec-GPC` request header),
and **DNT** (Do Not Track, the `DNT` request header).

## Lawful basis, per mode

| Mode | Identity | Lawful basis | Banner |
|---|---|---|---|
| `cookieless` (default) | a server-side HMAC that rotates every UTC day | audience measurement, no consent | none |
| `identified` | a persistent first-party cookie | consent | your application owns it |

**`cookieless`.** No cookie, and nothing on the identity path: the visitor id
is derived server-side and the key rotates at UTC midnight, so the same visitor
produces unrelated ids on two days *by construction*. That is the shape the
CNIL's audience-measurement exemption is written for.

The browser tracker does write two first-party keys, and a notice has to say
so: the current visit id in `sessionStorage` (`putnami.analytics.session`,
gone when the tab closes) and the not-yet-sent events in `localStorage`
(`putnami.analytics.queue:*`, cleared as they are acknowledged). Neither
identifies a visitor to the server — the server never learns them as an
identity — and neither survives as a cross-day identifier, but both are storage
on the visitor's terminal in the sense of ePrivacy Article 5(3), which the
audience-measurement exemption covers only as long as they stay strictly for
that. Say what they are; do not claim nothing is stored.

You still owe the visitor a notice — see
[privacy-notice-template.md](privacy-notice-template.md) — but not a consent gate.

**`identified`.** The plugin refuses to start without a `consent(ctx)`
callback, and it mints the cookie only when that callback returned `true`. The
banner, its wording, and the record of consent are your application's; the
plugin only asks. `forget(ctx)` erases the cookie on the response.

## The exact fields

| Field | Value | Notes |
|---|---|---|
| `event_id`, `session_id`, `seq` | UUID v7, integer | The session is client-owned and expires after 30 minutes of inactivity or at a UTC day boundary. |
| `ts`, `day`, `received_at` | instants | A client instant is re-based on the batch send time and clamped into the last 24 hours. |
| `name`, `source` | `page_view` / `action` / `form_submit`, `server` / `client` | |
| `app`, `env`, `app_version` | strings | Your application's own envelope. |
| `visitor_id`, `visitor_kind` | 22 characters, `daily` / `cookie` | See the identity section below. |
| `user_id` | `ctx.user.sub`, or null | The authenticated subject, when your application has one. Never sent to the browser. |
| `route`, `path` | pattern and root-relative path | Query and fragment stripped, 512 bytes maximum. |
| `referrer`, `referrer_type` | origin plus pathname, and a five-value class | Query and fragment stripped. |
| `utm_source` … `utm_term` | five strings, 128 characters each | Only the five `utm_*` keys are read from the query string. |
| `status_code`, `render_ms`, `engagement_ms` | numbers | |
| `action_name`, `outcome`, `props` | declared name, three-value class, declared properties | `props` carries only keys you declared, of the type you declared. |
| `browser`, `browser_major`, `os`, `device_type` | classified families | Never the raw `User-Agent`. |
| `language`, `viewport_class`, `country` | bounded vocabularies | `country` only when a trusted edge header supplies it. |

**What is never collected:** an IP address, a raw `User-Agent`, a query string
beyond the five `utm_*` keys, a page title, a form field, a token, or an e-mail
address. There is no column for any of them, so nothing upstream can store one
by accident.

## Identity

The cookieless visitor id is

```
visitor_id = base64url(HMAC(dayKey, app + "\n" + ip + "\n" + browser/os))[0..22]
dayKey     = HMAC(secret, "putnami-analytics/v1/" + utcDay)
```

The IP address exists only inside that function. It is never returned, logged,
or stored, and there is no GeoIP lookup — an operator who wants a country names
a trusted edge header that already resolved one.

**The secret never touches the database.** It lives in configuration and in
process memory. A read of every analytics table therefore recovers no visitor:
the ids are the output of a keyed hash whose key rotates daily and is not
stored beside them.

## Retention

| Setting | Default | Deletes |
|---|---|---|
| `retentionRawDays` | 90 | `analytics_event` |
| `retentionAggregateDays` | 760 | the three daily tables |
| `retentionMode` | `sweep` | how the deletion runs |

**`sweep` is best effort, and `pg_cron` is the guarantee.** The sweep runs
opportunistically inside an ingest transaction, at most once every ten minutes
per instance, bounded to a thousand rows per table per pass. An application
that stops receiving traffic stops expiring data, so a retention job that only
runs while traffic is arriving is not a retention commitment.
`retentionMode: 'pg_cron'` contributes a migration that schedules
`analytics_expire()` at 00:15 UTC in the database itself, which runs whether or
not anyone visits. Use it when you have to state a retention period to a
visitor or a regulator. It fails the migration when `pg_cron` is not
admin-installed, on purpose: silently omitting the job would couple expiry to
request-serving uptime while still reporting a guarantee.

**Changing the day counts after the pg_cron migration has run does not change
the schedule.** The window is baked into `analytics_expire()` when the migration
applies, and a migration runs once. Shortening `retentionRawDays` from 90 to 30
for a regulator therefore leaves rows living 90 days until you ship a new
migration that replaces the function. Switching `retentionMode` away from
`pg_cron` does not unschedule the job either: the down migration only runs if
you roll it back explicitly, so the cron keeps firing on the old window while
the sweep also runs. Treat the pg_cron window as part of the schema, not as
configuration you can turn.

`retentionMode: 'off'` deletes nothing, and you own the expiry.

## Opt-out signals

`Sec-GPC: 1` and `DNT: 1` are honoured by default (`respectGpc`,
`respectDnt`). Either header keeps the request cookieless: no persistent cookie
is read and none is minted, and the visitor falls back to the daily hash. The
signals do not stop the cookieless measurement itself, which stores no
identifier on the device and re-identifies nobody.

## Answering an access or erasure request

- **A logged-in visitor.** `user_id` is the join key. `SELECT … WHERE user_id =
  $1` returns everything held about that person; `DELETE FROM analytics_event
  WHERE user_id = $1` erases it. The daily aggregates carry no `user_id` and
  are not re-identifiable, so they stay.
- **A cookieless visitor.** There is nothing to look up and nothing to erase.
  The id is a keyed hash of an address that was never stored, under a key that
  rotated. That is not a gap in the implementation: it is the reason the mode
  needs no consent. Say so in your answer.
- **An identified visitor.** The cookie value is the id. The visitor supplies
  it, or your application reads it from the request that carries the cookie;
  `DELETE FROM analytics_event WHERE visitor_id = $1` erases the rows, and
  `forget(ctx)` erases the cookie.

## Before you expose an aggregate

A daily counter with a count of one is a single visitor. Apply small-group
suppression — hide or bucket any cell below a threshold you choose — before
publishing an aggregate outside the team that operates the application.

## Related

- [Configuration](configuration.md) — the option table and the fail-closed checks.
- [Data model](data-model.md) — the columns, the sentinels, the daily fold.
- [Privacy notice template](privacy-notice-template.md) — the paragraph to paste into your privacy page.
- [ADR 0001 — cookieless by default](adr/0001-cookieless-by-default.md).
