# Plugins & Lifecycle

Putnami applications are composed from plugins. Each plugin can hook into a lifecycle that spans build time and runtime, making it easy to extend the framework with custom functionality.

## Plugin interface

A plugin is an object that implements one or more lifecycle hooks:

```ts
import type { Module, Plugin, GenerateResult } from '@putnami/application';
import type { ReadyEndpoint } from '@putnami/runtime/jobs';

interface Plugin {
  generate?(owner: Module): Promise<GenerateResult> | GenerateResult;
  warmup?(owner: Module): Promise<void> | void;
  start?(owner: Module): Promise<void> | void;
  startupCompleted?(): void;
  readyEndpoints?(): ReadyEndpoint[];
  stop?(owner: Module): Promise<void> | void;
}
```

Plugins receive their owning `Module` (or `Application`, which extends `Module`) as the first argument. This lets plugins resolve other plugins or access module-level configuration.

## Lifecycle hooks

### generate()

Runs at build time during `putnami build`. Use this for:

- Code generation
- Asset compilation
- Route discovery
- Type generation

```ts
import type { Module, Plugin, GenerateResult } from '@putnami/application';
import { writeFileSync } from 'fs';

export function routeTypesPlugin(): Plugin {
  return {
    async generate(owner: Module): Promise<GenerateResult> {
      // Generate TypeScript types for routes
      const routes = discoverRoutes();
      const types = generateRouteTypes(routes);

      writeFileSync('.gen/routes.d.ts', types);

      return {
        files: ['.gen/routes.d.ts'],
      };
    },
  };
}
```

### warmup()

Runs before the application starts, after all plugins are loaded. Use this for:

- Route registration
- Cache preloading
- Connection pooling setup
- Dependency initialization

```ts
export function cachePlugin(): Plugin {
  const cache = new Map<string, unknown>();

  return {
    async warmup(owner: Module) {
      // Preload frequently accessed data
      const data = await fetchInitialData();
      for (const [key, value] of Object.entries(data)) {
        cache.set(key, value);
      }
      console.log(`Cache warmed with ${cache.size} entries`);
    },
  };
}
```

### start()

Runs when the application starts. Use this for:

- Starting HTTP servers
- Opening database connections
- Starting background workers
- Initializing external services

```ts
export function metricsPlugin(): Plugin {
  let metricsServer: Server;

  return {
    async start(owner: Module) {
      // Start a separate metrics server
      metricsServer = Bun.serve({
        port: 9090,
        fetch(req) {
          return new Response(getMetrics(), {
            headers: { 'Content-Type': 'text/plain' },
          });
        },
      });
      console.log('Metrics server started on port 9090');
    },

    async stop() {
      metricsServer?.stop();
    },
  };
}
```

### startupCompleted() and the ready record

Every plugin `start()` runs in parallel. Once all of them resolved, the
application calls `startupCompleted()` on every plugin, in plugin order, then
logs `🤖 ready`. That record carries a runtime-protocol `workload` readiness
claim under the reserved `putnami.ready` key, with the endpoints every plugin
reports from `readyEndpoints()` (the HTTP plugin reports its listener). An
endpoint the runtime protocol rejects, such as port 0, is dropped with a
warning rather than voiding the claim. The
platform plugin implements `startupCompleted()`: its `/readyz` answers ready
only from then on. A `start()` that is still pending delays both, and a
`start()` that rejects means neither happens. `startupCompleted()` must not
block.

### stop()

Runs during graceful shutdown (in reverse order of plugin registration). Use this for:

- Closing connections
- Flushing buffers
- Cleanup operations
- Releasing resources

```ts
export function databasePlugin(): Plugin {
  let connection: DatabaseConnection;

  return {
    async start() {
      connection = await createConnection();
    },

    async stop() {
      // Gracefully close the database connection
      await connection?.close();
      console.log('Database connection closed');
    },
  };
}
```

## Creating custom plugins

### Basic plugin

```ts
import type { Plugin } from '@putnami/application';

export function myPlugin(): Plugin {
  return {
    async warmup() {
      console.log('My plugin is warming up');
    },

    async start() {
      console.log('My plugin started');
    },

    async stop() {
      console.log('My plugin stopped');
    },
  };
}

// Usage
const app = application()
  .use(myPlugin());
```

### Plugin with configuration

