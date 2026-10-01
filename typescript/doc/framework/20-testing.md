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

### Testing services with `app.fork()`

The recommended approach is to fork your production application and override specific providers with test doubles:

```ts
// src/services/user.service.test.ts
import { describe, test, expect, mock } from 'bun:test';
import { app } from '../app'; // your production application
import { UserService } from './user.service';
import { UserRepository } from '../repositories/user.repository';

describe('UserService', () => {
  test('getUser returns user by id', async () => {
    await using testApp = app.fork()
      .override(UserRepository, () => ({
        findOne: mock(() => Promise.resolve({ id: '1', name: 'Test' })),
        save: mock((data) => Promise.resolve({ id: '1', ...data })),
        delete: mock(() => Promise.resolve()),
      }));

    await testApp.start();

    const service = testApp.get(UserService);
    const user = await service.getUser('1');

    expect(user).toEqual({ id: '1', name: 'Test' });
  });

  test('createUser saves and returns user', async () => {
    await using testApp = app.fork()
      .override(UserRepository, () => ({
        findOne: mock(() => Promise.resolve(null)),
        save: mock((data) => Promise.resolve({ id: '1', ...data })),
      }));

    await testApp.start();

    const service = testApp.get(UserService);
    const user = await service.createUser({
      name: 'New User',
      email: 'new@example.com',
    });

    expect(user.name).toBe('New User');
    expect(user.email).toBe('new@example.com');
  });
});
```

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

### Test database setup

```ts
// test/setup.ts
import { database, closeAllDatabases } from '@putnami/database';

export async function setupTestDb() {
  const db = database('test');

  // Run migrations
  await db`CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT
  )`;

  return db;
}

export async function cleanupTestDb() {
  const db = database('test');
  await db`TRUNCATE users CASCADE`;
}

export async function teardownTestDb() {
  await closeAllDatabases();
}
```

### Integration tests

```ts
// src/services/user.service.integration.test.ts
import { describe, test, expect, beforeAll, afterAll, beforeEach } from 'bun:test';
import { setupTestDb, cleanupTestDb, teardownTestDb } from '../../test/setup';
import { app } from '../app';
import { UserService } from './user.service';

describe('UserService (integration)', () => {
  let testApp: Awaited<ReturnType<typeof app.fork>>;

  beforeAll(async () => {
    await setupTestDb();
    testApp = app.fork();
    await testApp.start();
  });

  afterAll(async () => {
    await testApp.close();
    await teardownTestDb();
  });

  beforeEach(async () => {
    await cleanupTestDb();
  });

  test('creates and retrieves user', async () => {
    const service = testApp.get(UserService);

    const created = await service.createUser({
      name: 'Test User',
      email: 'test@example.com',
    });

    const retrieved = await service.getUser(created.id);

    expect(retrieved.name).toBe('Test User');
    expect(retrieved.email).toBe('test@example.com');
  });

  test('prevents duplicate emails', async () => {
    const service = testApp.get(UserService);

    await service.createUser({
      name: 'User 1',
      email: 'same@example.com',
    });

    await expect(
      service.createUser({
        name: 'User 2',
        email: 'same@example.com',
      })
    ).rejects.toThrow();
  });
});
```

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

```ts
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

```ts
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

```ts
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
