# ADR 0002 — Events land in the application's own database and are folded at ingest

- **Status**: accepted
- **Scope**: `@putnami/analytics` (`typescript/framework/analytics`)

## Context

Analytics data describes the application's own users. Shipping it to a service
the application does not operate decides ownership for the adopter and adds a
third-party egress. Putnami's data-ownership principle says the application
keeps its data.

A serverless application has no process between requests, so a background timer
that folds counters either never runs or keeps an instance warm. The write path
must also survive replays: the server records a page view and the browser
re-sends the same event with engagement time, and beacons replay on tab restore
or network flaps. A double count makes every aggregate wrong.

## Decision

The sink is the application's own Postgres, through the `analytics` datasource,
falling back to `default`. Tables arrive as migrations through the application's
migration path. There is no read API and no cloud push.

Each accepted batch is one transaction. The raw insert is an
`ON CONFLICT (event_id) DO UPDATE` that only enriches: it fills a null session,
sequence, referrer, campaign, language or viewport, takes the greater engagement
time, and returns `xmax = 0` to mark new rows. Only new rows feed the counter,
daily-unique and session aggregates. The primary key is the dedup, so a replay
an hour later is as safe as one a second later.

Aggregates fold at ingest, in that same transaction: pre-aggregated in memory
for the batch, then written with one upsert each.

Retention has three modes (`retentionMode`). `sweep`, the default, deletes
expired rows during ingest at most every ten minutes, 1 000 rows per table per
sweep, so it never turns a request into a long transaction. `pg_cron` schedules
expiry in the database, so it runs while the application is scaled to zero; use
it when expiry is an obligation. `off` disables expiry.

A form submission's outcome comes from the status of the response the action
produced: `400` and `422` mean invalid input, any other failing status means the
action failed, and a redirect is a success. Returning
`json({ errors }, { status: 400 })` is the documented way to fail a form, so
"returned rather than threw" is not success.

A sink failure logs one structured error without the payload. Every drop is
counted and never reported to the sender.

## Invariants

- Re-sending a batch, or re-sending the server-rendered page view from the
  client, changes no counter; only enrichment columns move.
- The ingest route answers `202` for every semantically bad batch.
- `analytics.ingest.accepted`, `analytics.ingest.dropped.<reason>` and
  `analytics.sink.write_ms` expose drops and write latency; they are no-ops
  without composed telemetry.
- Aggregates are written in the same transaction as their raw rows.
- Under `pg_cron`, expiry runs while the application is idle.

## Rejected alternatives

- **Object storage or an append-only log.** No framework primitive, no daily
  upsert; a query path over files is a second database, badly.
- **Folding in a background timer.** It never fires, or it keeps an instance
  warm.
- **A dedup cache in front of the insert.** Bounded window, dies with the
  instance, wrong with two instances. The primary key already answers.
- **Pushing to Putnami Cloud.** An egress the adopter did not ask for.
- **Reporting per-event drops in the response.** It becomes an oracle a sender
  can probe for the identity rules and the closed vocabulary.
- **An action outcome published by the client.** The browser does not know
  whether the server accepted the submission.

## Consequences

- An application without a database cannot use this package.
- A new aggregate dimension is a migration plus a backfill, not a query.
- Ingest cost grows with batch size, which is why the wire caps a batch at 50
  events.
- `sweep` is best-effort. A hard retention obligation needs `pg_cron` and its
  extension dependency.
