# CLI telemetry and your rights

Putnami CLI records a small, closed set of usage signals to help maintain and
improve the CLI. This page explains the current processing and the controls
available to you.

## Controller and contact

Fabien Dumay is the controller for the CLI telemetry described here. For
access, rectification, erasure, objection, or telemetry questions, write to
[contact@putnami.com](mailto:contact@putnami.com).

## Purpose and lawful basis

The purpose is product improvement for Putnami's own CLI: understanding broadly
used commands, workload shape, broad failure categories, and performance trends.
The lawful basis is legitimate interests, not consent. Read the versioned
[Legitimate Interests Assessment](https://github.com/Putnami/putnami/blob/main/tooling/cli/doc/cli-telemetry-lia.md)
for the purpose, necessity, and balancing analysis.

The CLI remains fully functional when telemetry is disabled.

## Exact data fields

The client enforces a compiled allowlist before storing or exporting an event.

| Event | Fields |
|-------|--------|
| `session:start` | Public job-command names; project count; job count; `interactive`; presence-only booleans for `flag.impacted`, `flag.coverage`, `flag.output`, `flag.no-cache`, `flag.projects`, and `flag.watch` |
| `session:end` | `success`; duration in milliseconds; `interactive`; a failure-only `errorCategory` of `usage`, `auth`, `api`, or `failure` |
| OTLP envelope | `event.name`; `device.id`; `cli.version`; `os`; `arch`; `service.name=putnami-cli`; `putnami.framework=go` |

Command names and error categories use receiver-enforced closed vocabularies;
unknown extension-defined command names are omitted. Putnami does **not**
collect code, file paths, project or workspace names, environment variables,
configuration, error messages, source-control data, user identity, or caller IP
addresses.

## Identifier and local retention

The CLI creates a random device identifier only when it records its first
eligible event. It rotates on each UTC month boundary; events retain the ID from
the month in which they were recorded. `putnami telemetry off` deletes both the
device ID and the local event buffer.

The local JSONL buffer holds at most 1,000 events. You can inspect it with
`putnami telemetry show` or remove it with `putnami telemetry off`.

## Notice, opt-out, CI, and agents

The first eligible interactive run prints a one-line notice and writes its
timestamp locally. That notice run records locally only; it does not transmit a
previous buffer. Non-TTY runs, including scripts and agents, never display a
notice and collect only after an interactive notice has been shown on the same
machine. Their events are tagged `interactive=false`.

You can object at any time with any of these controls:

- `putnami telemetry off` — explicit opt-out; deletes the local buffer and ID.
- `DO_NOT_TRACK=1` — disables telemetry unless an explicit CLI config setting wins.
- `PUTNAMI_TELEMETRY=off` (also `0` or `false`) — disables telemetry unless an explicit CLI config setting wins.

CI is disabled by default. `putnami telemetry status` shows the effective state,
the rule that decided it, and the current device-ID month when one exists.

## Remote delivery, retention, and location

The CLI sends eligible buffered events to Putnami's managed receiver at
`https://telemetry.putnami.dev`. `PUTNAMI_TELEMETRY_ENDPOINT` remains available
as an explicit override for controlled testing. Delivery is fail-silent and
never changes the command result.

Sanitized raw events are stored in a dedicated Google Cloud Logging bucket in
`europe-west1` (Belgium) and expire after 1 day. Durable storage in Cloud SQL
contains aggregate product counts and a 35-day privacy-control membership
projection keyed by monthly rotating device ID. The projection exists only to
enforce distinct-contributor suppression; it contains no raw events and is
never exposed by the read API. The managed receiver does not retain caller IP
addresses. Project counts, job counts, and durations are stored only as fixed
ranges, such as 6–20 projects or 30 seconds to 2 minutes, and flags only as the
number of sessions that set them; no exact value is kept.

Aggregates never leave the receiver as individual records. Internal readers see
them only through a private, authenticated contract that returns counts for
fixed 1-day, 7-day, and 30-day UTC windows, counts distinct rotating IDs without
ever returning one, and withholds any group unless at least five distinct
rotating device IDs contributed to it. Repeating an event does not increase
that contributor count. Each aggregate dimension has a fixed storage ceiling;
if one ceiling is reached, that dimension is reported as unavailable rather
than returning a plausible partial count, while unaffected dimensions remain
available. Aggregate rows and their contributor-membership projection are
retained for 35 days. There is no export, no raw-event access, and no way to ask
about a single device. A database-scheduled expiry runs even when the receiver
has scaled to zero; admission and deletion cover only the current UTC day and
the preceding 34 days.

## Your rights

You may object through the opt-out mechanisms above. To request access or
erasure of remote records, contact the controller. The rotating ID, bounded
local buffer, and remote raw-event TTL are designed so that data also ages out
without a request.

## Separate application telemetry

This page covers the Putnami **CLI** only. Telemetry in applications built with
the Go or TypeScript frameworks remains separately configured and opt-in; it is
not enabled or controlled by these CLI settings.
