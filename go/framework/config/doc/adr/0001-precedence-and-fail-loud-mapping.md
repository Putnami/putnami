# ADR 0001 — Merge configuration by priority and fail loud while mapping

- **Status**: accepted
- **Scope**: `go.putnami.dev/config` (`go/framework/config`)

## Context

Applications combine committed YAML, environment files, generated dependency
configuration, local secrets, remote sources, container data, and per-field
environment overrides, with overlapping nested keys. A malformed port,
duration, or secret must not silently become a zero value, and diagnostics must
not expose a sensitive value.

## Decision

Every `Source` declares an integer priority. Loading sorts a copy of the list
and deep-merges from lower to higher priority, copying nested maps and slices
so no source-owned data is mutated. A set `env` tag overrides the merged value.
A `default` tag applies only when the merged sources lack the field. Nested
structs still get their `env` and `default` tags when the parent key is absent.

Mapping fails when a scalar does not parse, a tag targets an unsupported kind,
or a value cannot be assigned, converted, or recursively mapped. For fields
tagged `sensitive:"true"`, diagnostics keep the name and kind and replace every
raw failing value with `[redacted]`, including in inner errors. Blocking
optional sources implement `ContextSource`, used by `LoadContext`; the
synchronous `Source` contract is unchanged. Remote backends stay out of core.

## Rejected alternatives

- **First source wins.** Callers would depend on discovery order, and nested
  overrides would replace whole objects.
- **Fall back to zero on invalid values.** The error surfaces later as an
  unrelated failure, or binds an unintended port.
- **Redact only in the outer error.** Attribute inspection walks the whole
  chain, so the inner parse error would leak the value.
- **Remote configuration in core.** It brings credentials, retry policy, and
  dependencies the core loader must not carry.

## Consequences

- Changing a source priority changes effective configuration and needs a
  documented migration.
- Multi-word fields need an explicit `env` tag, because `EnvSource` treats
  underscores as path separators.
- Each new destination kind needs an explicit mapping and failure rule.
