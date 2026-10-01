# @putnami/application

A Bun-native application framework with plugin architecture for building HTTP services, APIs, and static file servers.

## Installation

```bash
bun add @putnami/application
```

Or using Putnami CLI:

```bash
putnami add @putnami/application
```

## Features

- **Plugin Architecture** - Extensible plugin system with lifecycle hooks
- **HTTP Server** - Built-in HTTP server with routing, middleware, and WebSocket support
- **File-based Routing** - Automatic route discovery from file structure
- **Session Management** - Pluggable session storage (cookie, memory, database)
- **OAuth2 Authentication** - Built-in OAuth2 flows with JWT verification
- **Static File Serving** - Automatic compression, caching, and MIME detection
- **Configuration Management** - YAML-based config with environment support
- **Type-safe APIs** - Full TypeScript support throughout

## Table of Contents

- [Quick Start](#quick-start)
- [Core Concepts](#core-concepts)
- [Documentation](#documentation)
- [API Reference](#api-reference)
- [Examples](#examples)

## Quick Start

### Basic HTTP Server

```typescript
// src/main.ts
import { application, http } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .run(async (app) => {
    const httpPlugin = app.getPlugin(HttpPlugin);

    httpPlugin.get('/hello', () => {
      return { message: 'Hello, World!' };
    });
  });

await app.start();
```

### File-based API Routes

```typescript
// src/main.ts
import { application, http, api } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api());  // Scans ./api folder for routes

await app.start();
```

Create route handlers:

```typescript
// src/api/users/get.ts
import type { HttpRequestContext } from '@putnami/application';

export function GET(ctx: HttpRequestContext) {
  return { users: [] };
}
```

> **📚 Learn More:** See [Getting Started Guide](doc/getting-started.md) for a complete walkthrough.

## Core Concepts

### Application Lifecycle

The `application()` factory creates an application that orchestrates plugins through a well-defined lifecycle:

```typescript
const app = application()
  .use(http())         // Register plugins
  .use(api())
  .run(async () => {   // Optional startup logic
    console.log('Ready!');
  });

await app.start();     // Starts all plugins
await app.stop();      // Graceful shutdown
```

### Plugin Lifecycle

Plugins implement optional lifecycle hooks:

| Hook | When Called | Use Case |
|------|-------------|----------|
| `generate()` | Build time (`putnami build`) | Code generation, route discovery |
| `warmup()` | Before start | Plugin initialization, route registration |
| `start()` | After warmup | Start servers, subscribe to queues |
| `stop()` | Shutdown | Cleanup resources, close connections |

> **📚 Learn More:** See [Plugins Guide](doc/plugins.md) for plugin architecture details.

### HTTP Routing

Register routes manually or use file-based routing:

```typescript
// Manual registration
httpPlugin.get('/users', () => ({ users: [] }));
httpPlugin.post('/users', async (ctx) => {
  const body = await ctx.body<{ name: string }>();
  return { created: body };
});

// File-based (recommended)
// src/api/users/get.ts
export function GET(ctx: HttpRequestContext) {
  return { users: [] };
}
```

> **📚 Learn More:** See [HTTP Server Guide](doc/http-server.md) and [File-based Routing](doc/file-based-routing.md) for details.

## Documentation

Comprehensive guides to help you build applications:

- **[Getting Started](doc/getting-started.md)** - Installation, setup, and your first app
- **[Configuration](doc/configuration.md)** - Complete configuration reference for all plugins
- **[Plugins](doc/plugins.md)** - Plugin architecture and creating custom plugins
- **[HTTP Server](doc/http-server.md)** - Routing, middleware, and request handling
- **[File-based Routing](doc/file-based-routing.md)** - Automatic route discovery
- **[Sessions](doc/sessions.md)** - Session management with multiple storage backends
- **[OAuth](doc/oauth.md)** - OAuth2 authentication flows
- **[Static Files](doc/static-files.md)** - Serving static files with caching
- **[WebSockets](doc/websockets.md)** - Real-time communication
- **[API Reference](doc/api-reference.md)** - Complete API documentation

## API Reference

### Application

Main application class that orchestrates plugins.

| Method | Description |
|--------|-------------|
| `use(plugin)` | Register a plugin |
| `run(fn)` | Set startup runner |
| `onStop(fn)` | Register shutdown hook |
| `getPlugin(Type)` | Get plugin by type (sync) |
| `ensurePlugin(Type)` | Get or create plugin (async) |
| `start()` | Start application |
| `stop()` | Stop application |

### HTTP Plugin

HTTP server with routing and middleware.

```typescript
import { http, HttpPlugin } from '@putnami/application';

const httpPlugin = http({ port: 3000 });

httpPlugin.get('/users', handler);
httpPlugin.post('/users', handler);
httpPlugin.use(middleware);
```

### API Plugin

File-based route discovery.

```typescript
import { api } from '@putnami/application';

const app = application()
  .use(api({ prefix: '/api' }));
```

### Static Files Plugin

Serve static files with caching and compression.

```typescript
import { staticFiles } from '@putnami/application';

const app = application()
  .use(staticFiles({ prefix: '/assets' }));
```

> **📚 Full API:** See [API Reference](doc/api-reference.md) for complete documentation.

## Examples

### REST API

```typescript
// src/api/users/get.ts
export function GET(ctx: HttpRequestContext) {
  return { users: [] };
}

// src/api/users/post.ts
import { json } from '@putnami/application';

export async function POST(ctx: HttpRequestContext) {
  const body = await ctx.body<{ name: string }>();
  return json({ created: body }, { status: 201 });
}

// src/api/users/[id]/get.ts
export function GET(ctx: HttpRequestContext) {
  const id = ctx.params?.id;
  return { user: { id } };
}
```

### OAuth2 Authentication

```typescript
import { application, http, oAuth2, unauthorized } from '@putnami/application';
import { useUser, useOAuthService } from '@putnami/application';

const app = application()
  .use(http())
  .use(oAuth2());

// Protected route
export async function GET(ctx: HttpRequestContext) {
  const user = await useUser();
  if (!user) {
    return unauthorized();
  }
  const oauth = useOAuthService();
  const token = await oauth.clientToken();
  return { user };
}
```

### Session Management

```typescript
import { setSession, useSession } from '@putnami/application';

export async function POST(ctx: HttpRequestContext) {
  const { userId } = await ctx.body<{ userId: string }>();
  setSession('userId', userId);
  return { success: true };
}

export function GET(ctx: HttpRequestContext) {
  const userId = useSession<string>('userId');
  return { userId };
}
```

### WebSocket

```typescript
// src/api/chat/ws.ts
import type { WebSocketContext } from '@putnami/application';

export function OPEN(ctx: WebSocketContext) {
  console.log('Client connected');
}

export function MESSAGE(ctx: WebSocketContext) {
  ctx.ws.send(`Echo: ${ctx.message}`);
}

export function CLOSE(ctx: WebSocketContext) {
  console.log('Client disconnected');
}
```

> **📚 More Examples:** See the documentation guides for detailed examples.

## Configuration

Configure plugins via constructor options or YAML:

```typescript
// Constructor options
const app = application()
  .use(http({ port: 3000 }))
  .use(api({ prefix: '/api' }));
```

```yaml
# conf/.env.local.yaml
server:
  port: 3000
api:
  prefix: '/api'
session:
  store: 'cookie'
  cookieSecret: '...'
```

> **📚 Full Guide:** See [Configuration](doc/configuration.md) for all options.

## CLI Commands

```bash
# Serve application (dev mode)
putnami serve .

# Build for production
putnami build .

# Run tests
putnami test .
```

## Dependencies

This package relies on the following key dependencies:

- **Bun** - Native HTTP server and runtime
- **[@putnami/runtime](../runtime/README.md)** - Dependency injection, typed configuration, context, and logging
- **[@putnami/utils](../utils/README.md)** - Shared schema vocabulary and server utilities
- **[@putnami/migration](../migration/README.md)** - Migration sources collected during build and startup

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). What that promises is the
behavior stated in the
[application-lifecycle specification](specs/application-lifecycle.json), the
[server-streams specification](specs/server-streams.json), the
[api-contracts specification](specs/api-contracts.json), the
[build/runtime phase ADR](doc/adr/0001-build-and-runtime-are-disjoint-phases.md),
the
[server-stream admission ADR](doc/adr/0002-server-stream-admission-is-the-response-head.md)
and the
[WebSocket admission ADR](doc/adr/0003-websocket-admission-runs-the-security-chain-on-the-rebuilt-request.md):
build generation and the runtime lifecycle are disjoint, a failed startup
releases exactly what it acquired and is re-thrown to its caller, and shutdown
drains on the framework's own deadline.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
