import { describe, expect, it } from 'bun:test';
import { ContainerContext } from '../../src/inject/container-context';
import {
  ContainerClosedError,
  ContainerValidationError,
  NotRegisteredError,
  RequirementNotMetError,
} from '../../src/inject/errors';
import type { ContainerHolder, TraceData, TraceSink } from '../../src/inject/inject.type';
import { provide } from '../../src/inject/provider';
import { named } from '../../src/inject/token';
import { specTest } from '../../src/spectest';
import '../../src/inject/container-context-fork';

describe('ContainerContext', () => {
  describe('lifecycle', () => {
    it('should start and resolve singletons', async () => {
      class Database {
        connected = false;
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));

      await ctx.start();

      const db = ctx.get(Database);
      expect(db).toBeInstanceOf(Database);

      await ctx.close();
    });

    it('should resolve async factories during start', async () => {
      class Database {
        connected = false;
        async connect() {
          this.connected = true;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(
          Database,
          async () => {
            const db = new Database();
            await db.connect();
            return db;
          },
          {
            onClose: (db) => {
              db.connected = false;
            },
          },
        ),
      );

      await ctx.start();
      expect(ctx.get(Database).connected).toBe(true);

      await ctx.close();
    });

    specTest(
      'should throw when starting twice',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'one-shot-lifecycle',
        check: 'starting-twice-raises-a-named-error',
      },
      async () => {
        const ctx = new ContainerContext('test');
        await ctx.start();
        await expect(ctx.start()).rejects.toThrow("current state is 'started'");
        await ctx.close();
      },
    );

    specTest(
      'should throw when closing before start',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'one-shot-lifecycle',
        check: 'closing-before-start-raises-a-named-error',
      },
      async () => {
        const ctx = new ContainerContext('test');
        await expect(ctx.close()).rejects.toThrow("current state is 'idle'");
      },
    );

    specTest(
      'should throw when getting before start',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'one-shot-lifecycle',
        check: 'resolving-before-start-raises-a-named-error',
      },
      async () => {
        class Service {}
        const ctx = new ContainerContext('test');
        ctx.register(provide(Service));
        expect(() => ctx.get(Service)).toThrow('requires the context to be started');
      },
    );

    specTest(
      'should throw when getting after close',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'disposal-order',
        check: 'resolving-after-close-fails',
      },
      async () => {
        class Service {}
        const ctx = new ContainerContext('test');
        ctx.register(provide(Service));
        await ctx.start();
        await ctx.close();
        expect(() => ctx.get(Service)).toThrow(ContainerClosedError);
      },
    );

    specTest(
      'should call onClose in reverse order',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'disposal-order',
        check: 'onclose-hooks-run-in-reverse-construction-order',
      },
      async () => {
        const order: string[] = [];
        class A {}
        class B {}

        const ctx = new ContainerContext('test');
        ctx.register(provide(A, () => new A(), { onClose: () => order.push('A') }));
        ctx.register(provide(B, () => new B(), { onClose: () => order.push('B') }));

        await ctx.start();
        // Force resolution
        ctx.get(A);
        ctx.get(B);
        await ctx.close();

        expect(order).toEqual(['B', 'A']);
      },
    );
  });

  describe('validation', () => {
    specTest(
      'should fail start when deps are missing',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'graph-validation',
        check: 'an-unresolvable-dependency-fails-start',
      },
      async () => {
        class Unknown {}
        class Service {}

        const ctx = new ContainerContext('test');
        ctx.register(provide(Service, { deps: [Unknown] }));
        await expect(ctx.start()).rejects.toThrow(ContainerValidationError);
      },
    );

    specTest(
      'validates the whole graph before constructing any instance',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'graph-validation',
        check: 'validation-runs-before-any-instance-is-constructed',
      },
      async () => {
        // A provider that would resolve fine on its own, registered alongside
        // one that cannot. If validation ran lazily, per-provider, the healthy
        // one would already be constructed by the time the broken one failed.
        const constructed: string[] = [];
        class Unknown {}
        class Healthy {
          constructor() {
            constructed.push('Healthy');
          }
        }
        class Broken {
          constructor() {
            constructed.push('Broken');
          }
        }

        const ctx = new ContainerContext('test');
        ctx.register(provide(Healthy));
        ctx.register(provide(Broken, { deps: [Unknown] }));

        await expect(ctx.start()).rejects.toThrow(ContainerValidationError);
        expect(constructed).toEqual([]);
      },
    );

    specTest(
      'should fail start when circular deps exist',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'graph-validation',
        check: 'a-dependency-cycle-fails-start',
      },
      async () => {
        class A {}
        class B {}

        const ctx = new ContainerContext('test');
        ctx.register(provide(A, { deps: [B] }));
        ctx.register(provide(B, { deps: [A] }));

        await expect(ctx.start()).rejects.toThrow(ContainerValidationError);
      },
    );
  });

  describe('mount (ContainerHolder)', () => {
    it('should mount a holder as a child container', async () => {
      class Database {}
      class AuthService {}

      const authHolder: ContainerHolder = {
        getRegistrations: () => [provide(AuthService, { deps: [Database] })],
        getRequirements: () => [Database],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.mount(authHolder, 'auth');

      await ctx.start();
      expect(ctx.get(AuthService)).toBeInstanceOf(AuthService);
      await ctx.close();
    });

    it('should support multiple mounts', async () => {
      class Database {}
      class AuthService {}
      class StoreService {}

      const authHolder: ContainerHolder = {
        getRegistrations: () => [provide(AuthService)],
        getRequirements: () => [],
      };
      const storeHolder: ContainerHolder = {
        getRegistrations: () => [provide(StoreService)],
        getRequirements: () => [],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.mount(authHolder, 'auth');
      ctx.mount(storeHolder, 'store');

      await ctx.start();
      expect(ctx.get(AuthService)).toBeInstanceOf(AuthService);
      expect(ctx.get(StoreService)).toBeInstanceOf(StoreService);
      await ctx.close();
    });

    specTest(
      'should throw RequirementNotMetError when requirement is missing',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'graph-validation',
        check: 'an-unmet-module-requirement-fails',
      },
      () => {
        class Database {}
        class AuthService {}

        const authHolder: ContainerHolder = {
          getRegistrations: () => [provide(AuthService, { deps: [Database] })],
          getRequirements: () => [Database],
        };

        const ctx = new ContainerContext('test');
        // Database not registered in root
        expect(() => ctx.mount(authHolder, 'auth')).toThrow(RequirementNotMetError);
      },
    );

    it('should resolve parent dependencies from mounted holder', async () => {
      class Database {
        query() {
          return 'data';
        }
      }
      class UserRepo {
        constructor(public db: Database) {}
      }

      const usersHolder: ContainerHolder = {
        getRegistrations: () => [provide(UserRepo, { deps: [Database] })],
        getRequirements: () => [Database],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.mount(usersHolder, 'users');

      await ctx.start();
      const repo = ctx.get(UserRepo);
      expect(repo.db.query()).toBe('data');
      await ctx.close();
    });
  });

  describe('resolution', () => {
    it('should resolve named tokens', async () => {
      const Version = named<string>('version');
      const ctx = new ContainerContext('test');
      ctx.register(provide(Version, () => '3.0.0'));

      await ctx.start();
      expect(ctx.get(Version)).toBe('3.0.0');
      await ctx.close();
    });

    it('should resolve tagged providers via list()', async () => {
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
      const plugins = ctx.list({ tags: 'plugin' });
      expect(plugins).toHaveLength(2);
      await ctx.close();
    });

    it('should check has()', async () => {
      class Service {}
      class Unknown {}
      const ctx = new ContainerContext('test');
      ctx.register(provide(Service));

      await ctx.start();
      expect(ctx.has(Service)).toBe(true);
      expect(ctx.has(Unknown)).toBe(false);
      await ctx.close();
    });

    it('should check has() across mounted containers', async () => {
      class Database {}
      class AuthService {}

      const authHolder: ContainerHolder = {
        getRegistrations: () => [provide(AuthService)],
        getRequirements: () => [],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.mount(authHolder, 'auth');

      expect(ctx.has(Database)).toBe(true);
      expect(ctx.has(AuthService)).toBe(true);
    });

    it('should list() across root and mounted containers', async () => {
      class PluginA {
        name = 'A';
      }
      class PluginB {
        name = 'B';
      }

      const holder: ContainerHolder = {
        getRegistrations: () => [provide(PluginB, { tags: ['plugin'] })],
        getRequirements: () => [],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(PluginA, { tags: ['plugin'] }));
      ctx.mount(holder, 'plugins');

      await ctx.start();
      const plugins = ctx.list({ tags: 'plugin' });
      expect(plugins).toHaveLength(2);
      await ctx.close();
    });

    it('should throw a module-aware NotRegisteredError when a token is missing', async () => {
      // Regression: the miss path used to re-run root.get() purely to
      // throw, producing a chain-less error that named only the root and omitted
      // the mounted module containers that were also searched.
      class Database {}
      class AuthService {}
      class Missing {}

      const ctx = new ContainerContext('app');
      ctx.register(provide(Database));
      ctx.mount(
        {
          getRegistrations: () => [provide(AuthService)],
          getRequirements: () => [],
        },
        'auth',
      );
      ctx.mount(
        {
          getRegistrations: () => [],
          getRequirements: () => [],
        },
        'billing',
      );

      await ctx.start();

      let caught: unknown;
      try {
        ctx.get(Missing);
      } catch (err) {
        caught = err;
      }

      expect(caught).toBeInstanceOf(NotRegisteredError);
      const error = caught as NotRegisteredError;
      // The message lists every container that was searched, not just the root.
      expect(error.searchedContainers).toEqual(['app', 'auth', 'billing']);
      expect(error.message).toContain('app');
      expect(error.message).toContain('auth');
      expect(error.message).toContain('billing');
      expect(error.message).toContain('Missing');

      await ctx.close();
    });

    it('should resolve a public provider registered in a mounted module', async () => {
      // Confirms the module lookup loop still returns the instance (the fix only
      // changes the not-found path, not successful module resolution).
      class ModuleService {
        ping() {
          return 'pong';
        }
      }

      const ctx = new ContainerContext('app');
      ctx.mount(
        {
          getRegistrations: () => [provide(ModuleService)],
          getRequirements: () => [],
        },
        'mod',
      );

      await ctx.start();
      expect(ctx.get(ModuleService).ping()).toBe('pong');
      await ctx.close();
    });
  });

  describe('scoped execution', () => {
    specTest(
      'should create isolated scoped instances',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'scope-isolation',
        check: 'a-scoped-provider-is-one-instance-per-scope',
      },
      async () => {
        class RequestId {
          id = crypto.randomUUID();
        }

        const ctx = new ContainerContext('test');
        ctx.register(provide(RequestId, { scope: 'scoped' }));

        await ctx.start();

        let id1: string | undefined;
        let id2: string | undefined;

        await ctx.scope(async (scope) => {
          id1 = scope.get(RequestId).id;
          // Same within scope
          expect(scope.get(RequestId).id).toBe(id1);
        });

        await ctx.scope(async (scope) => {
          id2 = scope.get(RequestId).id;
        });

        expect(id1).toBeDefined();
        expect(id2).toBeDefined();
        expect(id1).not.toBe(id2);

        await ctx.close();
      },
    );

    it('should access singletons from scope', async () => {
      class Database {
        query() {
          return 'data';
        }
      }
      class RequestData {
        requestId = crypto.randomUUID();
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.register(provide(RequestData, { scope: 'scoped' }));

      await ctx.start();

      await ctx.scope(async (scope) => {
        // Singleton from parent
        expect(scope.get(Database).query()).toBe('data');
        // Scoped
        expect(scope.get(RequestData).requestId).toBeDefined();
      });

      await ctx.close();
    });

    it('should run scoped onClose hooks when scope ends', async () => {
      let closed = false;
      class ScopedResource {}

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(ScopedResource, () => new ScopedResource(), {
          scope: 'scoped',
          onClose: () => {
            closed = true;
          },
        }),
      );

      await ctx.start();

      await ctx.scope(async (scope) => {
        scope.get(ScopedResource);
        expect(closed).toBe(false);
      });

      expect(closed).toBe(true);
      await ctx.close();
    });

    specTest(
      'should isolate concurrent scopes',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'scope-isolation',
        check: 'concurrent-scopes-never-share-an-instance',
      },
      async () => {
        class RequestData {
          id = crypto.randomUUID();
        }

        const ctx = new ContainerContext('test');
        ctx.register(provide(RequestData, { scope: 'scoped' }));
        await ctx.start();

        const ids = await Promise.all([
          ctx.scope(async (scope) => scope.get(RequestData).id),
          ctx.scope(async (scope) => scope.get(RequestData).id),
          ctx.scope(async (scope) => scope.get(RequestData).id),
        ]);

        // All different
        expect(new Set(ids).size).toBe(3);

        await ctx.close();
      },
    );

    it('should support list() within scope', async () => {
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
        const plugins = scope.list<{ name: string }>({ tags: 'plugin' });
        expect(plugins).toHaveLength(2);
      });

      await ctx.close();
    });

    it('should resolve module providers from scope', async () => {
      class AuthService {
        validate() {
          return true;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.mount(
        {
          getRegistrations: () => [provide(AuthService)],
          getRequirements: () => [],
        },
        'auth',
      );
      await ctx.start();

      await ctx.scope(async (scope) => {
        expect(scope.get(AuthService).validate()).toBe(true);
        expect(scope.has(AuthService)).toBe(true);
      });

      await ctx.close();
    });

    it('should list module providers from scope', async () => {
      class PluginC {
        name = 'C';
      }

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(
          class PluginA {
            name = 'A';
          },
          { tags: ['plugin'] },
        ),
      );
      ctx.mount(
        {
          getRegistrations: () => [provide(PluginC, { tags: ['plugin'] })],
          getRequirements: () => [],
        },
        'plugins',
      );
      await ctx.start();

      await ctx.scope(async (scope) => {
        const plugins = scope.list<{ name: string }>({ tags: 'plugin' });
        expect(plugins).toHaveLength(2);
      });

      await ctx.close();
    });
  });

  describe('Symbol.asyncDispose', () => {
    it('should auto-close via await using pattern', async () => {
      class Service {}
      const ctx = new ContainerContext('test');
      ctx.register(provide(Service));
      await ctx.start();
      expect(ctx.has(Service)).toBe(true);

      await ctx[Symbol.asyncDispose]();

      // Context should be closed now
      expect(() => ctx.get(Service)).toThrow(ContainerClosedError);
    });

    it('should be safe to call when not started', async () => {
      const ctx = new ContainerContext('test');
      // Should not throw
      await ctx[Symbol.asyncDispose]();
    });
  });

  describe('fork + override', () => {
    it('should fork and override a provider', async () => {
      class Database {
        query() {
          return 'real-data';
        }
      }
      class UserService {
        constructor(readonly db: Database) {}
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.register(provide(UserService, { deps: [Database] }));

      class MockDatabase extends Database {
        query() {
          return 'mock-data';
        }
      }

      const testCtx = ctx.fork().override(Database, () => new MockDatabase());

      await testCtx.start();

      const userService = testCtx.get(UserService);
      expect(userService.db.query()).toBe('mock-data');

      await testCtx.close();
    });

    it('should not affect original context', async () => {
      class Database {
        query() {
          return 'real';
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));

      ctx.fork().override(Database, () => {
        const db = new Database();
        db.query = () => 'mock';
        return db;
      });

      await ctx.start();
      expect(ctx.get(Database).query()).toBe('real');
      await ctx.close();
    });

    it('should add new providers via override', async () => {
      class Service {}
      class Extra {}

      const ctx = new ContainerContext('test');
      ctx.register(provide(Service));
      const testCtx = ctx.fork().override(Extra, () => new Extra());

      await testCtx.start();
      expect(testCtx.get(Service)).toBeInstanceOf(Service);
      expect(testCtx.get(Extra)).toBeInstanceOf(Extra);
      await testCtx.close();
    });

    it('should preserve mounted module containers in the fork', async () => {
      // Regression: fork() previously copied only root registrations, so a
      // module()-composed app lost its module containers and get(ModuleService)
      // threw NotRegisteredError in the fork.
      class Database {
        query() {
          return 'real-data';
        }
      }
      class ModuleService {
        constructor(readonly db: Database) {}
      }
      class MockDatabase extends Database {
        query() {
          return 'mock-data';
        }
      }

      const moduleHolder: ContainerHolder = {
        getRegistrations: () => [provide(ModuleService, { deps: [Database] })],
        getRequirements: () => [Database],
      };

      const ctx = new ContainerContext('app');
      ctx.register(provide(Database));
      ctx.mount(moduleHolder, 'mod');

      const testCtx = ctx.fork().override(Database, () => new MockDatabase());
      await testCtx.start();

      // (a) module-scoped provider still resolves in the fork
      const moduleService = testCtx.get(ModuleService);
      expect(moduleService).toBeInstanceOf(ModuleService);
      // (b) the root override is honored through the module's parent chain
      expect(moduleService.db.query()).toBe('mock-data');

      await testCtx.close();
    });
  });

  describe('refresh (dynamic providers)', () => {
    it('should refresh dynamic providers', async () => {
      let counter = 0;
      const Config = named<number>('config');

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(
          Config,
          () => {
            counter++;
            return counter;
          },
          { dynamic: true },
        ),
      );

      await ctx.start();
      expect(ctx.get(Config)).toBe(1);

      await ctx.refresh();
      expect(ctx.get(Config)).toBe(2);

      await ctx.close();
    });

    it('should not refresh non-dynamic providers', async () => {
      let counter = 0;
      const Value = named<number>('value');

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(Value, () => {
          counter++;
          return counter;
        }),
      );

      await ctx.start();
      expect(ctx.get(Value)).toBe(1);

      await ctx.refresh();
      expect(ctx.get(Value)).toBe(1); // unchanged

      await ctx.close();
    });

    it('should run onClose for old instance on refresh', async () => {
      const closedValues: number[] = [];
      let counter = 0;
      const Config = named<number>('config');

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(
          Config,
          () => {
            counter++;
            return counter;
          },
          {
            dynamic: true,
            onClose: (v) => {
              closedValues.push(v);
            },
          },
        ),
      );

      await ctx.start();
      expect(ctx.get(Config)).toBe(1);

      await ctx.refresh();
      expect(closedValues).toEqual([1]);
      expect(ctx.get(Config)).toBe(2);

      await ctx.close();
    });
  });

  describe('tracing', () => {
    it('should apply tracing proxy when enabled', async () => {
      class Database {
        query() {
          return 'data';
        }
      }

      const traces: TraceData[] = [];
      const sink: TraceSink = { emit: (data) => traces.push(data) };

      const ctx = new ContainerContext('test', { proxy: { tracing: true }, traceSink: sink });
      ctx.register(provide(Database));

      await ctx.start();

      const db = ctx.get(Database);
      db.query();

      expect(traces.length).toBe(1);
      expect(traces[0].method).toBe('query');
      expect(traces[0].token).toBe('Database');

      await ctx.close();
    });

    it('should respect per-provider proxy: false opt-out', async () => {
      class Traced {
        call() {
          return 'traced';
        }
      }
      class Untraced {
        call() {
          return 'untraced';
        }
      }

      const traces: TraceData[] = [];
      const sink: TraceSink = { emit: (data) => traces.push(data) };

      const ctx = new ContainerContext('test', { proxy: { tracing: true }, traceSink: sink });
      ctx.register(provide(Traced));
      ctx.register(provide(Untraced, { proxy: false }));

      await ctx.start();

      ctx.get(Traced).call();
      ctx.get(Untraced).call();

      // Only Traced should have emitted
      const tracedCalls = traces.filter((t) => t.token === 'Traced');
      const untracedCalls = traces.filter((t) => t.token === 'Untraced');
      expect(tracedCalls.length).toBe(1);
      expect(untracedCalls.length).toBe(0);

      await ctx.close();
    });
  });

  describe('describe()', () => {
    it('should return container description', async () => {
      class Database {}
      class UserService {}

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.register(provide(UserService, { deps: [Database] }));

      const desc = ctx.describe();
      expect(desc.name).toBe('test');
      expect(desc.providers).toHaveLength(2);
    });

    it('should include mounted module containers', async () => {
      class Database {}
      class AuthService {}

      const authHolder: ContainerHolder = {
        getRegistrations: () => [provide(AuthService)],
        getRequirements: () => [],
      };

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      ctx.mount(authHolder, 'auth');

      const desc = ctx.describe();
      expect(desc.children).toHaveLength(1);
      expect(desc.children[0].name).toBe('auth');
    });

    it('should include validation issues with validate: true', () => {
      class Service {}
      class Missing {}

      const ctx = new ContainerContext('test');
      ctx.register(provide(Service, { deps: [Missing] }));

      const desc = ctx.describe({ validate: true });
      expect(desc.issues).toBeDefined();
      expect(desc.issues?.length).toBeGreaterThan(0);
      expect(desc.issues?.[0].type).toBe('missing-dependency');
    });
  });

  describe('lazy providers', () => {
    it('should not resolve lazy providers during start', async () => {
      let resolved = false;
      class ExpensiveService {
        constructor() {
          resolved = true;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(ExpensiveService, { lazy: true }));

      await ctx.start();
      expect(resolved).toBe(false);

      await ctx.close();
    });
  });

  describe('createScope (detached scope)', () => {
    it('should create a detached scope that resolves singletons', async () => {
      class Database {
        query() {
          return 'data';
        }
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(Database));
      await ctx.start();

      const { scope, close } = await ctx.createScope();
      expect(scope.get(Database).query()).toBe('data');
      await close();

      await ctx.close();
    });

    it('should create a detached scope that resolves scoped providers', async () => {
      class RequestId {
        id = crypto.randomUUID();
      }

      const ctx = new ContainerContext('test');
      ctx.register(provide(RequestId, { scope: 'scoped' }));
      await ctx.start();

      const { scope: scope1, close: close1 } = await ctx.createScope();
      const { scope: scope2, close: close2 } = await ctx.createScope();

      const id1 = scope1.get(RequestId).id;
      const id2 = scope2.get(RequestId).id;

      // Same within scope
      expect(scope1.get(RequestId).id).toBe(id1);
      // Different across scopes
      expect(id1).not.toBe(id2);

      await close1();
      await close2();
      await ctx.close();
    });

    it('should run onClose hooks when detached scope is closed', async () => {
      let closed = false;
      class ScopedResource {}

      const ctx = new ContainerContext('test');
      ctx.register(
        provide(ScopedResource, () => new ScopedResource(), {
          scope: 'scoped',
          onClose: () => {
            closed = true;
          },
        }),
      );

      await ctx.start();

      const { scope, close } = await ctx.createScope();
      scope.get(ScopedResource);
      expect(closed).toBe(false);

      await close();
      expect(closed).toBe(true);

      await ctx.close();
    });

    it('should support has() and list() on detached scope', async () => {
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

      const { scope, close } = await ctx.createScope();
      expect(scope.has(PluginA)).toBe(true);
      const plugins = scope.list<{ name: string }>({ tags: 'plugin' });
      expect(plugins).toHaveLength(2);

      await close();
      await ctx.close();
    });

    it('should resolve module providers from detached scope', async () => {
      class AuthService {
        validate() {
          return true;
        }
      }

      const ctx = new ContainerContext('test');
      ctx.mount(
        {
          getRegistrations: () => [provide(AuthService)],
          getRequirements: () => [],
        },
        'auth',
      );
      await ctx.start();

      const { scope, close } = await ctx.createScope();
      expect(scope.get(AuthService).validate()).toBe(true);
      expect(scope.has(AuthService)).toBe(true);
      await close();

      await ctx.close();
    });

    it('should throw when context is not started', async () => {
      const ctx = new ContainerContext('test');
      await expect(ctx.createScope()).rejects.toThrow('requires the context to be started');
    });

    it('should throw when context is closed', async () => {
      const ctx = new ContainerContext('test');
      await ctx.start();
      await ctx.close();
      await expect(ctx.createScope()).rejects.toThrow(ContainerClosedError);
    });
  });

  describe('debug mode', () => {
    it('should not throw when debug is enabled', async () => {
      class Service {}
      const ctx = new ContainerContext('test', { debug: true });
      ctx.register(provide(Service));

      await ctx.start();
      const svc = ctx.get(Service);
      expect(svc).toBeInstanceOf(Service);

      // Scope should also work with debug enabled
      await ctx.scope(async (scope) => {
        expect(scope.has(Service)).toBe(true);
      });

      await ctx.close();
    });

    it('should log scope proxy creation once per token pair', async () => {
      class RequestId {
        id = crypto.randomUUID();
      }
      class Logger {
        constructor(readonly reqId: RequestId) {}
      }

      const ctx = new ContainerContext('test', { debug: true });
      ctx.register(provide(RequestId, { scope: 'scoped' }));
      ctx.register(
        provide(Logger, (resolve) => new Logger(resolve(RequestId)), {
          deps: [RequestId],
        }),
      );

      await ctx.start();

      // Logger should be resolved (singleton) and the scope proxy should be logged
      const logger = ctx.get(Logger);
      expect(logger).toBeInstanceOf(Logger);

      await ctx.close();
    });
  });
});
