# Plugins

Understanding the plugin architecture and how to create custom plugins.

## Plugin Overview

The Application framework uses a plugin-based architecture. Plugins extend functionality and hook into the application lifecycle.

## Plugin Lifecycle

Plugins implement optional lifecycle hooks that are called in a specific order:

```
generate() → postGenerate() → warmup() → migrate() → start() → stop()
```

### Lifecycle Hooks

| Hook | When Called | Use Case |
|------|-------------|----------|
| `generate()` | Build time (`putnami build`) | Code generation, route discovery |
| `postGenerate()` | After all generation completes | Consume loaders or artifacts emitted by other plugins |
| `warmup()` | Before start | Plugin initialization, route registration |
| `migrate()` | After warmup | Explicit migration-runner hooks |
| `start()` | After warmup | Start servers, subscribe to queues |
| `stop()` | Shutdown | Cleanup resources, close connections |

### Capability manifest contributions

By default, `Application.build()` emits `.gen/schema/capabilities.json`. The built-in
producer discovers config, migrations and database requirements, health and
readiness probes, named lifecycle hooks, and the server loader modules emitted
by the application hook. After every dependency hook has run, the TypeScript
extension reconciles the merged export map — including loaders the producer
never saw, such as `@putnami/web`'s `react-loader` — into that manifest.
The producer publishes protocol v2 with canonical contribution identities and
stable declaration/artifact provenance. It does not copy the scheduler's
volatile source binding into `capabilities.json`; indexing systems compute
integrity against the revision they selected. A complete scheduler
`sourceRoot`/`sourceBinding` stamp remains required for generated feature
evidence. During a rolling CLI upgrade, a complete
legacy stamp publishes only the protocol-v1 activation contract (and no feature
evidence), so bundled loader/config behavior remains available without
inventing v2 provenance. Partially present or malformed binding metadata fails
closed.

New features belong on their native module with `.feature(...)`. API, event,
data, migration, DI, and generated-client plugins beneath that module populate
the disposable design graph automatically. Capability provenance is derived
from those registrations and generated artifacts. Use `DesignContributor` only when no native registration
can expose the technical fact. Application composition and runtime lifecycle
never read either graph or feature metadata.

To let one of those facts earn maturity, declare the association on the feature
itself: `proves: [{ requirement, contribution }]`. The requirement must already
exist in the project's `putnami.features.json` and accept `capability`
evidence; describe reads its stage from there, computes the whole record —
ID, issuer, source binding, provenance, subject — and writes it under
`.gen/schema/feature-evidence/`. A contribution nobody proves stays
`unclassified` in `putnami features validate`; never hand-write an evidence
fragment to clear one. See
[ADR 0003](../../../../protocols/features/doc/adr/0003-generated-feature-evidence.md).

Only plugins reachable from the module tree receive a builder, so a plugin that
composes another plugin privately — a controller holding `events()` or `sql()`
as a field rather than mounting it — would silently drop that inner plugin's
native contributions. Implement `DesignDelegate` instead of restating those
facts in a wrapper:

```typescript
import type { DesignDelegate, Plugin } from '@putnami/application';

class RelayController implements Plugin, DesignDelegate {
  private readonly inner = events({ outboxes: [IdentityOutbox] });

  designDelegates() {
    return [this.inner];
  }
}
```

Delegates contribute under the same owning module and feature scope as the
plugin that declares them, transitively and cycle-safe. The seam is read only
while the design graph is built: it changes no plugin lifecycle, hook ordering,
or dependency injection.

The application hook itself runs last (`hooks.preBuild.order: 100` in
`putnami.extension.json`): it imports the workload entry point, which may import
a module another extension generates into `.gen`, so every generating hook must
have run first. Importable `*-loader` exports are recorded as route-schema
entries when they own routes, or as generated source discoverers for non-route
modules such as SQL table/event-handler discovery. Both carry project-relative
activation module paths. Conflicting keys/paths or an invalid manifest fail the
build before the artifact is atomically republished.

Build callers that are not the scheduler's generate step can pass
`{ publishCapabilityManifest: false }` to run generation and import generated
loaders without mutating that build-owned manifest. `createTestApp` uses this mode so test execution cannot
clobber a reconciled manifest after generation finishes.