```ts
import type { Plugin } from '@putnami/application';

interface LoggerPluginOptions {
  level: 'debug' | 'info' | 'warn' | 'error';
  prefix?: string;
}

export function loggerPlugin(options: LoggerPluginOptions): Plugin {
  const { level, prefix = '[APP]' } = options;

  return {
    async warmup() {
      console.log(`${prefix} Logger initialized at ${level} level`);
    },
  };
}

// Usage
const app = application()
  .use(loggerPlugin({ level: 'info', prefix: '[MyApp]' }));
```

### Plugin with state

```ts
import type { Plugin } from '@putnami/application';
import { json } from '@putnami/application';

export function rateLimiterPlugin(maxRequests: number, windowMs: number): Plugin {
  const requests = new Map<string, number[]>();

  const isRateLimited = (ip: string): boolean => {
    const now = Date.now();
    const windowStart = now - windowMs;

    const timestamps = requests.get(ip) || [];
    const recent = timestamps.filter(t => t > windowStart);

    if (recent.length >= maxRequests) {
      return true;
    }

    recent.push(now);
    requests.set(ip, recent);
    return false;
  };

  return {
    async warmup(owner: Module) {
      // Access other plugins if needed
      const httpPlugin = owner.getPlugin(HttpPlugin);

      httpPlugin.prepend(async (ctx, next) => {
        const ip = ctx.headers.get('x-forwarded-for') || 'unknown';

        if (isRateLimited(ip)) {
          return json(
            { error: 'Too many requests' },
            { status: 429 }
          );
        }

        return next();
      });
    },
  };
}
```

### Plugin class

For complex plugins, use a class:

```ts
import type { Module, Plugin } from '@putnami/application';
import { json } from '@putnami/application';

export class HealthCheckPlugin implements Plugin {
  private checks: Map<string, () => Promise<boolean>> = new Map();

  addCheck(name: string, check: () => Promise<boolean>) {
    this.checks.set(name, check);
  }

  async warmup(owner: Module) {
    const httpPlugin = owner.getPlugin(HttpPlugin);

    httpPlugin.get('/health', async () => {
      const results: Record<string, boolean> = {};

      for (const [name, check] of this.checks) {
        try {
          results[name] = await check();
        } catch {
          results[name] = false;
        }
      }

      const healthy = Object.values(results).every(Boolean);

      return json(
        { healthy, checks: results },
        { status: healthy ? 200 : 503 }
      );
    });
  }
}

// Usage
const healthCheck = new HealthCheckPlugin();
healthCheck.addCheck('database', async () => {
  return await database().query('SELECT 1');
});

const app = application()
  .use(healthCheck);
```

## Accessing other plugins

### Get a registered plugin

```ts
import { HttpPlugin } from '@putnami/application';

export function myPlugin(): Plugin {
  return {
    async warmup(owner: Module) {
      // Get an existing plugin (throws if not found)
      const httpPlugin = owner.getPlugin(HttpPlugin);

      // Register routes
      httpPlugin.get('/my-route', () => ({ ok: true }));
    },
  };
}
```

### Ensure a plugin exists

```ts
export function myPlugin(): Plugin {
  return {
    async warmup(owner: Module) {
      // Get or wait for a plugin to be ready
      const httpPlugin = await owner.ensurePlugin(HttpPlugin);

      httpPlugin.get('/my-route', () => ({ ok: true }));
    },
  };
}
```

### List all plugins

```ts
export function debugPlugin(): Plugin {
  return {
    async warmup(owner: Module) {
      const plugins = owner.getPlugins();
      console.log(`Loaded ${plugins.length} plugins`);
    },
  };
}
```

## Module composition

Modules let you group related plugins into reusable, composable units. The hierarchy is **Application > Module > Plugin**: applications and modules can contain plugins and sub-modules, but only an Application has a startable lifecycle.

### Creating a module

```ts
import { module } from '@putnami/application';

const authModule = module('auth')
  .path('/auth')
  .use(oAuth2())
  .use(api());
```

### Module path

Use `.path()` to set a base path for the module. Child plugins (`api()`, `react()`, `staticFiles()`) automatically inherit this path as their route prefix:

```ts
import { module, api } from '@putnami/application';
import { react } from '@putnami/web';

const tasksModule = module('tasks')
  .path('/tasks')         // All child routes prefixed with /tasks
  .use(api())             // API routes: /tasks, /tasks/[id], etc.
  .use(react());          // React pages: /tasks, /tasks/new, etc.
```

