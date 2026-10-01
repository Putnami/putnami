# @putnami/utils

Shared utilities used across the Putnami framework. No internal dependencies.

## Structure

- **Shared** — helpers safe for both browser and server
- **Server** — Bun-specific utilities
- **Client** — browser-specific utilities

## Exports

Import from `@putnami/utils`:

- String/object utilities
- Type guards and assertion helpers
- Schema validation helpers
- Browser/server conditional exports (auto-resolved by bundler)

Import from `@putnami/utils/hooks` (server only):

- JSONL hook protocol — event emitters, command runner, and `HookContext`/`HookEvent` types (used by the extension system)

## Detailed Documentation

See `doc/` folder:
- `overview.md` — package structure
- `api-reference.md` — exported utilities
- `browser-vs-server.md` — conditional exports

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns the schema vocabulary the rest
of the framework re-exports; its behavior is defined by the
[declarative-schema specification](specs/declarative-schema.json) and the
[one-vocabulary ADR](doc/adr/0001-one-schema-vocabulary-browser-safe-by-construction.md).
Two rules govern every change here: a module under `src/shared` may not import a
Node built-in, because that import reaches every browser consumer; and the
compiled validator delegates any case it cannot reproduce exactly rather than
approximating it. Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