The TypeScript extension consumes those manifest entries when it generates the
bundled serve entrypoint. It normalizes v1 and v2 manifests into only the loader
and config activation fields, ignoring feature evidence and descriptive
provenance. It statically imports and registers each module before
the application is constructed, registers dependency `ConfigContributor`
definitions from the resulting graph, and verifies that every manifest config
path has a real runtime definition before building the DI container. Packaged
workloads therefore do not need `.gen/src` on disk or a hand-written config
registrar. The manifest's flattened config field metadata is never used to
fabricate executable schema validators.

`start()` and `stop()` alone do not carry stable hook names. A plugin that wants
those resources represented in the manifest implements `LifecycleContributor`:

```typescript
import type { LifecycleContributor, Plugin } from '@putnami/application';

const connectionPool: Plugin & LifecycleContributor = {
  lifecycleContributions: () => [
    { name: 'connectionPool', phase: 'starter' },
    { name: 'connectionPool', phase: 'stopper' },
  ],
};
```

A plugin can also require a complete provider set. Generation fails closed and
does not publish a manifest when a required kind is missing or a logical
provider is duplicated/conflicts:

```typescript
import type { Plugin, RequiredCapabilityContributor } from '@putnami/application';

const sqlFeature: Plugin & RequiredCapabilityContributor = {
  requiredCapabilities: () => [
    { name: 'sql', requires: ['datasource', 'migration', 'readiness'] },
  ],
};
```

Failures use stable `capabilities.missing_required_provider`,
`capabilities.duplicate_provider`, or `capabilities.conflicting_provider`
diagnostic codes.
Migration providers are scoped by `(kind, namespace)`, so different migration
kinds may share a namespace. Invalid runtime contributor values—including an
empty `requires` list or an unknown capability kind—also fail before the
manifest is published.

## Plugin Interface

All plugins implement the `Plugin` interface:

```typescript
import type { Module, Plugin, GenerateResult } from '@putnami/application';

export interface Plugin {
  generate?(owner: Module): Promise<GenerateResult>;
  postGenerate?(owner: Module, generated?: GenerateResult): Promise<GenerateResult>;
  warmup?(owner: Module): Promise<void>;
  migrate?(owner: Module): Promise<void>;
  start?(owner: Module): Promise<void>;
  stop?(owner: Module): Promise<void>;
}
```

## Creating a Custom Plugin

### Basic Plugin

Here's a simple plugin that logs when the application starts:

```typescript
import type { Module, Plugin } from '@putnami/application';

class LoggerPlugin implements Plugin {
  async warmup(owner: Module): Promise<void> {
    console.log('Logger plugin warming up...');
  }

  async start(owner: Module): Promise<void> {
    console.log('Logger plugin started');
  }

  async stop(owner: Module): Promise<void> {
    console.log('Logger plugin stopped');
  }
}

// Usage
const app = application()
  .use(new LoggerPlugin())
  .run(async () => {
    console.log('Application ready');
  });
```

### Plugin with Configuration

Plugins can accept configuration:

```typescript
import type { Module, Plugin } from '@putnami/application';

interface MyPluginConfig {
  enabled: boolean;
  prefix: string;
}

class MyPlugin implements Plugin {
  constructor(private config: MyPluginConfig) {}

  async warmup(owner: Module): Promise<void> {
    if (!this.config.enabled) {
      return;
    }
    console.log(`MyPlugin warming up with prefix: ${this.config.prefix}`);
  }
}

// Factory function
export function myPlugin(config: MyPluginConfig): MyPlugin {
  return new MyPlugin(config);
}

// Usage
const app = application()
  .use(myPlugin({ enabled: true, prefix: '/api' }));
```

### Plugin with Code Generation

Plugins can generate code during the build phase:

```typescript
import type { Module, Plugin, GenerateResult } from '@putnami/application';
import { writeFileSync, mkdirSync } from 'node:fs';
import { joinPath } from '@putnami/utils';

class CodeGenPlugin implements Plugin {
  async generate(owner: Module): Promise<GenerateResult> {
    const outputDir = '.gen/src';
    mkdirSync(outputDir, { recursive: true });

    const code = `
