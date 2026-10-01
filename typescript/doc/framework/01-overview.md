# Overview

Putnami frameworks are composable plugins. You only bring the parts you need for each app, and the CLI orchestrates them together. This modular architecture lets you start simple and add capabilities as your application grows.

## Packages at a glance

- **@putnami/application** - HTTP servers, middleware, API routing, WebSockets, sessions, OAuth, static files
- **@putnami/web** - React SSR, file-based routing, loaders, actions, streaming, hydration
- **@putnami/database** - PostgreSQL integration, entities, repositories, migrations, queries
- **@putnami/document** - Portable document collections, typed repositories, cursor pagination, memory and Firestore adapters
- **@putnami/storage** - Object storage, bucket declarations, presigned URLs, pluggable backends
- **@putnami/ui** - UI components and theming, design system, accessible components
- **@putnami/runtime** - Foundation utilities, DI container, config, logging, validation
- **@putnami/utils** - The declarative schema vocabulary plus shared browser/server helpers
- **@putnami/cli-protocol** - Typed, validated reader for Putnami CLI machine output

## Application composition

Every Putnami application starts with an `Application` instance. You compose functionality by chaining `.use()` calls with plugins and modules. Prefer the `application()` factory:

```ts
import { application, http, api, staticFiles } from '@putnami/application';
import { react } from '@putnami/web';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(api())
    .use(react())
    .use(sql())
    .use(staticFiles({ publicFolder: 'public' }));
```

### Modules

Group related plugins into reusable modules with `module()`. The hierarchy is **Application > Module > Plugin**:

```ts
import { application, module, http, api, oAuth2 } from '@putnami/application';

const authModule = module('auth')
  .use(oAuth2())
  .use(api({ prefix: '/auth' }));

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(authModule);
```

Modules are composable — they can contain plugins, sub-modules, and DI providers. Only the Application has a startable lifecycle; modules are structural units. Use `composeModules()` when a set of sibling feature modules should satisfy each other's DI requirements as one mounted unit.

### Plugin lifecycle

Each plugin can hook into four lifecycle phases:

1. **generate()** - Runs at build time for code generation and asset compilation
2. **warmup()** - Runs before the app starts for route registration and cache preloading
3. **start()** - Starts servers, opens database connections, initializes background tasks
4. **stop()** - Graceful shutdown in reverse order of registration

## Architecture patterns

### Full-stack web application

For applications with a React frontend and API backend:

```ts
import { application, http, api, staticFiles, oAuth2 } from '@putnami/application';
import { react } from '@putnami/web';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(sql())
    .use(oAuth2())
    .use(api())
    .use(react())
    .use(staticFiles({ publicFolder: 'public' }));
```

### API-only service

For microservices or backend APIs without a frontend:

```ts
import { application, http, api } from '@putnami/application';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(sql())
    .use(api());
```

### Background job runner

For one-shot jobs or scheduled tasks:

```ts
import { application } from '@putnami/application';
import { sql } from '@putnami/database';

export const job = () =>
  application()
    .use(sql())
    .run(async () => {
      // Perform your job logic here
      console.log('Job completed');
    });
```

## How to choose packages

### Building a web UI

Start with `@putnami/web` and `@putnami/ui`:

```ts
import { application, http, staticFiles } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () =>
  application()
    .use(http())
    .use(react())
    .use(staticFiles());
```

Key features:

- Server-side rendering with React 19 streaming
- File-based routing in `src/app/`
- Typed loaders for data fetching
- Actions for form handling
- Document helpers for SEO

### Building an API

Start with `@putnami/application` and the `api()` plugin:

```ts
import { application, http, api } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(api());
```

Key features:

- File-based route discovery
- Request context with typed body parsing
- Response helpers for JSON, redirects, errors
- Middleware composition
- WebSocket support

### Adding persistence

Add `@putnami/database` for PostgreSQL support:

```ts
import { application } from '@putnami/application';
import { sql } from '@putnami/database';

export const app = () =>
  application()
    .use(sql());
```

Key features:

- Entity decorators (`@Table`, `@Column`, `@Key`)
- Repository pattern with type-safe queries
- Migration management
- Multi-database support
- Query operators for complex filters

### Adding document storage

Add `@putnami/document` when your data is document-shaped and you want portable repositories across memory and Firestore:

```ts
import { application } from '@putnami/application';
import { document } from '@putnami/document';

export const app = () =>
  application()
    .use(document());
```

Key features:

- Declarative `Collection`, `Field`, and `DocumentId` builders
- Typed repositories with cursor-based pagination
- Portable query operators across adapters
- Single-store transactions
- Named document stores and optional strict index enforcement

### Adding authentication

Add the `oAuth2()` plugin for OAuth2/OIDC support:

```ts
import { application, http, oAuth2 } from '@putnami/application';

export const app = () =>
  application()
    .use(http())
    .use(oAuth2());
```

Key features:

- OAuth2 provider integration
- Session management
- Token handling
- Protected route helpers

### Shared concerns

Use `@putnami/runtime` for cross-cutting functionality:

```ts
import { useLogger, useConfig } from '@putnami/runtime';

class MyService {
  private logger = useLogger('MyService');

  doWork() {
    this.logger.info('Working...');
  }
}
```

Key features:

- Dependency injection with singleton and request scopes
- Typed configuration with validation
- Structured logging
- Error handling utilities

## Project structure

A typical Putnami project follows this structure:

```text
my-app/
  src/
    main.ts           # Application entry point
    app/              # Routes and pages
      page.tsx        # Homepage (React)
      loader.ts       # Homepage data loader
      api/
        health/
          get.ts      # GET /api/health
        users/
          get.ts      # GET /api/users
          post.ts     # POST /api/users
          [id]/
            get.ts    # GET /api/users/:id
    entities/         # Database entities
      user.ts
    services/         # Business logic
      user.service.ts
    lib/              # Utilities
  public/             # Static assets
  .env.local.yaml     # Local configuration
  package.json
  tsconfig.json
```

## Next steps

Dive deeper into each framework capability:

- [Web](/docs/frameworks/typescript/web) - React SSR and file-based routing
- [API](/docs/frameworks/typescript/api) - HTTP routes and request handling
- [Auth](/docs/frameworks/typescript/auth) - OAuth2 and authentication
- [Persistence](/docs/frameworks/typescript/persistence) - Database entities and queries
- [Document storage](/docs/frameworks/typescript/document) - Document collections and repository APIs
- [Storage](/docs/frameworks/typescript/storage) - Object storage and file uploads
- [Configuration](/docs/frameworks/typescript/configuration) - Typed config management
- [Plugins & Lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle) - Custom plugins
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware) - Request processing
- [Dependency Injection](/docs/frameworks/typescript/dependency-injection) - Service management
