# Workload qualification

`putnami lint,test,build,validate` proves a change statically. `putnami qualify`
proves that a running workload serves real requests, and says which build it
proved it on. It prints one verdict an agent, a reviewer or a CI check can read
without re-running anything.

The wire documents live in [`protocols/qualify`](../../../protocols/qualify/README.md);
[ADR 0039](adr/0039-derived-smoke-contracts-and-fail-closed-verdicts.md) records
why the smoke is derived and the verdict fails closed.

## `compose`

```bash
putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>] [--output <format>]
```

`compose` serves one workload together with every workload it runs with, in one
invocation, and keeps them running until it receives `SIGINT` or `SIGTERM`.

### The composition

The members are the transitive closure of the target's `runsWith`
(`putnami.json`, or `putnami.runsWith` in a TypeScript `package.json`). Each entry
resolves by project name, then by project id. Nothing else is inferred: the
dependency graph links a consumer to the generated client package it compiles
against, not to the provider workload it calls.

Planning refuses, before anything starts:

| Code | Meaning |
| --- | --- |
| `compose.not_serveable` | A member is a `library` or `image` project |
| `compose.serve_disabled` | A member's `putnami.json`, or the workspace, disables `serve` |
| `compose.unknown_member` | A `runsWith` entry names no project |
| `compose.cycle` | `runsWith` forms a cycle |
| `compose.no_serve_command` | No extension provides a serve step for a member |
| `compose.invalid_requirements` | A member's `infra/requirements.json` or client contract is invalid |

### Start order and readiness

