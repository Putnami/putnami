# Getting Started

This guide will help you set up your first application with `@putnami/application` and understand the core concepts.

## Prerequisites

- [Bun](https://bun.sh/) v1.4.0 or higher
- Basic knowledge of TypeScript
- Familiarity with HTTP servers and REST APIs (helpful but not required)

## Installation

```bash
bun add @putnami/application
```

Or using Putnami CLI:

```bash
putnami add @putnami/application
```

## Project Setup

### 1. Create Your Application Entry Point

Create `src/main.ts`:

```typescript
import { application, http, api, staticFiles } from '@putnami/application';

export const app = () => application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(staticFiles())
  .run(async () => {
    console.log('Server ready at http://localhost:3000');
  });
```

### 2. Run Your Application

```bash
putnami serve .
```

Or directly with Bun:

```bash
bun run src/main.ts
```

Visit `http://localhost:3000` to see your app!

## Understanding the File Structure

The Application framework uses a plugin-based architecture. Here's a typical project structure:

```
src/
├── main.ts              # Application entry point
├── api/                 # File-based API routes (scanned by api plugin)
│   ├── users/
│   │   ├── get.ts      # GET /users
│   │   └── [id]/
│   │       └── get.ts  # GET /users/:id
│   └── health/
│       └── get.ts      # GET /health
├── public/             # Static files (served by staticFiles plugin)
│   ├── index.html
│   └── styles.css
└── conf/               # Configuration files
    ├── .env.local.yaml
    └── .env.prod.yaml
```

## Core Concepts

### Application Lifecycle

The `application()` factory creates an application that orchestrates plugins and manages the lifecycle:

```typescript
import { application } from '@putnami/application';

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

Plugins implement optional lifecycle hooks that are called in a specific order:

| Hook | When Called | Use Case |
|------|-------------|----------|
| `generate()` | Build time (`putnami build`) | Code generation, route discovery |
| `warmup()` | Before start | Plugin initialization, route registration |
| `start()` | After warmup | Start servers, subscribe to queues |
| `stop()` | Shutdown | Cleanup resources, close connections |

### Basic HTTP Route

Create a simple route handler using the `endpoint()` builder:

```typescript
// src/api/hello/get.ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  return { message: 'Hello, World!' };
});
```

This automatically creates a `GET /hello` route that returns JSON.

For routes that need input validation, chain builder methods:

```typescript
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    // ctx.params.id is typed and validated as a UUID
    return { userId: ctx.params.id };
  });
```

See [Endpoint Builder](endpoint-builder.md) for the full guide.

### Manual Route Registration

You can also register routes manually:

```typescript
import { application, http, HttpPlugin } from '@putnami/application';

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

## Adding Data Loading

Route handlers receive a context object with request information:

```typescript
// src/api/users/get.ts
import type { HttpRequestContext } from '@putnami/application';

export async function GET(ctx: HttpRequestContext) {
  // Parse query parameters
  const { page = '1', limit = '10' } = ctx.queryParams();

  // Access request headers
  const userAgent = ctx.headers.get('user-agent');

  // Parse JSON body (for POST/PUT)
  // const body = await ctx.body<{ name: string }>();

  return {
    users: [],
    page: parseInt(page, 10),
    limit: parseInt(limit, 10),
  };
}
```

## Adding POST Routes

Handle form submissions and API requests with built-in validation:

```typescript
// src/api/users/post.ts
import { endpoint, Email } from '@putnami/application';

export default endpoint()
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const { name, email } = await ctx.body();
    // name and email are validated — missing or invalid values
    // are rejected with a 400 before this code runs

    const user = { id: '1', name, email };
    return user;
  });
```

## Development vs Production

### Development Mode

In development, the framework automatically:
- Scans for route changes and regenerates routes
- Provides detailed error messages
- Enables hot reloading (with Bun)

### Production Mode

For production, you should:

1. **Build your application:**
   ```bash
   putnami build .
   ```

2. **Set environment variables:**
   ```bash
   NODE_ENV=production bun run src/main.ts
   ```

3. **Configure production settings:**
   ```typescript
   // src/main.ts
   import { application, http } from '@putnami/application';

   export const app = () => application()
     .use(http({
       port: process.env.PORT || 3000,
     }))
     .run(async () => {
       console.log('Production server ready');
     });
   ```

## Project Structure Recommendations

Here's a recommended structure for a production app:

```
src/
├── main.ts                 # Application entry
├── api/                    # API routes (scanned by api plugin)
│   ├── users/
│   │   ├── get.ts
│   │   ├── post.ts
│   │   └── [id]/
│   │       ├── get.ts
│   │       ├── put.ts
│   │       └── delete.ts
│   └── health/
│       └── get.ts
├── public/                 # Static files
│   ├── index.html
│   ├── favicon.ico
│   └── assets/
├── lib/                    # Utilities and helpers
│   ├── db.ts
│   └── auth.ts
├── conf/                   # Configuration files
│   ├── .env.local.yaml
│   ├── .env.test.yaml
│   └── .env.prod.yaml
└── types/                  # TypeScript types
    └── index.ts
```

## Next Steps

- Read the [Endpoint Builder](endpoint-builder.md) guide for type-safe, validated routes
- Learn about [Plugins](plugins.md) to understand the plugin architecture
- Explore [HTTP Server](http-server.md) for routing and middleware
- Read about [File-based Routing](file-based-routing.md) for automatic route discovery
- Check out [Configuration](configuration.md) for customization options
- See [API Reference](api-reference.md) for complete API documentation

## Common Questions

**Q: Do I need to configure routing manually?**
A: No! The `api()` plugin automatically discovers routes from your file structure. You can also use manual configuration if needed.

**Q: How do I add middleware?**
A: Use the HTTP plugin's middleware system. See [HTTP Server](http-server.md#middleware) for details.

**Q: Can I use this with React or other frontend frameworks?**
A: Yes! The Application framework is backend-agnostic. Use `@putnami/web` for React SSR, or serve any frontend framework via static files.

**Q: How do I handle authentication?**
A: Use the [OAuth](oauth.md) plugin for OAuth2 flows, or implement custom authentication with [Sessions](sessions.md).
