import { describe, expect, it } from 'bun:test';
import { ContainerContext } from '../../src/inject/container-context';
import { NotRegisteredError } from '../../src/inject/errors';
import type { FilterOptions, ScopeContext } from '../../src/inject/inject.type';
import { provide } from '../../src/inject/provider';
import { named, tagged } from '../../src/inject/token';
import { useContainer, resolve, resolveInjection, SCOPE_CONTAINER_KEY } from '../../src/inject/scope';

class Database {
  query() {
    return [];
  }
}

class UserService {
  constructor(readonly db: Database) {}
}

const ApiUrl = named<string>('api-url');

describe('useContainer', () => {
  it('throws when no scope is active', () => {
    expect(() => useContainer()).toThrow('No active DI scope');
  });

  it('returns scope context when inside ctx.scope()', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));
    ctx.register(provide(UserService, { deps: [Database] }));

    await ctx.start();

    await ctx.scope(async () => {
      const scope = useContainer();
      expect(scope).toBeDefined();
      expect(scope.get).toBeDefined();
      expect(scope.list).toBeDefined();
      expect(scope.has).toBeDefined();
    });

    await ctx.close();
  });

  it('resolves same instances as scope parameter', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));

    await ctx.start();

    await ctx.scope(async (scope) => {
      const fromParam = scope.get(Database);
      const fromGlobal = useContainer().get(Database);
      expect(fromParam).toBe(fromGlobal);
    });

    await ctx.close();
  });
});

describe('resolve', () => {
  it('throws when no scope is active', () => {
    expect(() => resolve(Database)).toThrow('No active DI scope');
  });

  it('resolves a token from the current scope', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));

    await ctx.start();

    await ctx.scope(async () => {
      const db = resolve(Database);
      expect(db).toBeInstanceOf(Database);
    });

    await ctx.close();
  });

  it('returns same instance as useContainer().get()', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));

    await ctx.start();

    await ctx.scope(async () => {
      const fromResolve = resolve(Database);
      const fromContainer = useContainer().get(Database);
      expect(fromResolve).toBe(fromContainer);
    });

    await ctx.close();
  });
});

describe('resolveInjection', () => {
  it('resolves a token map from a scope', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));
    ctx.register(provide(UserService, { deps: [Database] }));
    ctx.register(provide(ApiUrl, () => 'https://api.example.com'));

    await ctx.start();

    await ctx.scope(async (scope) => {
      const deps = resolveInjection({ db: Database, userService: UserService, url: ApiUrl }, scope);

      expect(deps.db).toBeInstanceOf(Database);
      expect(deps.userService).toBeInstanceOf(UserService);
      expect(deps.url).toBe('https://api.example.com');
    });

    await ctx.close();
  });

  it('resolves tag selectors via list()', async () => {
    class PluginA {
      name = 'A';
    }
    class PluginB {
      name = 'B';
    }

    const ctx = new ContainerContext('test');
    ctx.register(provide(PluginA, { tags: ['plugin'] }));
    ctx.register(provide(PluginB, { tags: ['plugin'] }));

    await ctx.start();

    await ctx.scope(async (scope) => {
      const deps = resolveInjection({ plugins: tagged('plugin') }, scope);
      expect(deps.plugins).toHaveLength(2);
    });

    await ctx.close();
  });

  it('resolves FilterOptions via list()', async () => {
    class PluginA {
      name = 'A';
    }
    class PluginB {
      name = 'B';
    }

    const ctx = new ContainerContext('test');
    ctx.register(provide(PluginA, { tags: ['plugin'] }));
    ctx.register(provide(PluginB, { tags: ['plugin'] }));

    await ctx.start();

    await ctx.scope(async (scope) => {
      const filter: FilterOptions = { tags: 'plugin' };
      const deps = resolveInjection({ plugins: filter }, scope);
      expect(deps.plugins).toHaveLength(2);
    });

    await ctx.close();
  });

  it('works with useContainer() inside ctx.scope()', async () => {
    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));
    ctx.register(provide(ApiUrl, () => 'test-url'));

    await ctx.start();

    await ctx.scope(async () => {
      const scope = useContainer();
      const deps = resolveInjection({ db: Database, url: ApiUrl }, scope);
      expect(deps.db).toBeInstanceOf(Database);
      expect(deps.url).toBe('test-url');
    });

    await ctx.close();
  });

  it('includes injection key in error when token is not registered', async () => {
    class Missing {}

    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));

    await ctx.start();

    await ctx.scope(async (scope) => {
      expect(() => resolveInjection({ db: Database, svc: Missing }, scope)).toThrow(/inject\(\{ svc: Missing \}\)/);
    });

    await ctx.close();
  });

  it('rethrows a real NotRegisteredError preserving prototype, cause, and metadata', async () => {
    class Missing {}

    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));

    await ctx.start();

    await ctx.scope(async (scope) => {
      let caught: unknown;
      try {
        resolveInjection({ svc: Missing }, scope);
      } catch (err) {
        caught = err;
      }

      // The enriched error keeps the NotRegisteredError prototype so downstream
      // instanceof checks still work (the previous bare-Error rethrow lost this).
      expect(caught).toBeInstanceOf(NotRegisteredError);
      const error = caught as NotRegisteredError;
      expect(error.message).toContain('inject({ svc: Missing })');
      // Resolution metadata is carried over from the original error.
      expect(error.token).toBe(Missing);
      // The original NotRegisteredError is preserved as the cause.
      expect(error.cause).toBeInstanceOf(NotRegisteredError);
      expect(error.cause).not.toBe(error);
    });

    await ctx.close();
  });

  it('does not enrich an unrelated error that merely shares the name "NotRegisteredError"', () => {
    // A plain Error whose name happens to be "NotRegisteredError" must NOT be
    // mistaken for the DI error: the old string-name check would have rewrapped
    // it (and swallowed it as an inject() failure). instanceof avoids that.
    const impostor = new Error('totally unrelated failure');
    impostor.name = 'NotRegisteredError';

    const scope: ScopeContext = {
      get: () => {
        throw impostor;
      },
      list: () => [],
      has: () => false,
    };

    class Svc {}

    let caught: unknown;
    try {
      resolveInjection({ svc: Svc }, scope);
    } catch (err) {
      caught = err;
    }

    // The original error is rethrown untouched: not a NotRegisteredError, no
    // inject() enrichment, no cause chaining.
    expect(caught).toBe(impostor);
    expect(caught).not.toBeInstanceOf(NotRegisteredError);
    expect((caught as Error).message).toBe('totally unrelated failure');
  });
});

describe('SCOPE_CONTAINER_KEY', () => {
  it('is a Symbol.for value', () => {
    expect(typeof SCOPE_CONTAINER_KEY).toBe('symbol');
    expect(SCOPE_CONTAINER_KEY).toBe(Symbol.for('__di_scope_container__'));
  });
});
