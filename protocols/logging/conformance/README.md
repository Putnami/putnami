# Logging Conformance Corpus

`manifest.json` is the canonical **cross-runtime structured-log contract** for
the Putnami frameworks: golden log records that the Go and TypeScript
frameworks must both emit for the same boundary events, byte-for-byte after
the canonicalization described below. It is the artifact behind the
acceptance line *"shared conformance fixtures or golden tests assert
equivalent Go/TypeScript JSON output."*

Log fields are a public contract: dashboards, alerts, and log-based metrics
are written against these key names. Changing a key, a severity, or a message
in one runtime without the other — or without updating this corpus — is a
breaking change to that contract, and this corpus is what turns that drift
into a test failure instead of a silent production divergence.

## The contract

### Field naming and shape

- **camelCase keys everywhere**: `messageId`, `durationMs`, `durationUs`,
  `thresholdMs`, `rollbackCause`, `dlqTopic`, `sourcesForKind`, …
- **Domain groups**: boundary data nests under exactly one top-level key per
  boundary — `http`, `event`, `database`, or `migration`. There is no `job`
  group (no job/scheduler subsystem exists; the CLI JSONL job protocol in
  `protocols/runtime` is a deliberately separate wire format).
- **Framework-reserved top-level keys** (never inside groups, never forgeable
  by user fields): `severity`, `message`, `timestamp`, `logger`, `traceId`
  (or `logging.googleapis.com/trace` when a GCP project is configured), and
  `error`.
- **Durations are integers.** Every boundary terminal record carries
  `durationMs`; `database` records additionally carry `durationUs` because
  sub-millisecond queries are the norm there.

### Outcome vocabulary

`success` | `failure` | `canceled` | `skipped`, shared by every group.
Transactions map committed → `success` and rolled-back → `failure`, with the
*classified* reason in `rollbackCause` (never raw error text — the rollback
cause classification is a secret-safety invariant).

### Errors

Failures carry the logger's **structured `error` field** (name / message /
stack where safe, plus runtime-specific extras), with existing redaction
guarantees. Error text and dynamic identifiers are **never interpolated into
messages** — messages are stable constants so log-based metrics can match on
them:

| Boundary | Messages |
|----------|----------|
| http | `[<METHOD>] <routePath>` (same message for success and failure — severity and `error` carry the failure) |
| event | `message handled`, `message handling failed`, `message retry scheduled`, `message dead-lettered`, `message dropped` |
| database | `query executed`, `query failed`, `slow query`, `transaction committed`, `transaction rolled back` |
| migration | `migration applied`, `migration failed` |

### Severity policy

- Database query failure, slow query, and transaction rollback are
  **WARNING**: the pool reports outcome + latency with a structured error and
  *propagates* the error; the boundary terminal record owns ERROR, exactly
  once, only if the failure actually bubbles. A retried-and-recovered query
  never produces error-level noise.
- HTTP terminal: INFO below 500, ERROR at ≥ 500 (with the real handler error
  in `error`).
- Event terminal: INFO on success, ERROR on failure. Retry scheduling is
  WARNING; dead-letter and drops are ERROR.

### Terminal-record policy

Exactly **one terminal record per request/event boundary**. Warnings/errors
outside the terminal record are allowed only when they represent independent
operational signals (e.g. an advisory-lock release failure, a slow query).

A dead-lettered message reports **one** message-level outcome: the transport
enqueues the dead-letter first and then emits `message dead-lettered` if at
least one target accepted it, or a single `message dropped`
(`dlq_enqueue_failed`) if none did. The two are mutually exclusive — an operator
alerting on `message dead-lettered` must never be pointed at a DLQ the message
never reached — and a per-target refusal is not a message-level drop when
another target accepted the message.

### Attempt semantics

`event.attempt` is the **1-based delivery count of that delivery**. A retry
record reports the failed delivery's `attempt` and the scheduled
`nextAttempt` (= `attempt + 1`); exhausted-retry / dead-letter records report
the true final attempt count — not attempt + 1.

`event.maxRetries` is expressed in the **same unit as `attempt`**: the
maximum total delivery attempts for the message, regardless of what the
runtime's configuration option is named or how it counts (total deliveries
vs. number of retries). With `maxRetries: 2`, a permanently failing message
is delivered exactly twice: attempt 1 emits a retry record with
`nextAttempt: 2`; attempt 2 emits the dead-letter (or drop) record with
`attempt: 2`.

### Pinned logger names

`http`, `events.handler`, `events.publish`, `events.broker`, `database`,
`database.migration`. (Dots, not colons.)

### Context accumulation

Repeated work inside one boundary accumulates instead of last-write-losing:
`append(path, value)` and `increment(path, delta)` primitives exist in both
runtimes with a shared cap `MAX_APPENDED_FIELD_VALUES = 100` (the constant is
defined with that exact name in `go/framework/logger` and
`@putnami/runtime`; keep the three definitions — Go, TS, this README — in
sync). Pair every `append` with an `increment` of a sibling counter so the
true total survives the cap, as `event.publishCount` / `event.publishes`
does in the `http.terminal.success-with-publishes` case.

## Canonicalization (how records are compared)

Each runtime's harness captures the **real JSON log line** produced by the
real sink (`JSONSink` in Go, the `buildJsonRecord` path in TypeScript) while
driving **real boundary code** (middleware / dispatch / pool with stub
handlers), then:

1. Parse the line as JSON. (The TypeScript harness compares
   `JSON.parse(JSON.stringify(buildJsonRecord(entry)))`, which is the same value
   the sink prints — so both runtimes compare the aggregator's view, with
   `undefined` fields already dropped.)
