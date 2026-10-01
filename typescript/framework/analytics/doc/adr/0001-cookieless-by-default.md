# ADR 0001 — Visitor identity is a daily-rotating server-side hash unless the application asks for consent

- **Status**: accepted
- **Scope**: `@putnami/analytics` (`typescript/framework/analytics`)

## Context

Telling two visitors apart usually creates personal data: a cookie is durable
device state, a fingerprint is durable state the visitor cannot see, and a
stored network address is an identifier at rest.

Under the CNIL audience-measurement exemption, a measurement that cannot follow
a visitor across days, sites or sessions needs no consent banner. That exemption
lets an application compose the plugin and collect with no UI or legal work. An
application that wants a durable visitor must ask for consent, and that switch
is the application's to flip.

## Decision

The default mode is cookieless. The visitor id is
`base64url(HMAC-SHA256(dayKey, app + "\n" + clientIp + "\n" + uaFamily))`
truncated to 22 characters, with
`dayKey = HMAC-SHA256(secret, "putnami-analytics/v1/" + utcDay)`. The key
derives from the UTC day, so one person yields unrelated ids on either side of
midnight by construction, not by a deletion job. `visitor_kind` is `daily`. The
address and the raw user agent exist only inside that function; only the hash
and a coarse browser/OS family leave it.

The plugin fails closed at warmup when no server-side key is configured
(`analytics.secret`, else `session.cookieSecret`), because a missing key yields
a guessable hash.

`mode: 'identified'` mints a persistent signed cookie `<id>.<sig>`, only after
the application's `consent(ctx)` callback returned `true`. The signature is
checked with a constant-time compare; a cookie that fails it is treated as
absent. `visitor_kind` is `cookie`.

`Sec-GPC: 1` (`respectGpc`) and `DNT: 1` (`respectDnt`), both respected by
default, force cookieless collection whatever the mode. They are evaluated
before the consent callback runs.

The browser is never told who it is. The bootstrap carries `pv`, `route`,
`endpoint`, `app`, `env`, `version` and the declared action names, with no
`visitor_id` and no `user_id`.

## Invariants

- No network address, raw `User-Agent`, query string, page title, form field,
  token or e-mail is written to the database or to logs.
- In `cookieless` mode the plugin emits no `Set-Cookie` header.
- In `identified` mode a cookie appears only after `consent(ctx)` returned
  `true`, and never under a respected `Sec-GPC: 1` or `DNT: 1`.
- The bootstrap handed to the browser carries no visitor or user identifier.
- A person's visitor id is unrelated across two UTC days.

## Rejected alternatives

- **A persistent cookie by default.** It needs consent and a banner, a cost that
  belongs only to applications choosing identified mode.
- **A client-generated fingerprint.** Unbounded inputs derived where the visitor
  cannot inspect them: exactly what the exemption excludes.
- **Truncating the IP address.** Still an address at rest, joinable with other
  logs.
- **A long-lived salt rotated by a job.** Rotation then depends on the job
  running.
- **Letting the browser send the visitor id.** Any script on the page could read
  and exfiltrate it.

## Consequences

- Two devices behind one address with one browser family count as one visitor
  that day. Audience measurement is a trend, not a census.
- Session and unique-visitor aggregates are per UTC day. Cross-day metrics need
  identified mode.
- Rotating `analytics.secret` resets visitor identity from that moment; the key
  is a documented operational input.
- Identified mode requires the application to own the consent UI; this package
  does not provide one.
