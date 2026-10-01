# Getting Started

This guide will help you get started with `@putnami/utils`, a comprehensive collection of utility functions for object manipulation, type checking, URL handling, and more.

## Prerequisites

- [Bun](https://bun.sh/) v1.4.0 or higher
- TypeScript knowledge (helpful but not required)
- Basic understanding of JavaScript/TypeScript

## Installation

```bash
bun add @putnami/utils
```

## Quick Start

The package provides conditional exports - browser builds get only browser-safe utilities, while server builds get all utilities including file system operations.

### Basic Usage

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
```

### Server-Only Utilities

In server environments (Bun), you also have access to path and workspace utilities:

```typescript
import { joinPath, getWorkspaceRoot } from '@putnami/utils';

// Path utilities (server only)
const fullPath = joinPath(getWorkspaceRoot(), 'packages', 'core');
```

## Package Structure

The package is organized into three main categories:

```
@putnami/utils/
├── Shared utilities (browser + server)
│   ├── Object manipulation
│   ├── Array operations
│   ├── Type checking
│   ├── URL handling
│   └── Task processing
└── Server-only utilities
    ├── Path operations
    ├── Workspace management
    └── Stack trace utilities
```

## Understanding Conditional Exports

The package uses conditional exports to provide different builds for browser and server environments:

- **Browser builds**: Only include utilities that don't depend on server-specific modules
- **Server builds**: Include all utilities, including file system and workspace operations

See [Browser vs Server](./browser-vs-server.md) for detailed information about which utilities are available in each environment.

## Common Use Cases

### Working with Nested Objects

```typescript
import { getDeep, mergeDeep } from '@putnami/utils';

const config = {
  database: {
    host: 'localhost',
    port: 5432,
  },
};

// Access nested properties
const host = getDeep(config, 'database.host'); // 'localhost'

// Deep merge configurations
const defaults = { database: { host: 'localhost', port: 5432 } };
const overrides = { database: { port: 3306 } };
const merged = mergeDeep(defaults, overrides);
// { database: { host: 'localhost', port: 3306 } }
```

### Type Checking

```typescript
import { isString, isPlainObject, isNullish } from '@putnami/utils';

function processValue(value: unknown) {
  if (isNullish(value)) {
    return 'Value is missing';
  }

  if (isString(value)) {
    return value.toUpperCase();
  }

  if (isPlainObject(value)) {
    return JSON.stringify(value);
  }

  return String(value);
}
```

### URL Building

```typescript
import { buildUrlWithParams, parseQueryString } from '@putnami/utils';

// Build URLs with parameters
const url = buildUrlWithParams('https://api.example.com/users/[id]', {
  id: '123',
  page: 1,
  limit: 10,
});
// 'https://api.example.com/users/123?page=1&limit=10'

// Parse query strings
const params = parseQueryString('page=1&limit=10');
// { page: '1', limit: '10' }
```

### Concurrent Task Processing

```typescript
import { TaskQueue } from '@putnami/utils';

// Create a queue with max 3 concurrent tasks
const queue = new TaskQueue(3);

// Process multiple tasks with concurrency control
const results = await Promise.all([
  queue.process(() => fetch('/api/user/1').then(r => r.json())),
  queue.process(() => fetch('/api/user/2').then(r => r.json())),
  queue.process(() => fetch('/api/user/3').then(r => r.json())),
  queue.process(() => fetch('/api/user/4').then(r => r.json())), // Waits for a slot
]);
```

## Next Steps

- Learn about [Browser vs Server](./browser-vs-server.md) exports
- Explore the complete [API Reference](./api-reference.md)
- Check out the [README](../README.md) for migration from old function names
