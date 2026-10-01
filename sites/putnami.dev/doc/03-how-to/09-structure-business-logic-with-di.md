# Structure business logic with DI

You will create a service layer with dependency injection, organize it into a module, and inject it into API endpoints.

## Steps

### 1) Define a service

Services are plain classes. Dependencies come through the constructor:

```ts
// src/services/user.service.ts
export class UserService {
  constructor(private db: DatabaseService) {}

  async getUser(id: string) {
    return this.db.query(`SELECT * FROM users WHERE id = $1`, [id]);
  }

  async listUsers() {
    return this.db.query(`SELECT * FROM users`);
  }
}
```

No decorators, no framework imports — just a class.

### 2) Register providers

In `src/main.ts`, register services with explicit dependencies:

```ts
import { application, http, api } from '@putnami/application';

export const app = () =>
  application()
    .use(http())
    .use(api())
    .provide(DatabaseService)
    .provide(UserService, { deps: [DatabaseService] });
```

The `deps` array tells the DI container what to pass to the constructor, in order.

### 3) Inject into endpoints

Use `.inject()` on `endpoint()`, `loader()`, or `action()` builders. Resolved dependencies are exposed on `ctx.deps`:

```ts
// src/api/users/get.ts
import { endpoint } from '@putnami/application';
import { UserService } from '../../services/user.service';

export default endpoint()
  .inject({ users: UserService })
  .handle(async (ctx) => {
    return { users: await ctx.deps.users.listUsers() };
  });
```

```ts
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';
import { UserService } from '../../../services/user.service';

export default endpoint()
  .params({ id: Uuid })
  .inject({ users: UserService })
  .handle(async (ctx) => {
    const user = await ctx.deps.users.getUser(ctx.params.id);
    if (!user) throw new HttpException(404, 'User not found');
    return { user };
  });
```

### 4) Group into modules

When your service layer grows, group related providers into modules with explicit contracts:

```ts
// src/modules/auth.module.ts
import { module } from '@putnami/application';

export const authModule = module('auth')
  .require(DatabaseService)                              // Must exist in parent
  .provide(AuthService, { deps: [DatabaseService] })     // Public — usable by endpoints
  .provide(TokenValidator, { visibility: 'private' });   // Private — internal to module
```

Mount the module in your application:

```ts
export const app = () =>
  application()
    .use(http())
    .use(api())
    .provide(DatabaseService)
    .use(authModule);
```

### 5) Use scoped providers for per-request state

Mark providers as `scoped` to get a fresh instance per HTTP request:

```ts
export const app = () =>
  application()
    .provide(RequestContext, { scope: 'scoped' })
    .provide(AuditLogger, { deps: [RequestContext] });
```

Scoped providers are automatically resolved per request when injected into endpoints.

### 6) Test with fork

Fork the DI context and override providers for test isolation:

```ts
import { describe, test, expect } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';

describe('UserService', () => {
  test('lists users', async () => {
    await using ctx = new ContainerContext('test');
    ctx.register(provide(DatabaseService, () => ({
      query: async () => [{ id: '1', name: 'Alice' }],
    })));
    ctx.register(provide(UserService, { deps: [DatabaseService] }));
    await ctx.start();

    const users = ctx.get(UserService);
    const result = await users.listUsers();
    expect(result).toHaveLength(1);
  });
});
```

## Next steps

- See the [Dependency Injection reference](/docs/frameworks/typescript/dependency-injection) for scopes, tags, lazy providers, and tracing
- Add [persistence](/docs/how-to/add-persistence) for database-backed services
- Add [authentication](/docs/how-to/add-authentication) to protect injected endpoints

> **Companion sample:** [`typescript/samples/05-dependency-injection`](https://github.com/putnami/putnami/tree/main/typescript/samples/05-dependency-injection) — run `putnami serve @example/05-dependency-injection` to see DI in action.

You now have a service layer with dependency injection, organized into modules, injected into type-safe endpoints, and testable in isolation.
