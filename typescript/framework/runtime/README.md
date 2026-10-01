# @putnami/runtime

Core utilities for dependency injection, context management, configuration, HTTP error handling, and logging.

## Installation

```bash
bun add @putnami/runtime
```

## Features

- **Dependency Injection** - Hierarchical DI container with singleton and scoped lifecycles
- **Context Management** - Async-local storage for request-scoped data propagation
- **Configuration** - Type-safe YAML-based configuration with validation
- **Error Handling** - Comprehensive HTTP exception classes and utilities
- **Logging** - Context-aware structured logging with JSON support
- **Model Utilities** - Class transformation and validation helpers

## Quick Start

### Dependency Injection

```typescript
import { provide, ContainerContext } from '@putnami/runtime';

class Database {
  query(sql: string) { return []; }
}

class UserService {
  constructor(private db: Database) {}
  getUser(id: string) {
    return this.db.query(`SELECT * FROM users WHERE id = '${id}'`);
  }
}

// Register providers
const providers = [
  provide(Database),
  provide(UserService, { deps: [Database] }),
];

// Use with @putnami/application for full lifecycle:
// application().provide(Database).provide(UserService, { deps: [Database] })
```

### Configuration

```typescript
import { Config, useConfig } from '@putnami/runtime';
import { IsString, IsNumber } from 'class-validator';

@Config({ path: 'database' })
class DatabaseConfig {
  @IsString() host = 'localhost';
  @IsNumber() port = 5432;
}

const dbConfig = useConfig(DatabaseConfig);
console.log(dbConfig.host);
```

### Context Management

```typescript
import { runInContext, useContext } from '@putnami/runtime';

await runInContext({ userId: '123' }, async () => {
  const ctx = useContext();
  console.log(ctx.userId); // '123'
});
```

### Error Handling

```typescript
import { NotFoundException, BadRequestException } from '@putnami/runtime';

if (!user) {
  throw new NotFoundException('User not found');
}
```

### Logging

```typescript
import { useLogger } from '@putnami/runtime';

const logger = useLogger('my-service');
logger.info('Processing request', { userId: '123' });
logger.error('Failed to process', error);
```

> **📚 Learn More:** See [Getting Started Guide](doc/getting-started.md) for a complete walkthrough.

## Documentation

Comprehensive guides for all features:

- **[Getting Started](doc/getting-started.md)** - Installation, setup, and quick examples
- **[Configuration](doc/configuration.md)** - Type-safe configuration with YAML files and validation
- **[Dependency Injection](doc/dependency-injection.md)** - DI patterns, scopes, and best practices
- **[Context Management](doc/context-management.md)** - Request-scoped context and async propagation
- **[Error Handling](doc/error-handling.md)** - HTTP exceptions, validation errors, and error patterns
- **[Logging](doc/logging.md)** - Context-aware logging and structured output
- **[API Reference](doc/api-reference.md)** - Complete API documentation

## API Overview

### Dependency Injection

| Function | Description |
|---------|-------------|
| `application()` | Create a new DI application with lifecycle management |
| `provide(token, ...)` | Create a provider registration |
| `module(name, ...)` | Create a module grouping related providers |
| `named<T>(name)` | Create a named token for non-class values |
| `tagged<T>(tag)` | Create a tag selector for multi-resolution |

### Configuration

| Function | Description |
|---------|-------------|
| `@Config(options)` | Decorator for configuration classes |
| `useConfig<T>(type, options?)` | Load and validate configuration |
| `getEnv()` | Get current environment ('local', 'test', 'production') |
| `resetConfigLoader()` | Reset config cache (for testing) |

### Context Management

| Function | Description |
|---------|-------------|
| `runInContext<C, R>(context, fn)` | Run code within an async context |
| `useContext<C>()` | Get the current context |

### Error Handling