2. **Tokens** — a leaf whose *expected* value is one of the `tokens` keys is
   type/pattern-checked and then **replaced by the token text on both sides**
   (so a value that legitimately varies per run is pinned by type, and a
   mismatch shows the arriving value against the token):
   - `<iso8601-millis-utc>` — string matching
     `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`
   - `<number>` — any JSON number
   - `<string>` — any non-empty JSON string
3. **`$open` objects** — an expected object carrying `"$open": true` allows
   extra keys in the actual record: unexpected keys are stripped before
   comparison and the `$open` marker is removed. `$open` is permitted **only**
   on:
   - `error` — runtime-specific extras legitimately differ (Go `ErrorInfo`
     adds `code` / `category` / `retryable` / `source`; TypeScript adds
     `cause` and redacted custom fields);
   - the `database` group on **query** cases — TypeScript records at the
     repository layer and adds `table`; the Go pool cannot know it. The
     `operation` value vocabulary is also runtime-specific (Go:
     `exec`/`query`/`queryRow`; TS: statement kinds), which is why it is
     pinned as `<string>`, not a literal.
4. Re-serialize with **sorted keys, 2-space indent** (Go's `encoding/json`
   sorts map keys natively; the TS helper sorts explicitly — this sidesteps
   insertion-order differences).
5. **Byte-compare** against the case's `record`, itself re-serialized the
   same way.

Everything not covered by a token or `$open` is exact — an **unexpected key
anywhere fails both runtimes**. Byte comparison after normalization is
deliberate (same rationale as `protocols/infra/fixtures/equivalence`): it
catches *extra* fields (drift) as well as missing ones.

**Record selection.** A harness picks the record under test by the case's
pinned `logger` + `message` pair and requires **exactly one** match: the
terminal-record policy is part of the contract, so two records for one boundary
event fail the case rather than letting the harness choose one.

**Failure message.** Both harnesses print the two canonical forms and then name
the three files a contract change must touch together: this corpus, the Go
boundary test, and the TypeScript boundary test. A failure in one runtime means
either the change forgot the twin runtime or it forgot the corpus — the message
says so, and says "update the fixture AND the other runtime's test".

Run with `GOOGLE_CLOUD_PROJECT` / `GCP_PROJECT` unset so the trace key stays
plain `traceId`.

`traceId` presence: `http` and `event` cases require it (the harness installs
the request-id/trace middleware, and event dispatch seeds the envelope's
trace). `database` and `migration` cases run on a background context and must
**not** emit it.

Fixture routes use **static paths** (`/conformance/orders`): the two routers'
parameter syntaxes differ by design (Go `{id}`, TypeScript `[id]`), and this
corpus pins field *shape*, not router syntax.

## Cases

13 `required` + 1 `recommended` (`http.terminal.success-with-publishes`,
which additionally proves the accumulation primitives end to end). See
`manifest.json` — each case carries a documentation-grade `drive` block
describing the stub wiring; the per-runtime harnesses hard-code that wiring
per case id.

## Runners

All 14 cases are executable in both runtimes.

- **Go:** `go.putnami.dev/logger/logtest` — `LoadCases(t, manifestPath,
  boundary)` / `NewRecorder(t)` (captures the real `JSONSink` output through
  `logger.NewJSONSinkWriter`) / `AssertRecord(t, gotLine, wantCase)`, with
  `CompareRecord` as its pure, unit-tested core. Thin
  `logging_cross_language_test.go` callers live next to each boundary.
- **TypeScript:** `@putnami/runtime/testing` (`log-conformance`) — `loadCases`
  / `findCase` / `findRecord` / `assertRecord`, rendering each captured
  `MemoryLogger` entry through the sink's own `buildJsonRecord`, with
  `compareRecord` as its pure core. Thin `logging-cross-language.test.ts`
  callers live in `application`, `events`, and `database`.

Where each boundary's cases are driven:

| Cases | Go | TypeScript |
|-------|----|------------|
| `http.terminal.success`, `http.terminal.5xx` | `go/framework/http` | `typescript/framework/application` |
| every `event.*`, plus `http.terminal.success-with-publishes` | `go/framework/events` | `typescript/framework/events` |
| every `db.*` and `migration.*` | `go/framework/database` | `typescript/framework/database` |

Two placements are dependency-driven, not arbitrary: the accumulation case
needs a publisher *and* the HTTP middleware, and only the events module depends
on the HTTP module (never the reverse); the migration records are emitted by the
database module in both runtimes (`go/framework/migration` and
`@putnami/migration` own only the registry's records, and the database module
depends on them, not the other way round).

**Gating.** The `db.*` cases need a live Postgres and use the same gating as the
transaction conformance corpus (`DATABASE_TEST_BINDINGS`; skipped when absent).
The `migration.*` cases are **not** gated: both runtimes drive their real
migrator over a DB-free recording connection, so migration-record drift is
caught by the ordinary unit gate.

## Update procedure

A contract change lands as **one commit** touching three places: this corpus,
the Go emitters/tests, and the TypeScript emitters/tests. A failure in one
runtime's conformance test means either your change forgot the twin runtime
or it forgot this corpus — the failure message names both. Never loosen a
case with `$open` or a token to make one runtime pass; that defeats the
corpus's purpose.

## Pack manifest

[`pack.json`](./pack.json) follows the pack-manifest convention
(`protocols/transaction/conformance/README.md`): `id`
`putnami.logging.conformance`, `corpus` `manifest.json`, `languages`
`["go", "typescript"]`. `capabilityKinds` is empty — this pack certifies the
logging contract, not a capability kind.