If a plugin specifies an explicit `prefix`, it takes priority over the module path.

### Module security

Use `.secure()` to protect all routes registered by plugins in the module:

```ts
const dashboard = module('dashboard')
  .path('/dashboard')
  .secure()                      // Require authentication
  .use(api());

const admin = module('admin')
  .path('/admin')
  .secure({ roles: ['admin'] })  // Require admin role
  .use(api());
```

### Composing modules

```ts
import { application, http } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(tasksModule)
  .use(authModule);

await app.start();
```

When the application starts, plugins from all modules are collected depth-first and executed in registration order. During shutdown, plugins are stopped in reverse order.

### Nested modules

Modules can contain other modules:

```ts
const inner = module('inner').use(somePlugin());
const outer = module('outer').use(inner).use(anotherPlugin());

const app = application().use(outer);
```

### Plugin resolution across modules

Plugins can find other plugins by walking up the module hierarchy. This means a plugin registered on a parent module is visible to all child modules:

```ts
export function myPlugin(): Plugin {
  return {
    async warmup(owner) {
      // Searches owner's plugins first, then walks up to parent modules
      const http = owner.getPlugin(HttpPlugin);
      http.get('/my-route', () => ({ ok: true }));
    },
  };
}
```

### Modules with DI providers

Modules can declare DI providers and requirements:

```ts
const dbModule = module('db')
  .provide(DatabaseService)
  .use(sql());

const authModule = module('auth')
  .require(DatabaseService)
  .provide(AuthService, { deps: [DatabaseService] })
  .use(oAuth2());
```

When sibling feature modules need to satisfy each other's DI requirements, wrap
them with `composeModules()`. Their plugins remain discoverable as children, but
their providers mount as one composed DI unit:

```ts
const authModule = composeModules([authUsers(), authCodes(), authOAuthClients()], {
  name: 'auth.server',
});
```

## Application lifecycle

### Creating an application

Use the `application()` factory for fluent composition, or instantiate `new Application()` directly when needed:

```ts
import { application } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(myPlugin());
```

### Running an application

```ts
const app = application()
  .use(http({ port: 3000 }))
  .use(myPlugin());

// Start the application
await app.start();

// Check if running
console.log(app.isRunning()); // true

// Stop the application
await app.stop();
```

### One-shot jobs

For scripts that run once and exit:

```ts
const job = application()
  .use(sql())
  .run(async () => {
    // This runs after warmup, before start
    await migrateDatabase();
    console.log('Migration complete');
  });

await job.start();
// Application exits after the run callback completes
```

### Shutdown hooks

```ts
const app = application()
  .use(http())
  .onStop(async () => {
    // Custom shutdown logic
    await flushLogs();
    await closeConnections();
  });
```

## Plugin ordering

Plugins are executed in registration order for `generate`, `warmup`, and `start`. For `stop`, they execute in reverse order:

```ts
const app = application()
  .use(pluginA())  // warmup: 1st, stop: 3rd
  .use(pluginB())  // warmup: 2nd, stop: 2nd
  .use(pluginC()); // warmup: 3rd, stop: 1st
```

## Build vs runtime

### Build-only plugins

Some plugins only run at build time:

```ts
export function typeGenPlugin(): Plugin {
  return {
    async generate(app) {
      // Only runs during `putnami build`
      await generateTypes();
      return { files: ['.gen/types.ts'] };
    },
    // No runtime hooks
  };
}
```

### Runtime-only plugins

Some plugins only run at runtime:

```ts
export function requestLoggerPlugin(): Plugin {
  return {
    async warmup(owner) {
      const http = owner.getPlugin(HttpPlugin);
      http.use(async (ctx, next) => {
        const start = Date.now();
        const response = await next();
        console.log(`${ctx.method} ${ctx.path()} - ${Date.now() - start}ms`);
        return response;
      });
    },
  };
}
```

## Related guides

- [Overview](/docs/frameworks/typescript/overview)
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware)

## Support and compatibility

`@putnami/application` is a public, documented, maintained package classified
`stable`. Its application-lifecycle specification and its accepted
build/runtime phase ADR live next to the package source. The promise is that
the two phases stay disjoint — `build()` generates artifacts and never opens a
runtime resource, `start()` opens resources and never generates a file — and
that a failed startup stops only the plugins that started, closes the container,
and re-throws to its caller instead of ending the host process.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
