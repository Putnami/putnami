# Getting Started

This guide will help you get started with `@putnami/runtime`, the foundational package that provides dependency injection, configuration management, context handling, error handling, and logging utilities for Putnami applications.

## Prerequisites

- [Bun](https://bun.sh/) v1.4.0 or higher
- Basic knowledge of TypeScript
- Familiarity with decorators and dependency injection concepts (helpful but not required)

## Installation

```bash
bun add @putnami/runtime
```

## Quick Start

`@putnami/runtime` provides several key features that work together to build robust applications:

### 1. Dependency Injection

Register and resolve services with explicit provider registration:

```typescript
import { ContainerContext, provide } from '@putnami/runtime';

class Database {
  query(sql: string) { return []; }
}

class UserService {
  constructor(private db: Database) {}
  getUser(id: string) {
    return this.db.query(`SELECT * FROM users WHERE id = '${id}'`);
  }
}

const ctx = new ContainerContext('app');
ctx.register(provide(Database));
ctx.register(provide(UserService, { deps: [Database] }));

await ctx.start();
const userService = ctx.get(UserService);
await ctx.close();
```

### 2. Configuration Management

Define typed configuration with schema validation:

```typescript
import { Config, useConfig, Default, Optional } from '@putnami/runtime';

const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Number, 5432),
  name: Default(String, 'postgres'),
});

// Load config (automatically reads from YAML files)
const dbConfig = useConfig(DatabaseConfig);
console.log(dbConfig.host); // 'localhost' or value from config
```

### 3. Context Management

Run code within a request context:

```typescript
import { runInContext, useContext } from '@putnami/runtime';

interface RequestContext {
  userId: string;
  requestId: string;
}

await runInContext({ userId: '123', requestId: 'abc' }, async () => {
  const ctx = useContext<RequestContext>();
  console.log(ctx.userId); // '123'
});
```

### 4. Error Handling

Use typed HTTP exceptions:

```typescript
import { NotFoundException, BadRequestException } from '@putnami/runtime';

if (!user) {
  throw new NotFoundException('User not found');
}

if (!email) {
  throw new BadRequestException('Email is required');
}
```

### 5. Logging

Use context-aware logging:

```typescript
import { useLogger } from '@putnami/runtime';

const logger = useLogger('my-service');
logger.info('Processing request', { userId: '123' });
logger.error('Failed to process', error);
```

## Project Structure

A typical Putnami application using `@putnami/runtime` might look like this:

```
src/
├── main.ts                 # Application entry point
├── config/
│   └── database.config.ts  # Configuration classes
├── services/
│   ├── user.service.ts     # Business logic services
│   └── email.service.ts
├── repositories/
│   └── user.repository.ts # Data access layer
└── app/
    └── routes/
        └── users/
            └── get.ts      # HTTP route handlers
```

## Core Concepts

### Dependency Injection Scopes

Services can be registered with different scopes:

- **Singleton** (default): One instance shared across the entire application
- **Scoped**: New instance per scope invocation (HTTP request, job, event)

### Configuration Loading

Configuration is loaded from multiple sources in priority order (highest wins):

1. Environment variables via `Env()` descriptors
2. `CONFIG_DATA` environment variable (YAML string)
3. `.gen/conf/.env.{env}.yaml` (generated files)
4. `conf/.env.{env}.yaml` (source config files)
5. `confInit` programmatic defaults
6. `Default()` schema values

The environment is automatically detected based on `NODE_ENV` and other indicators.

### Context Propagation

Context is propagated through async call stacks using AsyncLocalStorage, allowing you to access request-scoped data anywhere in your code without explicitly passing it around.

## Next Steps

- Learn about [Configuration](configuration.md) for detailed config setup
- Explore [Dependency Injection](dependency-injection.md) for advanced DI patterns
- Read about [Context Management](context-management.md) for request-scoped services
- Check out [Error Handling](error-handling.md) for comprehensive error management
- See [Logging](logging.md) for structured logging
- Review the [API Reference](api-reference.md) for complete function signatures
