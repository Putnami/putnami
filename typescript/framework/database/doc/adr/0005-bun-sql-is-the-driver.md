# ADR 0005 — Bun.SQL is the driver, behind a Putnami-owned client

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`) and its
  consumers of the raw client (`@putnami/analytics`)

## Context

The package ran on postgres.js. The datasource-bound client
([ADR 0003](0003-shared-physical-pool-per-database.md)) and the transaction
guard re-pointed private fields of the driver's `Query` objects (`handler`,
`executed`, `resolve`, `reject`, `cancelled`), so a driver upgrade could break
them silently. Every framework query reserves a connection and pipelines a
`set_config('search_path', …)`; on postgres.js that reservation dominated the
cost of a point `SELECT`.

Bun ships a native PostgreSQL client, `Bun.SQL`. Measured on one host against
PostgreSQL 16, a point `SELECT` through a datasource-bound client cost about
97 µs of CPU on postgres.js and about 48 µs on `Bun.SQL` (one query in flight),
and 70 µs against 27 µs with 32 in flight. `Bun.SQL` covers what the framework
needs: reserved connections, a manual `BEGIN` on a reserved connection,
multi-statement `unsafe()`, startup parameters, a password thunk called per
connection, Unix sockets, cancellation, `LISTEN`/`NOTIFY`, and nested fragments
built on another connection of the same pool. It has no column transform, no
custom type parsers, no debug hook, and no `cursor()`, `forEach()`,
`describe()`, COPY streams, `typed`, `subscribe` or `largeObject`. Its server
errors carry the SQLSTATE in `errno` and its own `ERR_POSTGRES_*` code in
`code`. Its pool dials every connection up to `max` whenever it has none
ready: on its first query, and again after the idle timeout closed them all,
where postgres.js opened connections one at a time as concurrent demand
needed them. With a password thunk, that burst also asks for one token per
connection. Integer and float arrays read from the binary protocol come back
as typed arrays (`Int32Array`, `Float32Array`, …), and an array of an enum
type reads as its text form.

## Decision

1. **Driver.** `@putnami/database` opens its connections with `Bun.SQL`. The
   `postgres` package is no longer a dependency.
2. **Pool.** The physical pool is the package's own
   (`postgres/driver-pool.ts`): single-connection `Bun.SQL` instances, created
   as concurrent demand needs them up to `poolSize`, the most recently released
   reused first so the others reach their idle timeout. An operation that finds
   them all leased waits for one. Builders come from an instance that never
   runs a query, and `LISTEN` from one of its own.
3. **Putnami-owned client.** `database()`, `Repository.conn()`,
   `useTxConnection()` and `reserve()` hand out a `SqlClient` the package
   defines (`src/sql-client.ts`), never the driver's object. A query it builds
   captures its template and runs only when awaited or executed, on the
   connection the client chooses; a nested query used as a fragment is rebuilt
   on that connection. Identifier, row and list builders, `json` and `array`
   come from the driver and touch no connection.
4. **Errors.** A server error keeps its driver class and message; its `code`
   is the SQLSTATE and `driverCode` keeps the driver's own code. A client-side
   error (connection refused, closed, timed out) keeps the driver's code.
5. **Names.** The client keeps the naming postgres.js's `camel` transform
   gave the framework, which the Repository depends on: result rows of a row
   query have camelCase keys (`created_at` as `createdAt`), `values()` and
   `raw()` results are not transformed, and `count` and `command` stay on the
   result. An identifier builder (`sql('createdAt')`) and the keys and columns
   of a row builder are written in snake_case, and an `undefined` field of a
   row builder is sent as `NULL`. JSON and JSONB values come back as stored:
   postgres.js also camelCased the keys inside them, which the session store
   had to work around.
6. **Types.** `bigint` reads as a JavaScript `BigInt`, `numeric` as a lossless
   string, `date`, `timestamp` and `timestamptz` as `Date` (a `timestamp`
   without time zone is read as UTC), `json` and `jsonb` parsed, and an array
   as a plain JavaScript array: the client turns the driver's typed arrays into
   plain ones (`bytea` stays a byte buffer). A `real` (`float4`) read with a
   bound parameter keeps the driver's single precision (`0.1` reads as
   `0.10000000149011612`). A column type the driver does not
   know, pgvector and arrays of an enum type included, reads as its text form;
   an enum array is written through text (`sql.array(values, 'text')::mood[]`).
7. **Dropped members.** `cursor()`, `forEach()`, `describe()`, COPY streams,
   `typed`, `subscribe` and `largeObject` are not part of `SqlClient`, and
   `unsafe()` and `file()` take no options argument (`.simple()` selects the
   simple protocol). A builder no longer reads the keyword before it:
   `sql([...])` is always an `IN` list, where postgres.js also made a column
   list of it after `SELECT`.
8. **Debug log.** With `debug`, the client logs each statement's text with
   every parameter value redacted, as it did through the driver's hook.

## Rejected alternatives

- **Exposing `Bun.SQL`'s own type.** The datasource-bound client cannot hand
  the shared pool's members through unchanged without escaping its
  `search_path`, and the type would tie applications to the driver again.
- **Keeping postgres.js beside `Bun.SQL`.** Two drivers double the transaction
  and pooling paths the conformance suites must hold, for no remaining caller.
- **`Bun.SQL`'s own pool.** Every instance of a workload would hold `poolSize`
  backends from its first query, and reopen them all, each with its TLS and
  authentication handshake, after every idle period.

## Consequences

- Applications that used a dropped member of the raw client, or read a
  driver-specific error code, change; the persistence contract is pre-1.0
  ([relational-persistence non-goals](../../specs/relational-persistence.json)).
- A `timestamp` without time zone read on a host whose time zone is not UTC
  no longer shifts by the host offset.
- A JSON document with snake_case keys reads back with those keys, where
  postgres.js returned them camelCased.
