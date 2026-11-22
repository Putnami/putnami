# Testing

Putnami uses Bun's native test runner for fast, integrated testing. Run tests with the CLI to automatically detect impacted projects and optimize test execution.

## Running tests

### Basic commands

```bash
# Run tests for impacted projects only
bunx putnami test --impacted

# Run all tests
bunx putnami test --all

# Run tests for specific project
bunx putnami test my-project

# Watch mode
bunx putnami test . --watch
```

### Test file patterns

Tests are discovered by these patterns:

- `**/*.test.ts`
- `**/*.spec.ts`
- `test/**/*.ts`

## Unit testing

### Testing services with a forked context

Register the production providers in a `ContainerContext` once. In each test,
fork it and override the providers you replace with test doubles. The fork
leaves the original untouched:

```ts
// src/services/user.service.test.ts
import { describe, test, expect, mock } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';
import { UserService } from './user.service';
import { UserRepository } from '../repositories/user.repository';

const base = new ContainerContext('test');
base.register(provide(UserRepository));
base.register(provide(UserService, { deps: [UserRepository] }));

describe('UserService', () => {
  test('getUser returns user by id', async () => {
    await using ctx = base.fork().override(UserRepository, () => ({
      findOne: mock(() => Promise.resolve({ id: '1', name: 'Test' })),
    }));
    await ctx.start();

    const user = await ctx.get(UserService).getUser('1');

    expect(user).toEqual({ id: '1', name: 'Test' });
  });
});
```

To test through HTTP with a test double, start the application with
`createTestApp` from `@putnami/application/testing`, and register the double
in its `configure` callback: `configure: (app) => app.provide(...)`.

### Testing services with a standalone context

You can also create a self-contained `ContainerContext` for tests:

```ts
import { describe, test, expect, mock } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';
import { UserService } from './user.service';
import { UserRepository } from '../repositories/user.repository';

describe('UserService', () => {
  test('getUser returns user by id', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(UserRepository, () => ({
      findOne: mock(() => Promise.resolve({ id: '1', name: 'Test' })),
      save: mock((data) => Promise.resolve({ id: '1', ...data })),
      delete: mock(() => Promise.resolve()),
    })));
    ctx.register(provide(UserService, { deps: [UserRepository] }));

    await ctx.start();

    const service = ctx.get(UserService);
    const user = await service.getUser('1');

    expect(user).toEqual({ id: '1', name: 'Test' });

    await ctx.close();
  });
});
```

### Testing utilities

```ts
// src/lib/validators.test.ts
import { describe, test, expect } from 'bun:test';
import { isValidEmail, formatCurrency } from './validators';

describe('isValidEmail', () => {
  test('accepts valid emails', () => {
    expect(isValidEmail('user@example.com')).toBe(true);
    expect(isValidEmail('user.name@domain.co.uk')).toBe(true);
  });

  test('rejects invalid emails', () => {
    expect(isValidEmail('invalid')).toBe(false);
    expect(isValidEmail('missing@')).toBe(false);
    expect(isValidEmail('@nodomain.com')).toBe(false);
  });
});

describe('formatCurrency', () => {
  test('formats USD correctly', () => {
    expect(formatCurrency(1234.56, 'USD')).toBe('$1,234.56');
  });

  test('handles zero', () => {
    expect(formatCurrency(0, 'USD')).toBe('$0.00');
  });
});
```

## Testing API routes

### Route handler tests

Test API routes by creating a test context with mock providers:

```ts
// src/app/api/users/get.test.ts
import { describe, test, expect, mock } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';
import { UserRepository } from '../../../repositories/user.repository';

describe('GET /api/users', () => {
  test('returns list of users', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(UserRepository, () => ({
      find: mock(() => Promise.resolve([
        { id: '1', name: 'User 1' },
        { id: '2', name: 'User 2' },
      ])),
    })));

    await ctx.start();

    const repo = ctx.get(UserRepository);
    const users = await repo.find();

    expect(users).toHaveLength(2);
    expect(users[0].name).toBe('User 1');

    await ctx.close();
  });
});
```

## Testing with database

`putnami test` starts a throwaway Postgres for a project that uses `sql()` and
passes it to the tests in `DATABASE_TEST_BINDINGS`. `provision()` creates an
isolated database for the suite, applies the project's migrations, and points
`DATABASE_BINDINGS` at it. `cleanup()` drops it. The migrations come from the
bundle `putnami build` writes under `.gen/migration-bundle/`: run
`putnami build` once, and again after you change a migration; `provision()`
warns when the bundle is missing or older than `src/`. Pass
`provision({ sources })` to choose the sources, or `provision({ sources: [] })`
to apply none. Start the application with `sql()`: the database is already
migrated.

### Integration tests

Start the application on the provisioned database with `createTestApp`, and
test it through HTTP:

```ts
// test/users.integration.test.ts
import { afterAll, beforeAll, beforeEach, describe, expect, test } from 'bun:test';
import { api } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { closeAllDatabases, database, provision, sql } from '@putnami/database';

describe('users (integration)', () => {
  let db: Awaited<ReturnType<typeof provision>>;
  let testApp: TestApp;

  beforeAll(async () => {
    db = await provision();
    testApp = await createTestApp({
      plugins: [sql(), api({ scanPath: 'src/api' })],
    });
  });

  afterAll(async () => {
    await testApp?.stop();
    await closeAllDatabases();
    await db?.cleanup();
  });

  beforeEach(async () => {
    const client = await database();
    await client`TRUNCATE users CASCADE`;
  });

  test('creates and retrieves a user', async () => {
    const created = await testApp.fetch('/users', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name: 'Test User', email: 'test@example.com' }),
    });
    expect(created.status).toBe(200);
    const { id } = await created.json();

    const res = await testApp.fetch(`/users/${id}`);
    expect((await res.json()).email).toBe('test@example.com');
  });
});
```

