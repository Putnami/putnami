# ADR 0002 — The log level is live, errors always pass, and every record is counted

- **Status**: accepted
- **Scope**: `go.putnami.dev/logger` (`go/framework/logger`) and the logger of
  `@putnami/runtime` (`typescript/framework/runtime`), which follows the same
  rule
- **Supersedes**: the sentence of ADR 0001 that says the default logger reads
  its level once from `LOG_LEVEL`

## Context

A service read its level once, at start. Raising the level to debug to
diagnose an incident meant a restart, which often clears the incident. The
per-request access line was logged at info, so info carried one record per
request and an operator could not keep info on without paying for it. Nothing
told the operator how many records a level hid.

## Decision

1. **One key, one rule.** The minimum level is the config key
   `logger.level`: `debug`, `info`, `warn` or `error`. Without the key,
   `LOG_LEVEL` sets it, and otherwise it is `info`. An unknown value counts as
   `info`.
2. **One live level per process.** The default logger, and every logger
   derived from it, read one process-wide level on each record. `SetLevel`
   (Go) and `setLogLevel` (TypeScript) change it, and the next record follows
   it. A logger built with an explicit level (`logger.New` in Go) keeps it.
3. **The application watches the key.** A started application re-reads
   `logger.level` every `logger.refreshSeconds` seconds, 30 by default and
   never when 0, from its config files and `CONFIG_DATA`. It applies the value
   only when it differs from the last value it read, so the last change wins,
   whether it came from the config or from a `SetLevel` call. The watcher
   skips remote sources: the value a remote source gave at start stays until a
   local change or a `SetLevel` call, and the refresh never calls the network.
4. **Errors always pass.** A record at error or above is emitted whatever the
   minimum level.
5. **The access line is debug.** The per-request access line logs at debug,
   except a failure, which stays at error.
6. **Every record is counted.** Each record a logger receives counts once in
   `log.records`, labeled `level` and `outcome` (`emitted` or `dropped`). Go
   keeps cumulative counts that the telemetry plugin observes; TypeScript
   notifies the telemetry collector on each record.
   `protocols/telemetry/fixtures/conventions/log-records.json` holds the
   convention both runtimes test against.

## Consequences

- An operator edits `logger.level` in a mounted config file and the service
  follows within `logger.refreshSeconds`, without a restart.
- Export directives, which change what a service sends at runtime, hold a
  floor and so wrap this rule instead of changing it: they tighten the level
  through a separate process-wide limit, `SetLevelLimit` /
  `setLogLevelLimit`, that every logger consults on each record. The live
  level stays the config's; a config change or a `SetLevel` call during a
  directive applies, and the stricter of the two holds until the directive's
  ttl ends.
- Dashboards that counted info access lines must count `http.server.*`
  metrics, which every request records.

## Rejected alternatives

- **Reload the whole configuration.** Other blocks hold connections and
  credentials; reloading them in place is a larger contract that no block
  needs today.
- **Poll remote config sources.** Every instance would call the config server
  every refresh, and a remote source can block on retries.
- **Watch files with OS notifications.** Mounted config volumes replace
  files through symlink swaps that notifications miss; a stat-free re-read of
  a few small files every 30 seconds costs nothing.