export const generatedConstant = 'Hello from generated code!';
`;

    writeFileSync(joinPath(outputDir, 'generated.ts'), code);

    return {
      exports: {
        './generated': './.gen/src/generated.ts',
      },
    };
  }
}
```

### Plugin with Dependencies

Plugins can depend on other plugins using `ensurePlugin()`:

```typescript
import type { Module, Plugin } from '@putnami/application';
import { HttpPlugin } from '@putnami/application';

class MyHttpPlugin implements Plugin {
  async warmup(owner: Module): Promise<void> {
    // Ensure HttpPlugin exists, create if missing
    const httpPlugin = await owner.ensurePlugin(HttpPlugin);

    // Register routes
    httpPlugin.get('/my-route', () => {
      return { message: 'Hello from MyHttpPlugin!' };
    });
  }
}
```

### Plugin with Synchronous Dependencies

Use `getPlugin()` for plugins that must already exist:

```typescript
import type { Module, Plugin } from '@putnami/application';
import { HttpPlugin } from '@putnami/application';

class DependentPlugin implements Plugin {
  async warmup(owner: Module): Promise<void> {
    // Get existing plugin (throws if not found)
    const httpPlugin = owner.getPlugin(HttpPlugin);

    // Use the plugin
    httpPlugin.get('/dependent-route', () => {
      return { message: 'Hello!' };
    });
  }
}

// HttpPlugin must be registered first
const app = application()
  .use(http())
  .use(new DependentPlugin());
```

## Built-in Plugins

### HTTP Plugin

Provides HTTP server functionality:

```typescript
import { http, HttpPlugin } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }));
```

See [HTTP Server](http-server.md) for details.

### API Plugin

File-based route discovery. Routes mount at the root by default — the scan folder name (`api`) is for file organisation only, not a URL prefix:

```typescript
import { api } from '@putnami/application';

const app = application()
  .use(api());  // src/api/users/get.ts → GET /users
```

See [File-based Routing](file-based-routing.md) for details.

### Static Files Plugin

Serves static files:

```typescript
import { staticFiles } from '@putnami/application';

const app = application()
  .use(staticFiles({ prefix: '/assets' }));
```

See [Static Files](static-files.md) for details.

### Config Plugin

Configuration management:

```typescript
import { config } from '@putnami/application';

const app = application()
  .use(config({ env: 'local' }));
```

See [Configuration](configuration.md) for details.

### OAuth Plugin

OAuth2 authentication:

```typescript
import { oAuth2 } from '@putnami/application';

const app = application()
  .use(oAuth2());
```

See [OAuth](oauth.md) for details.

## Module Composition

Modules let you group related plugins, providers, and sub-modules into reusable, composable units. The hierarchy is: **Application > Module > Plugin**.

### Creating a module

```typescript
import { module } from '@putnami/application';
import { api, oAuth2 } from '@putnami/application';

const authModule = module('auth')
  .path('/auth')
  .use(oAuth2())
  .use(api());
```

### Module path

Use `.path()` to set a base path for the module. Child plugins (`api()`, `react()`, `staticFiles()`) automatically inherit this path as their route prefix:

```typescript
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

```typescript
const dashboardModule = module('dashboard')
  .path('/dashboard')
  .secure()                      // Require authentication
  .use(api());

const adminModule = module('admin')
  .path('/admin')
  .secure({ roles: ['admin'] })  // Require admin role
  .use(api());
```

### Composing modules into an application

```typescript
import { application, http } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(tasksModule)
  .use(authModule);
```

### Nested modules

Modules can contain other modules:

```typescript
const innerModule = module('inner').use(somePlugin());
const outerModule = module('outer')
  .use(innerModule)
  .use(anotherPlugin());

const app = application()
  .use(outerModule);
```

### Plugin resolution across modules

Plugins can find other plugins by walking up the module hierarchy:

```typescript
class MyPlugin implements Plugin {
  async warmup(owner: Module): Promise<void> {
    // Searches local plugins first, then walks up to parent modules
    const http = owner.getPlugin(HttpPlugin);
  }
}
```

### Modules with DI providers

Modules can declare DI providers and requirements:

```typescript
const dbModule = module('db')
  .provide(DatabaseService)
  .use(sql());