`database()` is async: await it before you run a query. The migration source
and plugin come from your project; see
[Persistence](/docs/frameworks/typescript/persistence#migrations).

## Testing React components

### Loader tests

```ts
// src/app/users/loader.test.ts
import { describe, test, expect, mock } from 'bun:test';
import { application } from '@putnami/application';
import { UserService } from '../../services/user.service';
import { loader } from './loader';

describe('users loader', () => {
  test('returns users from service', async () => {
    const app = application()
      .provide(UserService, () => ({
        listUsers: async () => [
          { id: '1', name: 'User 1' },
          { id: '2', name: 'User 2' },
        ],
      }));

    await app.start();

    const mockContext = {
      queryParams: () => new URLSearchParams(),
    };

    const result = await loader(mockContext as any);

    expect(result.users).toHaveLength(2);

    await app.stop();
  });
});
```

## Mocking

### Using Bun's mock

```ts nocheck
import { mock, spyOn } from 'bun:test';

// Create a mock function
const mockFn = mock(() => 'mocked value');

// Call it
mockFn('arg1', 'arg2');

// Assertions
expect(mockFn).toHaveBeenCalled();
expect(mockFn).toHaveBeenCalledWith('arg1', 'arg2');
expect(mockFn).toHaveBeenCalledTimes(1);

// Mock implementation
const mockAsync = mock(async () => ({ data: 'value' }));

// Spy on existing method
const obj = { method: () => 'original' };
const spy = spyOn(obj, 'method').mockReturnValue('mocked');
```

### Mocking modules

```ts
import { mock } from 'bun:test';

// Mock at module level
mock.module('./email-client', () => ({
  sendEmail: mock(async () => ({ success: true })),
}));

// Now imports will use the mock
import { sendEmail } from './email-client';
```

### Mocking fetch

```ts nocheck
import { mock } from 'bun:test';

const originalFetch = global.fetch;

beforeEach(() => {
  global.fetch = mock(async (url: string) => {
    if (url.includes('/api/users')) {
      return new Response(JSON.stringify([{ id: '1', name: 'User' }]));
    }
    return new Response('Not found', { status: 404 });
  });
});

afterEach(() => {
  global.fetch = originalFetch;
});
```

## Test patterns

### Arrange-Act-Assert

```ts
test('user can be created', async () => {
  // Arrange
  const service = get(UserService);
  const input = { name: 'Test', email: 'test@example.com' };

  // Act
  const result = await service.createUser(input);

  // Assert
  expect(result.name).toBe('Test');
  expect(result.email).toBe('test@example.com');
  expect(result.id).toBeDefined();
});
```

### Test fixtures

```ts nocheck
// test/fixtures/users.ts
export const testUsers = {
  admin: {
    id: 'admin-1',
    name: 'Admin User',
    email: 'admin@example.com',
    role: 'admin',
  },
  regular: {
    id: 'user-1',
    name: 'Regular User',
    email: 'user@example.com',
    role: 'user',
  },
};

// In tests
import { testUsers } from '../../test/fixtures/users';

test('admin can access dashboard', async () => {
  // Use fixture
  const result = await checkAccess(testUsers.admin, '/dashboard');
  expect(result.allowed).toBe(true);
});
```

### Factory functions

```ts
// test/factories/user.factory.ts
let counter = 0;

export function createTestUser(overrides: Partial<User> = {}): User {
  counter++;
  return {
    id: `test-user-${counter}`,
    name: `Test User ${counter}`,
    email: `test${counter}@example.com`,
    createdAt: new Date(),
    ...overrides,
  };
}

// In tests
test('lists users', async () => {
  const users = [createTestUser(), createTestUser(), createTestUser()];
  // ...
});
```

## Coverage

### Enabling coverage

Coverage is enabled by default in Putnami. View coverage reports after running tests:

```bash
bunx putnami test --all

# Coverage report is generated in ./coverage/
```

### Coverage configuration

```json
// package.json
{
  "putnami": {
    "test": {
      "coverage": true,
      "coverageThreshold": {
        "lines": 80,
        "functions": 80,
        "branches": 70
      }
    }
  }
}
```

## Best practices

### Test isolation

Each test should be independent. Use `await using` or fresh `ContainerContext` instances:

```ts
import { ContainerContext, provide } from '@putnami/runtime';

test('isolated test', async () => {
  await using ctx = new ContainerContext('test');
  ctx.register(provide(MyService));

  await ctx.start();
  // test with ctx.get(MyService)
});
// ctx is automatically cleaned up via Symbol.asyncDispose
```

### Descriptive test names

```ts
// Good
test('createUser throws ConflictException when email exists', async () => {});
test('getUser returns null when user not found', async () => {});

// Bad
test('test1', async () => {});
test('should work', async () => {});
```

### Test behavior, not implementation

```ts
// Good - tests behavior
test('user can log in with valid credentials', async () => {
  const result = await authService.login('user@example.com', 'password');
  expect(result.success).toBe(true);
});

// Bad - tests implementation details
test('login calls bcrypt.compare', async () => {
  // ...
});
```

### Keep tests fast

```ts
// Use mocks for external services via provide()
provide(EmailService, () => ({ send: async () => {} }))

// Use in-memory database for unit tests
// Use real database only for integration tests
```

## Related guides

- [Dependency Injection](/docs/frameworks/typescript/dependency-injection)
- [Configuration](/docs/frameworks/typescript/configuration)