1. Orphaned compositions are reaped (see [Recovery](#recovery)).
2. The members' `serve` pipelines are prepared through the engine: every finite
   step (config merge, generate, describe, and the steps of their dependencies)
   runs as an ordinary cached session. The long-running serve steps are withheld.
3. Databases are provisioned.
4. One reverse proxy starts per member, so every URL is known.
5. Members start in topological order, dependencies first, target last. Each
   member starts only after the one before it emitted the runtime protocol's
   typed `ready` event (v2, target `server` or `workload`). The proxy forwards to
   the port of the event's first endpoint.

Readiness is never inferred from a log line or a TCP poll. A member that exits
before its ready event fails the composition at once (`compose.member_exited`);
one that stays silent fails after `--ready-timeout`, default `60s`
(`compose.ready_timeout`). Either way the error names the member and the phase,
text output prints the last lines of the member's output on stderr, and
everything already started is torn down before `compose` exits.

Dependencies run production-mode (`NODE_ENV=production`) without watch. The
target watches by default: a change restarts it behind the same proxy URL;
`--no-watch` runs it like its dependencies.

### Proxies

Every member binds port `0` (`PORT=0` and the serve step's `port` param) and is
reached through a proxy on `127.0.0.1`. Dependants hold the proxy URL, which stays
the same across every restart of the member. While the member has no bound port —
starting, restarting, or gone — the proxy answers `503` with the platform
readiness envelope:

```json
{"status":"unavailable","checks":{"compose":"backend not ready"}}
```

The proxy passes `Host` through unchanged. The target's proxy port is `--port`,
else `options.serve.port`, else `3000`; `--port 0` picks an ephemeral port.
Dependencies always get ephemeral proxy ports.

### Injected configuration

Each member's serve step receives one `CONFIG_DATA` environment variable, which
both runtimes read at priority 60, above every configuration file:

```json
{
  "clients": {"services": {"items": {"url": "http://127.0.0.1:62715"}, "go.putnami.dev/examples/service-to-service": {"url": "http://127.0.0.1:62715"}}},
  "database": {"protocolVersion": 1, "databases": {"default": {"engine": "postgres", "schema": "public", "connection": {"host": "127.0.0.1", "port": 55432, "database": "compose_…", "user": "…", "password": "…"}}}}
}
```

- `clients.services` has one entry per member the workload runs with, under the
  service id of the provider's client contract (`x-putnami-client.service.id` in
  `schema/openapi.json`) and under the provider's project name. A provider that
  commits no contract is reachable under its project name only, and the member
  status carries a note saying so. The consumer's own identity
  (`clients.clientId`) stays in the consumer's configuration.
- `database` has one binding per datasource of the member's committed
  `infra/requirements.json`, with the first declared schema, or `public`.

A section with nothing to carry is omitted. The value is injected into the serve
step's process only: it never reaches a file in the project tree, a job context,
a cache key, the lease, an error, or the output, which name sections and
datasources only. A member's own output is not held to that: a workload can print
what it received, so its log lines stay out of every error message, structured
failure document and verdict, and reach stderr in text output only. A member whose environment already carries `CONFIG_DATA` — from
the invoking shell or from its serve task's manifest — is refused
(`compose.config_data_conflict`) rather than merged.

### Databases

`compose` uses the same PostgreSQL provider selection as test environments:

| Provider | Isolation | What happens |
| --- | --- | --- |
| Docker (default) | `database` | One server per host, reused. Each (member, datasource) gets `compose_<id>_<slug>_<datasource>`, truncated to 63 bytes, where `<slug>` is the project id lowercased with every other byte replaced by `_`. The name is recorded in the lease before the database is created, and dropped when the composition stops. |
| `PUTNAMI_TEST_PG_URL` | `none` | Members share the provided server's database. Nothing is created or dropped. |

Workloads apply their own migrations at boot; `compose` runs no migration step.
The statements run through the `psql` the pinned image ships (`docker exec`), so
the CLI carries no database driver.

### Output

Human output lists each member with its proxy URL, readiness time and injected
sections, then the serve logs. With `--output=json` or `--output=jsonl`, stdout
carries exactly two result documents and serve logs go to stderr:

1. When every member is ready: `data` is `{id, target, members[], isolation, reaped}`,
   each member `{project, proxyUrl, backendPort, databases, configSections, readyMs}`.
2. At exit: the same document without `reaped` (reported once, at start) and with `cleanup: {state, leftovers}`.

A failure is a single failure document whose `data` is `{code, id, member, phase, cleanup, reaped}`; `id` and `cleanup` are present once the failed start had created its lease.
The exit code is `0` only when the composition stopped on a signal and its
cleanup is `clean`.

### Recovery

A composition keeps `.putnami/compose/<id>/lease.json` while it runs:

```json
{"version": 1, "id": "…", "pid": 0, "createdAt": "…", "target": "/…", "pgids": [0], "groups": [{"pgid": 0, "leaderStart": "…"}], "databases": ["compose_…"], "provisionerDigest": "…", "proxyPorts": [0]}
```

The owner holds `owner.lock` for its whole life, and records each resource before
acquiring it. On `SIGINT` or `SIGTERM`, `compose` cancels its members (the process
group receives `SIGTERM`, then `SIGKILL` after five seconds), closes the proxies,
drops the databases, confirms none is left, and removes the lease. Whatever
survives is reported as `cleanup.state: partial` and stays in the lease.

A composition killed without teardown (`kill -9`, a crash) leaves its lease. The
next `compose` reaps every lease whose lock is free **and** whose pid is dead:
`SIGTERM` to each recorded process group, `SIGKILL` after two seconds, drop the
databases, remove the lease. Each reap is reported under `reaped`, with anything
it could not release named in `leftovers`.

A process group id is reused once its group is empty, so a group is signaled
only while its leader is still the process `compose` started: the lease records
the leader's start time next to the id. A group whose leader is gone, or whose
start time was never recorded, is named in `leftovers` and never signaled. An id
led by another process is no longer this composition's and is ignored. A serve
step's group leaves the lease as soon as the step returns with nothing of the
group still running, so a long watch session does not accumulate ids.

## The contract

The smoke contract is derived from the workload's route inventory
(`putnami.http-routes.v1`), read from `schema/http-routes.json` and then from
`.gen/schema/http-routes.json`. A route becomes a request when all of these hold:

- it matches exactly (no template, no prefix);
- it accepts `GET` or `HEAD` (one request per path, `GET` preferred);
- it is `publicEdge`;
- it is not a platform endpoint (`/livez`, `/healthz`, `/readyz`, `/version`),
  bare or under `--platform-prefix`, and not under `/debug/pprof` or `/_putnami/`.

The platform prefix is where the workload mounts `/readyz` and `/version`, which
the readiness and version-binding phases call. `--platform-prefix` sets it. Without
the flag, the same inventory decides: the prefix is the one under which it
declares both `GET <prefix>/readyz` and `GET <prefix>/version` as exact routes, so
a site that serves them at `/_/readyz` and `/_/version` needs no flag. With no
such pair it is the root; with pairs under two prefixes the command fails and asks
for the flag. The inventory also says whether `<prefix>/readyz` is served at all,
which decides how a local target proves readiness (see
[Readiness](#readiness)).

Requests are sorted by path and capped at 25. Each request passes when the target
answers with a status below `500`; redirects are recorded, never followed. Only
read-only requests are derived, because the inventory carries no annotation that
says a mutation is safe.

```bash
putnami qualify /go/samples/service-to-service --print-contract
```

`--print-contract` prints the contract and exits `0`, even when it is empty. The
contract carries a `digest` over its requests, so two runs can tell whether they
executed the same smoke.

An inventory that exists but fails its protocol is refused (exit `2`). A missing
inventory is not an error: the verdict is `unsupported`, and its remedy is
`putnami build --projects <id>`, which produces the inventory.

## Targets

One runner, three kinds of target. Each binds its verdict to a different proof of
which build it ran against.

| Target | `--target` | What it runs against | Binding |
| --- | --- | --- | --- |
| This machine | `local` | the workload composed with `compose`, together with every workload it runs with | `tree`: the worktree fingerprint |
| A pull request preview | the preview URL | the build CI deployed for the pull request head | `artifact`: `--expect-sha <headSha>` |
| Any deployed environment | its URL | staging, production, or any other running deployment | `artifact`: `--expect-sha <deployedSha>` |

A URL target carrying credentials, a query or a fragment is refused, because the
URL is recorded in the verdict. `--expect-sha` is a lowercase hexadecimal sha of
at least 7 characters, and is required with a URL target. With `--target local`
it is a usage error (exit `2`): a local verdict binds to the worktree instead.

### The local target

```bash
putnami qualify /go/samples/migrations-feature --target local
putnami qualify @example/go-items-consumer --target local --output=json
```

`--target local` runs one composition for the length of the run:

1. It reads the worktree fingerprint, the same one `putnami tree fingerprint`
   prints. Outside a git worktree the command fails (exit `1`) before anything
   starts: there is no tree to bind a verdict to.
2. It composes the workload exactly as `compose --no-watch --port 0` does. The
   target and every member run production-mode (`NODE_ENV=production`) without
   watch, each on port `0` behind its own proxy, with dependency URLs and
   per-composition databases injected. A watched target could rebuild itself
   during the smoke, and the verdict would no longer name one tree.
3. It runs the contract against the target's proxy URL.
4. It tears the composition down, whatever happened before.

`--ready-timeout` bounds each member's typed ready event and then the readiness
phase.

#### Readiness

A local target is ready when its application reports that its startup completed:
a typed `ready` event with target `workload`. The Go and TypeScript application
frameworks write it on their `🤖 ready` log record, only after every plugin
starter and every module start hook returned. It lists the endpoints the
application's plugins bound, such as the HTTP listener, and a worker without a
listener writes none. The composition records it for every member. A member's
first ready event, which is all compose waits for before it starts the next
member, is usually a server claim: an HTTP plugin writes one as
soon as it listens, before the rest of the application started. That claim is
not a completed startup, and neither is an answer from a route: an auth denial
or a `404` comes from a listener whatever state the application is in.

A target that never reports completed startup within `--ready-timeout` is
`timed_out`, and nothing is requested. A target that exits first is
`composition_failed`, naming the member and the phase.

When the route inventory declares `GET <prefix>/readyz` as an exact route, or
`--platform-prefix` names where the platform endpoints are mounted, readiness
polls `<prefix>/readyz` through the target's proxy instead, as it does for a URL
target. The platform plugin of both frameworks answers `503` there until the
same completed startup, so a `200` proves the same thing as the claim.

Serve logs are not part of the verdict. Human output shows them, with the
preparation's task output, only with `--verbose`, on stderr. Without `--verbose`,
and always with `--output=json`, they are dropped; the preparation still reports
its failures on stderr.

#### The tree binding

The verdict's `binding` is `{kind: "tree", fingerprint, dirty, headSHA}`, read
before the serve pipelines are prepared. The `version-binding` phase reads the
worktree again once every member is ready, and passes only when the fingerprint
is unchanged. Between the two readings the serve pipelines ran and every member
was built and started, so an equal fingerprint is what lets the verdict say the
workload it smoked was built from that exact tree.

A different fingerprint is `digest_mismatch`, and no smoke request is sent. It
means a file changed during the run: an editor, another agent, or a
preparation step that writes into the sources. The binding keeps the fingerprint
the run started from, and the diagnostic names both.

The fingerprint covers every untracked file that git does not ignore. Keep
`.putnami/` ignored: it holds the session records and the composition lease the
run itself writes.

#### Composition failures and teardown

A composition that cannot start makes `resolve-target` `composition_failed`. Its
diagnostic carries compose's code, the member and the compose phase, for example
`compose.ready_timeout: /go/samples/service-to-service: no typed ready event within 60s (phase readiness)`.
A target that exits before it reports completed startup makes `readiness`
`composition_failed` the same way, with `compose.member_exited`.
The member's own output never enters the verdict, because a workload may print the
configuration it received. Nothing is requested.

`teardown` runs on every path: after a pass, after a failure, after a failed
start and after `Ctrl-C`. The verdict's `cleanup` records what it released:

- `clean`: every member stopped and every proxy, database and lease was released.
- `partial`: `leftovers` names what survived. The `teardown` phase is `failed`,
  so the verdict is never `passed`, and the lease stays on disk.

The first `Ctrl-C` cancels the run: the active phase becomes `canceled` and the
teardown still runs. A second `Ctrl-C`, or ten seconds without the run ending,
forces the CLI to exit. What that leaves behind is reaped by the next `compose`
or `qualify --target local`, as described in [Recovery](#recovery): a lease whose
lock is free and whose owner is dead has the process groups it can confirm killed,
its databases dropped and its directory removed.

#### Databases on a provided server

With `PUTNAMI_TEST_PG_URL` set, members share the provided server's database
(isolation `none`). Nothing is created or dropped, so migrations and rows from one
run are still there in the next, and two concurrent runs see each other's data.
The verdict does not record the isolation; a proof that must start from an empty
database needs the Docker provider.

## Phases and states

Every verdict lists five phases, in order. A phase runs only when every earlier
phase passed; the others are `not_run`.

| Phase | URL target | Local target | Non-pass state |
| --- | --- | --- | --- |
| `resolve-target` | one `HEAD` on the base URL, bounded at 5s | compose the workload | `target_unreachable` (URL: connection refused, DNS, TLS, timeout); `composition_failed` (local) |
| `readiness` | `GET <prefix>/readyz` every 250 ms until `200` with `status: ok` | the application's completed-startup report (a typed `ready` event with target `workload`); `GET <prefix>/readyz` through the target's proxy when the inventory declares it | `timed_out` after `--ready-timeout` (default `60s`); `composition_failed` when a local target exits first |
| `version-binding` | `GET <prefix>/version`, compare `sha` with `--expect-sha` | read the worktree fingerprint again | `digest_mismatch` when the sha differs or is absent, or when the worktree changed |
| `smoke` | each request in order, `--request-timeout` each (default `5s`), bodies capped at 1 MiB | same | `failed` when any request answers `>= 500`, fails in transport or times out |
| `teardown` | nothing to release | stop the composition | `failed` when resources are left behind |

The verdict state is the first phase state that is neither `passed` nor `not_run`:

| State | Meaning |
| --- | --- |
| `passed` | Every phase passed, at least one request ran, and every request passed. The only pass. |
| `failed` | A request answered `>= 500`, failed in transport or timed out, or teardown left resources behind. |
| `unsupported` | The contract is empty: no route inventory, or no request safe to derive. No target is opened. |
| `not_run` | Nothing decided the run. |
| `timed_out` | A deadline expired before readiness was reached. |
| `canceled` | The run was interrupted while a phase was active. |
| `target_unreachable` | No HTTP exchange with a URL target completed. |
| `digest_mismatch` | The target is not the build the binding names: another sha on `/version`, no sha, or a worktree that changed. |
| `composition_failed` | A local target could not be composed, or exited before it reported completed startup. |

**Only `passed` exits `0`.** Every other state exits `1`. Usage errors exit `2`
before anything is contacted.

## Reading the verdict

Human output prints one line per phase, one line per request, and a final line:

```
✓ resolve-target 0ms
✓ readiness 0ms
✗ version-binding digest_mismatch 0ms
    qualify.version_missing: GET http://127.0.0.1:3801/version reports no sha, so the deployed build cannot be matched to 3cc91b658aa016af84fda3f307d3c7eb5c2b8e8a
- smoke not_run 0ms
✓ teardown 0ms
  - GET /tasks not_run 0ms
verdict: digest_mismatch (/go/samples/task-api, url, artifact expected 3cc91b658aa016af84fda3f307d3c7eb5c2b8e8a observed none)
```

A local verdict names its tree instead: `verdict: passed (/go/samples/migrations-feature, local, tree 5d41402abc4b dirty)`.

`--output=json` prints the verdict as the result envelope's `data`, on success and
on failure alike. A local verdict also carries `target.compositionId`, the proxy
URL the smoke ran against, and `cleanup`.

### The preview contract

A CI system that deploys a pull request preview qualifies it with one command:

```bash
putnami qualify <project> --target <previewUrl> --expect-sha <headSha> --output=json
```

It reads two members of `data`: `state`, and `binding.observedSHA`, the sha the
preview reported. The check passes only on `state == "passed"`; every other state,
and a non-zero exit, fails it. Show the preview URL and `observedSHA` in the check
summary, so a reviewer sees which build was proven.

## Limits

- `/version` must report the sha the workload was built from. A build that does
  not stamp it yields `digest_mismatch` against a URL target, by design. A local
  target does not read `/version`.
- A local target needs a git worktree, and fails closed when its own run writes
  into a file git does not ignore.
- `qualify` waits on a live workload, so the MCP `run_jobs` tool refuses it and
  it is never sent to a remote runner.
- The verdict is not recorded in `session.json`.
