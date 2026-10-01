# `dbtestenv` — the database test environment a `putnami test` run executes against

This package owns the provider-shaped half of a test database: the
`auto`/`require`/`skip` policy, the dependency-closure datasource merge, the
server itself, the `sensitive` binding artifact the test task reads, and the
non-secret lease a later invocation reclaims by. A language extension registers
the two jobs and supplies nothing else:

```go
"test-env-up":   dbtestenv.UpJob(),
"test-env-down": dbtestenv.DownJob(),
```

The design decisions behind it are
[ADR 0002 — a provisioned secret lives in exactly one artifact](../doc/adr/0002-a-secret-lives-in-one-artifact.md).

## Two providers

| Provider | Server | Selected when | Owns the server |
|----------|--------|---------------|-----------------|
| docker (`docker.go`) | a digest-keyed `postgres:17` container this run starts | `PUTNAMI_TEST_PG_URL` is unset | yes — it starts, reuses, reaps and tears down |
| provided server (`provided.go`) | a Postgres the pipeline already started | `PUTNAMI_TEST_PG_URL` is set | no — it never creates or removes anything |

Both run the same task. The policy, the closure merge, the provisioning, the
0600 artifact and the lease are provider-independent; a provider answers only
what it is, whether it can be used, whether it owns what it hands back, and what
policy its binding carries.

## Precedence

Checked in this order, each one before the next can cost anything:

1. **`DATABASE_TEST_BINDINGS`, if set, always wins.** A pipeline that hands over
   a complete binding has said everything. Nothing is provisioned, nothing is
   written, and the inherited value reaches the test task through the ambient
   environment — where the test task's own `{"from": "env"}` input folds it into
   the cache key.
2. **`PUTNAMI_TEST_PG_URL`, if set, selects the provided server.** Naming a
   server is an explicit instruction: it beats docker locally, and on CI it is
   the only option. The value is *not* parsed during selection — a malformed URL
   fails loudly, with a diagnostic naming it, instead of silently falling back to
   a container nobody asked for.
3. **Otherwise docker**, under the unchanged fail-closed gate.

## Provided-server mode

```bash
# the whole runner-side contract
docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=... postgres:17
export PUTNAMI_TEST_PG_URL='postgres://postgres:...@127.0.0.1:5432/postgres?sslmode=disable'
putnami lint,test,build --impacted
```

Each project's own `test~test-env` task then does locally what it does on CI:
merge **its own** dependency closure's datasources, resolve the policy, confirm
the endpoint answers, and write the per-project binding. There is deliberately no
workspace-union binding — a task provisions exactly its own closure, inside the
DAG where its cost is scheduled, bounded, cached and measured, rather than inside
a test framework's per-suite hook budget.

**URL shape.** `postgres://` or `postgresql://`, with whatever credentials the
server needs. A missing port resolves to `5432`, a missing path to the `postgres`
maintenance database, and the query string becomes the connection's driver
options (`?sslmode=disable`) — including any pool parameter you want to state
instead of the default below. Anything else fails with a cause that never quotes
the URL, because the URL carries a password.

**Fail-closed, restated.** The gate is fail-closed about *creating*
infrastructure: only `auto`, off CI, with docker on `PATH`, may start a server.
A provided server is not created by this run, so `require` — the CI default, and
the mode whose whole meaning is "a database must be there" — uses it, and so does
`auto`. `skip` declines in both modes.

**Pool: 2 connections, released after 5 s idle.** Every binding this package
emits carries `pool_max_conns=2`, `pool_min_conns=0` and
`pool_max_conn_idle_time=5s` in the connection's params, for both providers. The
framework's own defaults — 10 connections per datasource, held idle for 30
minutes — are right for a long-lived service and wrong for a test binary: a
workload declaring 24 datasources opens 240 connections to use one or two at a
time, exhausts a stock Postgres (`max_connections` 100), and forces the whole
pipeline to serialize its test tasks around a limit most never touch. `2` rather
than `1`, because a test holding a transaction and running one query beside it
needs a second connection — `1` deadlocks where `2` waits. A key
`PUTNAMI_TEST_PG_URL` states wins, per key: `?pool_max_conns=5` keeps 5 and
still gets the two defaults it did not contradict. Too tight shows up as a wait
rather than an error, so raise it from a named suite rather than from a guess.