const authModule = module('auth')
  .require(DatabaseService)
  .provide(AuthService, { deps: [DatabaseService] })
  .use(oAuth2());
```

Use `composeModules()` when sibling feature modules should resolve DI
requirements from each other while remaining visible as child modules for plugin
lifecycle walks:

```typescript
const authModule = composeModules([authUsers(), authCodes(), authOAuthClients()], {
  name: 'auth.server',
});

application().use(authModule);
```

## Plugin Registration Order

Plugins are initialized in the order they are registered:

```typescript
const app = application()
  .use(plugin1())  // First: warmup, then start
  .use(plugin2())  // Second: warmup, then start
  .use(plugin3()); // Third: warmup, then start
```

During shutdown, plugins are stopped in reverse order:

```
plugin3.stop() → plugin2.stop() → plugin1.stop()
```

## Plugin Communication

Plugins can communicate through the Application instance:

```typescript
class PluginA implements Plugin {
  sharedData = { value: 42 };
}

class PluginB implements Plugin {
  async warmup(owner: Module): Promise<void> {
    // Access PluginA through the module hierarchy
    const pluginA = owner.getPlugin(PluginA);
    console.log(pluginA.sharedData); // { value: 42 }
  }
}
```

## Best Practices

1. **Use factory functions** - Create plugins with `function plugin(config)` pattern
2. **Handle errors gracefully** - Don't let plugin errors crash the application
3. **Document dependencies** - Clearly state which plugins your plugin requires
4. **Use `ensurePlugin()` for optional dependencies** - Creates plugin if missing
5. **Use `getPlugin()` for required dependencies** - Fails fast if missing
6. **Clean up resources** - Always implement `stop()` if you allocate resources

## Example: Database Plugin

Here's a complete example of a database plugin:

```typescript
import type { Module, Plugin } from '@putnami/application';

interface DatabaseConfig {
  connectionString: string;
}

class DatabasePlugin implements Plugin {
  private connection: any;

  constructor(private config: DatabaseConfig) {}

  async warmup(owner: Module): Promise<void> {
    console.log('Database plugin warming up...');
    // Initialize connection pool, etc.
  }

  async start(owner: Module): Promise<void> {
    console.log('Database plugin starting...');
    // Connect to database
    this.connection = await connect(this.config.connectionString);
  }

  async stop(owner: Module): Promise<void> {
    console.log('Database plugin stopping...');
    // Close connections
    if (this.connection) {
      await this.connection.close();
    }
  }

  getConnection() {
    return this.connection;
  }
}

export function database(config: DatabaseConfig): DatabasePlugin {
  return new DatabasePlugin(config);
}

// Usage
const app = application()
  .use(database({ connectionString: process.env.DATABASE_URL! }))
  .run(async (app) => {
    const db = app.getPlugin(DatabasePlugin);
    const users = await db.getConnection().query('SELECT * FROM users');
  });
```

## Factory Functions

### `application()`

Creates a new `Application` instance as the standard fluent entry point for composing plugins and modules:

```typescript
import { application } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(react());

await app.start();
```

### `module(name)`

Creates a new `Module` instance for grouping related plugins:

```typescript
import { module } from '@putnami/application';

const authModule = module('auth')
  .path('/auth')
  .use(oAuth2())
  .use(api());
```

### `composeModules(modules, options?)`

Creates a module that mounts a set of child modules as one DI unit. Requirements
declared by one child can be satisfied by providers from another child, duplicate
providers across the composed subtree throw, and unsatisfied requirements are
de-duplicated for the parent application.

```typescript
import { composeModules } from '@putnami/application';

const authModule = composeModules([authUsers(), authCodes()], {
  name: 'auth.server',
});
```

## Next Steps

- Learn about [HTTP Server](http-server.md) for HTTP plugin details
- Explore [File-based Routing](file-based-routing.md) for API plugin
- Check [API Reference](api-reference.md) for complete plugin API