| Class/Function | Description |
|---------------|-------------|
| `HttpException` | Base class for all HTTP exceptions |
| `BadRequestException` (400) | Client error exceptions |
| `NotFoundException` (404) | Resource not found |
| `InternalServerErrorException` (500) | Server error exceptions |
| `ValidationException` | Formatted validation errors |
| `AggregateException` | Multiple errors in one exception |
| `createHttpException(status, message?)` | Factory for creating exceptions |
| `isHttpException(value)` | Type guard for HTTP exceptions |

### Logging

| Function | Description |
|---------|-------------|
| `useLogger(name?)` | Get a context-aware logger |
| `Logger.with(key, value)` | Add request-scoped context, or return a derived logger outside a request scope |

> **📚 Full API:** See [API Reference](doc/api-reference.md) for complete documentation.

## Common Patterns

### Service with Dependencies

```typescript
import { application, module, provide } from '@putnami/runtime';

class DatabaseService {
  query(sql: string) { /* ... */ }
}

class UserRepository {
  constructor(private db: DatabaseService) {}
  findById(id: string) {
    return this.db.query(`SELECT * FROM users WHERE id = '${id}'`);
  }
}

const app = application()
  .provide(DatabaseService)
  .provide(UserRepository, { deps: [DatabaseService] });

await app.start();
const repo = app.get(UserRepository);
await app.close();
```

### Scoped Services

```typescript
class RequestData {
  requestId = crypto.randomUUID();
}

const app = application()
  .provide(RequestData, { scope: 'scoped' });

await app.start();

await app.scope(async (scope) => {
  const data = scope.get(RequestData);
  console.log(data.requestId); // Unique per scope
});

await app.close();
```

### Multi-Datasource Configuration

```typescript
@Config({ path: 'database' })
class DatabaseConfig {
  @IsString() host = 'localhost';
  @IsNumber() port = 5432;
}

// Load from different paths
const authDb = useConfig(DatabaseConfig, { path: 'database.auth' });
const mainDb = useConfig(DatabaseConfig, { path: 'database.main' });
```

### Validation with Errors

```typescript
import { ValidationException } from '@putnami/runtime';
import { validate } from 'class-validator';

const errors = await validate(userDto);
if (errors.length > 0) {
  throw new ValidationException(errors);
  // Returns 400 with formatted errors
}
```

## Project Structure

A typical project using `@putnami/runtime`:

```
src/
├── config/
│   └── database.config.ts  # Configuration classes
├── services/
│   └── user.service.ts     # Business logic
├── repositories/
│   └── user.repository.ts # Data access
└── app/
    └── routes/
        └── users/
            └── get.ts      # Route handlers
```

## Dependencies

This package relies on:

- **[@putnami/utils](../utils/README.md)** - the schema vocabulary re-exported here
- **yaml** (`^2.8.2`) - YAML parsing

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It carries two public
promises:

- **[Dependency injection](specs/dependency-injection.json)**, with the
  [composition-time lifetimes ADR](doc/adr/0001-lifetimes-and-visibility-are-composition-time-decisions.md).
  The whole graph is validated when the container starts — a missing provider,
  a cycle, or an unmet module requirement fails there rather than on the first
  request that needs it — and disposal runs in reverse construction order.
- **[Typed configuration](specs/typed-configuration.json)**, with the
  [precedence and secrets ADR](doc/adr/0002-configuration-precedence-and-name-only-secrets.md).
  Sources merge in a fixed order, a field-level `Env()` fills only keys nothing
  else supplied, and a `Sensitive()` field contributes its name — never its
  value — to errors, descriptions, and infrastructure manifests.

The schema helpers re-exported here (`Default`, `Env`, `Sensitive`, `Int`, …)
are owned by [`@putnami/utils`](../utils/README.md).

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

This project is the TypeScript conformance fixture of the executable spec
gate: [`putnami.features.json`](putnami.features.json) declares
acceptance checks for the `singleton-identity` requirement of the
[dependency-injection spec](specs/dependency-injection.json), the protecting
tests bind themselves with `specTest` from `@putnami/runtime/spectest`, and
every `test` run publishes the reserved verification report. The project
deliberately stays on the inherited workspace `report` mode; `putnami specs
verify` replays the recorded verdict.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