**Isolation: `schema`.** One engine, one database, N schemas; no `CREATE`/`DROP
DATABASE` at all. On a shared cluster a per-suite `DROP DATABASE` forces an
immediate cluster-wide checkpoint whose cost tracks the whole fleet's concurrent
writes rather than the dropped database; `CREATE`/`DROP SCHEMA` costs neither.
Both runtime providers (`go.putnami.dev/database/testprovider` and
`@putnami/database`) implement schema isolation. In docker mode nothing is
stated, so the runtime keeps its own default of database isolation.

**Reuse: `none`.** The `bundle-template` layer amortizes migrations across suites
that share a *long-lived* server. On a server that lives one run, every template
is built cold, once per bundle, serialized on a Postgres advisory lock — pure
overhead, paid inside a per-suite hook budget. It is turned off **by policy, not
retired**: the value stays legal in the protocol, both runtime providers still
implement it, and a caller who wants it supplies their own
`DATABASE_TEST_BINDINGS` (which always wins).

**Teardown.** Nothing this package writes can remove a provided server. The lease
records `retain: true`, so the finalizer leaves it alone; `TeardownAll` is a
no-op, so `--infra-down` — which reaps what *this workspace* started — removes
nothing; and per-suite cleanup is a `DROP SCHEMA … CASCADE` the runtime provider
issues, which needs no `keepDatabases`. The server's lifetime is the pipeline's
business.

**Cache keys.** `test-env-up` is `cache: false`, but the test task's key folds
the producing action's digest, so the producer's declared inputs are the test
verdict's identity. An extension or workspace that adopts provided-server mode
should declare `PUTNAMI_TEST_PG_URL` alongside `DATABASE_TEST_BINDINGS` — as a
`{"from": "env"}` input on its `test-env-up` task, or as `options.test.envInputs`
on the project — so a run that names a server keys differently from one that does
not.

## Per-composition databases

`DatabaseIsolator` (`isolation.go`) is the optional interface a provider
implements when it can give one caller its own physical databases:
`CreateDatabase`, `DropDatabase` (which terminates the sessions still open on
it) and `ListDatabases` by literal prefix. The docker `Provisioner` implements it
by running the pinned image's `psql` through `docker exec`, with quoted
identifiers and no credential in the argv. `ProvidedServer` does not: a server
the pipeline owns is shared, never carved up.

`putnami compose` is the caller: it creates one database per (member,
datasource) of a composition and drops them when the composition stops or when
the next invocation reaps a composition that died.

## Security

Exactly one file ever holds a credential: the invocation-scoped,
`sensitive`-declared bindings artifact, created at mode 0600 inside a 0700 tree
and destroyed when the invocation ends. Nothing else this package writes — the
lease, the container name, its label, every docker filter, every event, every
diagnostic — can carry one, and the types make that checkable rather than
conventional. `Lease` has no free-form member; `serverConfig` and
`providedConfig` both exclude the password, so no digest can be derived through
it; and no error quotes the raw `PUTNAMI_TEST_PG_URL`, because `net/url` echoes
its whole input.

## Tests

`docker_test.go` and `provided_test.go` are two conformance suites asking the
same questions — resource identity, reuse, bounded readiness, teardown,
credential confinement — of the two providers, plus the fixture pinning the exact
binding artifact bytes a provided-server test task receives. Neither suite needs
a Postgres, a docker daemon or a network: every outward call goes through a seam
the tests replace. `isolation_test.go` asserts the isolator's argv and quoting the
same way, plus one test against a real server that runs only outside CI when a
docker binary is on `PATH`.
