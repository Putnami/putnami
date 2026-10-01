# @putnami/utils

Shared utility functions for the Putnami framework with browser/server conditional exports.

> 📚 **Documentation**: For complete documentation, see the [documentation folder](./doc/). Start with [Getting Started](./doc/getting-started.md).

## Installation

```bash
bun add @putnami/utils
```

## Features

- **Declarative schema**: the one field vocabulary that configuration, endpoint
  validation, and generated documentation all consume
- **Conditional exports**: the browser build gets only browser-safe utilities;
  the server build gets those plus the Node-backed ones
- **Type safe**: a schema declaration infers its own TypeScript type
- **Documented surface**: full JSDoc with examples for every exported function

## Usage

```typescript
import { getDeep, mergeDeep, ensureArray, isString } from '@putnami/utils';

// Object utilities (browser + server)
const value = getDeep(config, 'database.host');
const merged = mergeDeep(defaults, overrides);

// Array utilities (browser + server)
const items = ensureArray(maybeArray);

// Type utilities (browser + server)
if (isString(value)) {
  console.log(value.toUpperCase());
}

// Path utilities (server only)
import { joinPath, getWorkspaceRoot } from '@putnami/utils';
const fullPath = joinPath(getWorkspaceRoot(), 'packages', 'core');
```

## API Reference

### Schema (Browser + Server)

This package owns the schema vocabulary; `@putnami/runtime` re-exports it and
`@putnami/application` validates request and response payloads with it. One
declaration therefore drives configuration reads, endpoint validation, and the
generated OpenAPI document.

```typescript
import { Default, Env, Int, Optional, Sensitive, validateSchema } from '@putnami/utils';

const DatabaseSchema = {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
  name: String,                                   // required
  password: Sensitive(Env('DB_PASSWORD', String)),// never printed by value
  poolSize: Optional(Int),
};

const { data, errors } = validateSchema(DatabaseSchema, input);
```

| Helper | Description |
|--------|-------------|
| `Optional(type)` | Field may be absent |
| `Default(type, value)` | Optional in input, always present in output |
| `Env(name, type)` | Fill from an environment variable when no source supplied the key |
| `Sensitive(type)` | Redact the value in errors, descriptions, and manifests |
| `Desc(text, schema)` | Attach documentation without losing the shape |
| `Int`, `Uuid`, `Email`, `Url`, `DateIso` | Constrained scalar types |
| `ArrayOf(type)` / `MapOf(key, value)` | Collections |
| `Min`, `Max`, `MinLength`, `MaxLength`, `Pattern`, `OneOf` | Constraints |
| `validateSchema(schema, value, opts?)` | Validate, apply defaults, optionally coerce |
| `compileSchemaValidator(schema)` | Fast path for hot validation; delegates what it cannot inline |

Coercion is off by default: a JSON body is checked against its declared type,
while path and query segments — always strings on the wire — opt in.

### Object Utilities (Browser + Server)

| Function | Description |
|----------|-------------|
| `getDeep(obj, path)` | Access nested properties via dot notation |
| `mergeDeep(a, b)` | Deep merge two objects |
| `sortObjectKeys(obj)` | Sort object keys alphabetically |
| `omitUndefinedValues(obj)` | Remove undefined values recursively |

### Array Utilities (Browser + Server)

| Function | Description |
|----------|-------------|
| `ensureArray(value)` | Wrap value in array if not already |
| `first(arr)` | Get first element |
| `last(arr)` | Get last element |
| `unique(arr)` | Remove duplicates |
| `chunk(arr, size)` | Split into chunks |

### Type Utilities (Browser + Server)

| Function | Description |
|----------|-------------|
| `isString(value)` | Check if value is string |
| `isObject(value)` | Check if value is object |
| `isPlainObject(value)` | Check if plain object (not array/date) |
| `isNullish(value)` | Check if null or undefined |
| `isFunction(value)` | Check if function |
| `throwError(msg)` | Throw error (never returns) |

### URL Utilities (Browser + Server)

| Function | Description |
|----------|-------------|
| `buildUrlWithParams(url, params)` | Build URL with query/path params |
| `parseQueryString(query)` | Parse query string to object |
| `getBaseUrl(url)` | Get URL without query/hash |
| `joinUrlPath(...segments)` | Join URL path segments |

### Task Utilities (Browser + Server)

| Function | Description |
|----------|-------------|
| `TaskQueue` | Concurrent task processor with limit |

### Path Utilities (Server Only)

| Function | Description |
|----------|-------------|
| `joinPath(...paths)` | Join path segments |
| `relativePath(from, to)` | Get relative path |
| `getDirectoryName(path)` | Get directory name |
| `resolvePath(...paths)` | Resolve to absolute path |
| `getBaseName(path)` | Get filename |
| `getExtension(path)` | Get file extension |
| `normalizePath(path)` | Normalize path |
| `isAbsolutePath(path)` | Check if absolute |
| `toPosixPath(path)` | Convert separators to forward slashes |
| `joinPosixPath(...paths)` | Join path segments with forward slashes on every platform |

### Workspace Utilities (Server Only)

| Function | Description |
|----------|-------------|
| `getWorkspaceRoot()` | Get workspace root path |
| `getProjectRoot()` | Get current project root |
| `getCurrentProject()` | Get current project info |
| `getProject(path)` | Get project by path/name |
| `listProjectDependencies(name)` | List all dependencies |

### Stack Utilities (Server Only)

| Function | Description |
|----------|-------------|
| `getCallerInfo(depth)` | Get caller stack info |
| `getExternalCaller()` | Get first external caller (outside @putnami) |
| `parseStackTrace(error)` | Parse error stack trace |

### Build Hooks (Server Only, dedicated subpath)

The JSONL hook protocol is the seam between a package's `bin/` entry point and
the CLI extension system. It is deliberately off the root barrel:

```typescript
import { emitLog, runHookCommand, standardHookModel } from '@putnami/utils/hooks';
```

## Documentation

For detailed documentation, examples, and API reference, see:

- [Getting Started](./doc/getting-started.md) - Installation and quick start
- [Browser vs Server](./doc/browser-vs-server.md) - Understanding conditional exports
- [API Reference](./doc/api-reference.md) - Complete function documentation

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). What that promises is the
behavior stated in the
[declarative-schema specification](specs/declarative-schema.json) and the
[one-vocabulary ADR](doc/adr/0001-one-schema-vocabulary-browser-safe-by-construction.md):
one schema declaration serves configuration, validation, and documentation; the
browser entry point pulls in no server-only module; and the compiled validator
agrees with the reference validator or defers to it.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
