import { describe, expect, it } from 'bun:test';
import { specTest } from '../../src/spectest';
import { Container } from '../../src/inject/container';
import {
  CircularDependencyError,
  ContainerClosedError,
  DuplicateProviderError,
  NotRegisteredError,
} from '../../src/inject/errors';
import { provide } from '../../src/inject/provider';
import { named, tokenName } from '../../src/inject/token';

describe('Container', () => {
  describe('basic resolution', () => {
    it('should resolve a registered class provider', () => {
      class AppConfig {
        port = 3000;
      }

      const container = new Container('test');
      container.register(provide(AppConfig));

      const config = container.get(AppConfig);
      expect(config).toBeInstanceOf(AppConfig);
      expect(config.port).toBe(3000);
    });

    specTest(
      'should return the same singleton instance on multiple gets',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'singleton-identity',
        check: 'singleton-single-construction',
      },
      () => {
        class Counter {
          value = 0;
        }

        const container = new Container('test');
        container.register(provide(Counter));

        const a = container.get(Counter);
        a.value = 42;
        const b = container.get(Counter);
        expect(b.value).toBe(42);
        expect(a).toBe(b);
      },
    );

    it('should resolve dependencies via deps option', () => {
      class Database {
        query(sql: string) {
          return `result: ${sql}`;
        }
      }
      class UserRepo {
        constructor(public db: Database) {}
      }

      const container = new Container('test');
      container.register(provide(Database));
      container.register(provide(UserRepo, { deps: [Database] }));

      const repo = container.get(UserRepo);
      expect(repo.db).toBeInstanceOf(Database);
      expect(repo.db.query('SELECT 1')).toBe('result: SELECT 1');
    });

    it('should resolve factory providers', () => {
      class Database {
        constructor(public url: string) {}
      }

      const container = new Container('test');
      container.register(provide(Database, () => new Database('postgres://localhost')));

      const db = container.get(Database);
      expect(db.url).toBe('postgres://localhost');
    });

    it('should resolve factory with resolve function', () => {
      class Config {
        url = 'postgres://localhost';
      }
      class Database {
        constructor(public url: string) {}
      }

      const container = new Container('test');
      container.register(provide(Config));
      container.register(
        provide(Database, (resolve) => {
          const config = resolve(Config);
          return new Database(config.url);
        }),
      );

      const db = container.get(Database);
      expect(db.url).toBe('postgres://localhost');
    });
  });

  describe('named tokens', () => {
    it('should resolve named tokens', () => {
      const Version = named<string>('version');

      const container = new Container('test');
      container.register(provide(Version, () => '2.0.0'));

      expect(container.get(Version)).toBe('2.0.0');
    });
  });

  describe('tagged providers', () => {
    it('should resolve all tagged providers via list()', () => {
      class PluginA {
        name = 'A';
      }
      class PluginB {
        name = 'B';
      }

      const container = new Container('test');
      container.register(provide(PluginA, { tags: ['plugin'] }));
      container.register(provide(PluginB, { tags: ['plugin'] }));

      const plugins = container.list<{ name: string }>({ tags: 'plugin' });
      expect(plugins).toHaveLength(2);
      expect(plugins[0].name).toBe('A');
      expect(plugins[1].name).toBe('B');
    });

    it('should return empty array for unknown tag', () => {
      const container = new Container('test');
      expect(container.list({ tags: 'nonexistent' })).toEqual([]);
    });

    it('should filter by multiple tags (AND logic)', () => {
      class SecurePlugin {
        name = 'secure';
      }
      class PublicPlugin {
        name = 'public';
      }

      const container = new Container('test');
      container.register(provide(SecurePlugin, { tags: ['plugin', 'secure'] }));
      container.register(provide(PublicPlugin, { tags: ['plugin'] }));

      const securePlugins = container.list<{ name: string }>({ tags: ['plugin', 'secure'] });
      expect(securePlugins).toHaveLength(1);
      expect(securePlugins[0].name).toBe('secure');
    });
  });

  describe('has()', () => {
    it('should return true for registered tokens', () => {
      class Service {}
      const container = new Container('test');
      container.register(provide(Service));
      expect(container.has(Service)).toBe(true);
    });

    it('should return false for unregistered tokens', () => {
      class Service {}
      const container = new Container('test');
      expect(container.has(Service)).toBe(false);
    });
  });

  describe('parent chain', () => {
    it('should resolve from parent container', () => {
      class Database {}

      const parent = new Container('parent');
      parent.register(provide(Database));

      const child = parent.createChild('child');
      expect(child.get(Database)).toBeInstanceOf(Database);
    });

    it('should prefer local over parent', () => {
      class Config {
        constructor(public value: string) {}
      }

      const parent = new Container('parent');
      parent.register(provide(Config, () => new Config('parent')));

      const child = parent.createChild('child');
      child.register(provide(Config, () => new Config('child')));

      expect(child.get(Config).value).toBe('child');
      expect(parent.get(Config).value).toBe('parent');
    });

    it('should resolve tagged across hierarchy via list()', () => {
      class PluginA {
        name = 'A';
      }
      class PluginB {
        name = 'B';
      }

      const parent = new Container('parent');
      parent.register(provide(PluginA, { tags: ['plugin'] }));

      const child = parent.createChild('child');
      child.register(provide(PluginB, { tags: ['plugin'] }));

      const plugins = child.list({ tags: 'plugin' });
      expect(plugins).toHaveLength(2);
    });

    it('should check has() through parent chain', () => {
      class Service {}
      const parent = new Container('parent');
      parent.register(provide(Service));

      const child = parent.createChild('child');
      expect(child.has(Service)).toBe(true);
    });
  });

  describe('visibility', () => {
    it('should hide private providers from child containers', () => {
      class InternalService {}

      const parent = new Container('parent');
      parent.register(provide(InternalService, { visibility: 'private' }));

      // Parent can access it
      expect(parent.get(InternalService)).toBeInstanceOf(InternalService);

      // Child cannot
      const child = parent.createChild('child');
      expect(() => child.get(InternalService)).toThrow(NotRegisteredError);
    });

    it('should allow public providers from child containers', () => {
      class PublicService {}

      const parent = new Container('parent');
      parent.register(provide(PublicService, { visibility: 'public' }));

      const child = parent.createChild('child');
      expect(child.get(PublicService)).toBeInstanceOf(PublicService);
    });

    it('should hide private providers from has() in child', () => {
      class InternalService {}

      const parent = new Container('parent');
      parent.register(provide(InternalService, { visibility: 'private' }));

      const child = parent.createChild('child');
      expect(child.has(InternalService)).toBe(false);
      expect(parent.has(InternalService)).toBe(true);
    });
  });

  describe('error handling', () => {
    it('should throw NotRegisteredError for unknown token', () => {
      class Unknown {}
      const container = new Container('test');
      expect(() => container.get(Unknown)).toThrow(NotRegisteredError);
    });

    it('should throw DuplicateProviderError for duplicate registration', () => {
      class Service {}
      const container = new Container('test');
      container.register(provide(Service));
      expect(() => container.register(provide(Service))).toThrow(DuplicateProviderError);
    });

    it('should throw ContainerClosedError after close', async () => {
      class Service {}
      const container = new Container('test');
      container.register(provide(Service));
      await container.close();
      expect(() => container.get(Service)).toThrow(ContainerClosedError);
    });

    it('should detect circular dependencies at runtime', () => {
      class A {
        constructor(public b: unknown) {}
      }
      class B {
        constructor(public a: unknown) {}
      }

      const container = new Container('test');
      container.register(provide(A, { deps: [B] }));
      container.register(provide(B, { deps: [A] }));

      expect(() => container.get(A)).toThrow(CircularDependencyError);
    });

    it('should include resolution chain in NotRegisteredError', () => {
      class Config {}
      class Database {
        constructor(public config: Config) {}
      }
      class Missing {}
      class UserService {
        constructor(
          public db: Database,
          public missing: Missing,
        ) {}
      }

      const container = new Container('test');
      container.register(provide(Config));
      container.register(provide(Database, { deps: [Config] }));
      container.register(
        provide(UserService, (resolve) => {
          const db = resolve(Database);
          const missing = resolve(Missing);
          return new UserService(db, missing);
        }),
      );

      try {
        container.get(UserService);
        expect.unreachable('should have thrown');
      } catch (err) {
        expect(err).toBeInstanceOf(NotRegisteredError);
        const e = err as NotRegisteredError;
        expect(e.message).toContain('Missing');
        expect(e.message).toContain('Resolution chain');
        expect(e.message).toContain('Hint:');
        expect(e.message).toContain('.provide(Missing)');
        expect(e.resolutionChain.length).toBeGreaterThan(0);
      }
    });

    it('should include the originating chain for a cross-container dynamic miss', () => {
      // A child/module provider whose factory dynamically resolves an undeclared
      // token. The miss walks up to the (empty-resolving) root; the thrown error
      // must still carry the originating provider in its resolution chain.
      class Missing {}
      class Dependent {
        constructor(public missing: unknown) {}
      }

      const root = new Container('root');
      const child = root.createChild('module');
      child.register(provide(Dependent, (resolve) => new Dependent(resolve(Missing)), { deps: [] }));

      try {
        child.get(Dependent);
        expect.unreachable('should have thrown');
      } catch (err) {
        expect(err).toBeInstanceOf(NotRegisteredError);
        const e = err as NotRegisteredError;
        expect(e.message).toContain('Missing');
        expect(e.message).toContain('Resolution chain');
        // The chain must include the originating provider, not be empty.
        expect(e.resolutionChain.length).toBeGreaterThan(0);
        expect(e.resolutionChain.map(tokenName)).toContain('Dependent');
      }
    });
  });

  describe('validation', () => {
    it('should detect missing dependencies', () => {
      class Unknown {}
      class Service {}

      const container = new Container('test');
      container.register(provide(Service, { deps: [Unknown] }));

      const result = container.validate();
      expect(result.valid).toBe(false);
      expect(result.issues).toHaveLength(1);
      expect(result.issues[0].type).toBe('missing-dependency');
    });

    it('should detect circular dependencies statically', () => {
      class A {}
      class B {}

      const container = new Container('test');
      container.register(provide(A, { deps: [B] }));
      container.register(provide(B, { deps: [A] }));

      const result = container.validate();
      const cycles = result.issues.filter((i) => i.type === 'circular-dependency');
      expect(cycles.length).toBeGreaterThan(0);
    });

    it('should detect scope violations', () => {
      class RequestData {}
      class Singleton {}

      const container = new Container('test');
      container.register(provide(RequestData, { scope: 'scoped' }));
      container.register(provide(Singleton, { deps: [RequestData] }));

      const result = container.validate();
      const violations = result.issues.filter((i) => i.type === 'scope-violation');
      expect(violations.length).toBeGreaterThan(0);
    });

    it('should pass validation for valid container', () => {
      class Database {}
      class UserService {}

      const container = new Container('test');
      container.register(provide(Database));
      container.register(provide(UserService, { deps: [Database] }));

      const result = container.validate();
      expect(result.valid).toBe(true);
    });
  });

  describe('resolve.all() in factories', () => {
    it('should resolve tagged providers via resolve.all() with FilterOptions', () => {
      class PluginA {
        name = 'A';
      }
      class PluginB {
        name = 'B';
      }
      class PluginManager {
        // biome-ignore lint/suspicious/noExplicitAny: test
        constructor(public plugins: any[]) {}
      }

      const container = new Container('test');
      container.register(provide(PluginA, { tags: ['plugin'] }));
      container.register(provide(PluginB, { tags: ['plugin'] }));
      container.register(
        provide(PluginManager, (resolve) => {
          const plugins = resolve.all({ tags: 'plugin' });
          return new PluginManager(plugins);
        }),
      );

      const manager = container.get(PluginManager);
      expect(manager.plugins).toHaveLength(2);
      expect(manager.plugins[0]).toBeInstanceOf(PluginA);
      expect(manager.plugins[1]).toBeInstanceOf(PluginB);
    });

    it('should return empty array for unknown tags in resolve.all()', () => {
      class Service {
        // biome-ignore lint/suspicious/noExplicitAny: test
        constructor(public items: any[]) {}
      }

      const container = new Container('test');
      container.register(provide(Service, (resolve) => new Service(resolve.all({ tags: 'nonexistent' }))));

      const svc = container.get(Service);
      expect(svc.items).toEqual([]);
    });
  });

  describe('dynamic providers', () => {
    it('should refresh dynamic providers', async () => {
      let counter = 0;
      class Config {
        constructor(public value: number) {}
      }

      const container = new Container('test');
      container.register(
        provide(
          Config,
          () => {
            counter++;
            return new Config(counter);
          },
          { dynamic: true },
        ),
      );

      await container.resolveAll();
      expect(container.get(Config).value).toBe(1);

      await container.refreshDynamic();
      expect(container.get(Config).value).toBe(2);
    });

    it('should run onClose for old instance before refresh', async () => {
      let closedValue: number | undefined;
      class Config {
        constructor(public value: number) {}
      }

      let counter = 0;
      const container = new Container('test');
      container.register(
        provide(
          Config,
          () => {
            counter++;
            return new Config(counter);
          },
          {
            dynamic: true,
            onClose: (instance) => {
              closedValue = instance.value;
            },
          },
        ),
      );

      await container.resolveAll();
      expect(container.get(Config).value).toBe(1);

      await container.refreshDynamic();
      expect(closedValue).toBe(1);
      expect(container.get(Config).value).toBe(2);
    });

    it('should not refresh non-dynamic providers', async () => {
      let counter = 0;
      class Service {
        constructor(public value: number) {}
      }

      const container = new Container('test');
      container.register(
        provide(Service, () => {
          counter++;
          return new Service(counter);
        }),
      );

      await container.resolveAll();
      expect(container.get(Service).value).toBe(1);

      await container.refreshDynamic();
      // Should still be 1 - not refreshed
      expect(container.get(Service).value).toBe(1);
    });
  });

  describe('resolveAll dependency ordering', () => {
    it('resolves a sync singleton registered BEFORE its async dependency', async () => {
      // The dependent is registered first, so registration order does NOT match
      // dependency order. resolveAll must still resolve the async dep first.
      class AsyncDep {
        ready = false;
      }
      class SyncDependent {
        constructor(public dep: AsyncDep) {}
      }

      const container = new Container('test');
      container.register(
        provide(SyncDependent, (resolve) => new SyncDependent(resolve(AsyncDep)), { deps: [AsyncDep] }),
      );
      container.register(
        provide(AsyncDep, async () => {
          const d = new AsyncDep();
          d.ready = true;
          return d;
        }),
      );

      await container.resolveAll();

      const dependent = container.get(SyncDependent);
      expect(dependent.dep).toBeInstanceOf(AsyncDep);
      expect(dependent.dep.ready).toBe(true);
    });

    it('resolves regardless of registration order (async dep registered first)', async () => {
      class AsyncDep {
        ready = false;
      }
      class SyncDependent {
        constructor(public dep: AsyncDep) {}
      }

      const container = new Container('test');
      container.register(
        provide(AsyncDep, async () => {
          const d = new AsyncDep();
          d.ready = true;
          return d;
        }),
      );
      container.register(
        provide(SyncDependent, (resolve) => new SyncDependent(resolve(AsyncDep)), { deps: [AsyncDep] }),
      );

      await container.resolveAll();
      expect(container.get(SyncDependent).dep.ready).toBe(true);
    });

    it('instantiates dependencies before dependents (topological order)', async () => {
      const order: string[] = [];
      class A {
        constructor() {
          order.push('A');
        }
      }
      class B {
        constructor(public a: A) {
          order.push('B');
        }
      }
      class C {
        constructor(public b: B) {
          order.push('C');
        }
      }

      const container = new Container('test');
      // Register dependents before dependencies.
      container.register(provide(C, { deps: [B] }));
      container.register(provide(B, { deps: [A] }));
      container.register(provide(A));

      await container.resolveAll();
      expect(order).toEqual(['A', 'B', 'C']);
    });

    it('resolves a chain of async singletons in dependency order', async () => {
      const order: string[] = [];
      class Config {
        url = 'postgres://localhost';
      }
      class Database {
        constructor(public url: string) {}
      }
      class Repo {
        constructor(public db: Database) {}
      }

      const container = new Container('test');
      // Repo (sync) depends on Database (async) depends on Config (async).
      container.register(provide(Repo, (resolve) => new Repo(resolve(Database)), { deps: [Database] }));
      container.register(
        provide(
          Database,
          async (resolve) => {
            order.push('Database');
            return new Database(resolve(Config).url);
          },
          { deps: [Config] },
        ),
      );
      container.register(
        provide(Config, async () => {
          order.push('Config');
          return new Config();
        }),
      );

      await container.resolveAll();
      expect(order).toEqual(['Config', 'Database']);
      expect(container.get(Repo).db.url).toBe('postgres://localhost');
    });

    it('does not pre-resolve standalone lazy providers', async () => {
      let lazyResolved = false;
      class LazyDep {
        constructor() {
          lazyResolved = true;
        }
      }
      class Eager {
        constructor(public lazy: LazyDep) {}
      }

      const container = new Container('test');
      container.register(provide(LazyDep, { lazy: true }));
      container.register(provide(Eager, { deps: [LazyDep] }));

      // The eager provider depends on a lazy one. resolveAll skips LazyDep in
      // its dependency-order traversal, then resolving Eager pulls LazyDep via
      // the factory's normal dependency resolution. Standalone lazy providers
      // are never eagerly resolved.
      class StandaloneLazy {
        constructor() {
          throw new Error('lazy provider should not be resolved during resolveAll');
        }
      }
      container.register(provide(StandaloneLazy, { lazy: true }));

      await container.resolveAll();
      // Eager was resolved, pulling LazyDep through its constructor deps.
      expect(lazyResolved).toBe(true);
      // StandaloneLazy was never touched.
      expect(container.getInstance(StandaloneLazy)).toBeUndefined();
    });

    it('does not loop forever on a dependency cycle and surfaces the cycle', async () => {
      class A {
        constructor(public b: unknown) {}
      }
      class B {
        constructor(public a: unknown) {}
      }

      const container = new Container('test');
      container.register(provide(A, { deps: [B] }));
      container.register(provide(B, { deps: [A] }));

      // resolveAll must terminate (not hang) and the cycle is reported by
      // instantiate()'s circular-dependency detection.
      await expect(container.resolveAll()).rejects.toThrow(CircularDependencyError);
    });
  });

  describe('getInstance / setInstance', () => {
    it('should get and set cached instances', () => {
      class Service {
        constructor(public value: string) {}
      }

      const container = new Container('test');
      container.register(provide(Service, () => new Service('original')));

      // Resolve first
      const original = container.get(Service);
      expect(original.value).toBe('original');

      // Replace with wrapper
      const wrapped = new Service('wrapped');
      container.setInstance(Service, wrapped);

      expect(container.get(Service)).toBe(wrapped);
      expect(container.getInstance(Service)).toBe(wrapped);
    });
  });

  describe('close lifecycle', () => {
    it('should call onClose hooks in reverse order', async () => {
      const order: string[] = [];
      class A {}
      class B {}

      const container = new Container('test');
      container.register(provide(A, { onClose: () => order.push('A') } as never));
      container.register(provide(B, { onClose: () => order.push('B') } as never));

      // Force instantiation
      container.get(A);
      container.get(B);

      await container.close();
      expect(order).toEqual(['B', 'A']);
    });

    it('should close children before parent', async () => {
      const order: string[] = [];
      class ParentService {}
      class ChildService {}

      const parent = new Container('parent');
      parent.register(provide(ParentService, { onClose: () => order.push('parent') } as never));

      const child = parent.createChild('child');
      child.register(provide(ChildService, { onClose: () => order.push('child') } as never));

      parent.get(ParentService);
      child.get(ChildService);

      await parent.close();
      expect(order).toEqual(['child', 'parent']);
    });

    it('should run all close hooks even when a middle one throws', async () => {
      const order: string[] = [];
      class A {}
      class B {}
      class C {}

      const container = new Container('test');
      // Registration order A, B, C -> reverse close order C, B, A.
      container.register(provide(A, { onClose: () => order.push('A') } as never));
      container.register(
        provide(B, {
          onClose: async () => {
            order.push('B');
            throw new Error('boom');
          },
        } as never),
      );
      container.register(provide(C, { onClose: () => order.push('C') } as never));

      container.get(A);
      container.get(B);
      container.get(C);

      // Must not reject even though the middle hook throws.
      await expect(container.close()).resolves.toBeUndefined();

      // Every hook ran, including the ones registered before the failing one.
      expect(order).toEqual(['C', 'B', 'A']);
      // Container teardown completed regardless of the hook failure.
      expect(container.describe().closed).toBe(true);
    });

    it('should not let one child close failure abort the rest', async () => {
      const order: string[] = [];
      class Svc {}

      const parent = new Container('parent');

      const child1 = parent.createChild('child1');
      child1.register(provide(Svc, { onClose: () => order.push('child1') } as never));
      child1.get(Svc);

      const child2 = parent.createChild('child2');
      child2.register(
        provide(Svc, {
          onClose: () => {
            throw new Error('child2 boom');
          },
        } as never),
      );
      child2.get(Svc);

      const child3 = parent.createChild('child3');
      child3.register(provide(Svc, { onClose: () => order.push('child3') } as never));
      child3.get(Svc);

      await expect(parent.close()).resolves.toBeUndefined();

      // Children closed in reverse order; child2's failure did not strand child1.
      expect(order).toEqual(['child3', 'child1']);
      expect(parent.describe().closed).toBe(true);
    });
  });

  describe('describe()', () => {
    it('should return container description with providers', () => {
      class Database {}
      class UserService {}

      const container = new Container('test');
      container.register(provide(Database));
      container.register(provide(UserService, { deps: [Database], tags: ['service'] }));

      container.get(Database);

      const desc = container.describe();
      expect(desc.name).toBe('test');
      expect(desc.closed).toBe(false);
      expect(desc.providers).toHaveLength(2);

      const dbDesc = desc.providers.find((p) => p.token === 'Database');
      expect(dbDesc).toBeDefined();
      expect(dbDesc?.resolved).toBe(true);
      expect(dbDesc?.scope).toBe('singleton');

      const userDesc = desc.providers.find((p) => p.token === 'UserService');
      expect(userDesc).toBeDefined();
      expect(userDesc?.resolved).toBe(false);
      expect(userDesc?.tags).toEqual(['service']);
      expect(userDesc?.deps).toEqual(['Database']);
    });

    it('should include children in description', () => {
      const parent = new Container('parent');
      const child = parent.createChild('child');

      class Service {}
      child.register(provide(Service));

      const desc = parent.describe();
      expect(desc.children).toHaveLength(1);
      expect(desc.children[0].name).toBe('child');
      expect(desc.children[0].providers).toHaveLength(1);
    });

    it('should include lazy and dynamic flags in description', () => {
      class Config {}

      const container = new Container('test');
      container.register(
        provide(Config, () => new Config(), {
          dynamic: true,
          lazy: true,
        }),
      );

      const desc = container.describe();
      const configDesc = desc.providers[0];
      expect(configDesc.dynamic).toBe(true);
      expect(configDesc.lazy).toBe(true);
    });

    it('should not include issues by default', () => {
      class Service {}
      class Missing {}

      const container = new Container('test');
      container.register(provide(Service, { deps: [Missing] }));

      const desc = container.describe();
      expect(desc.issues).toBeUndefined();
    });

    it('should include validation issues when validate: true', () => {
      class Service {}
      class Missing {}

      const container = new Container('test');
      container.register(provide(Service, { deps: [Missing] }));

      const desc = container.describe({ validate: true });
      expect(desc.issues).toBeDefined();
      expect(desc.issues?.length).toBeGreaterThan(0);
      expect(desc.issues?.[0].type).toBe('missing-dependency');
    });

    it('should include empty issues array when valid and validate: true', () => {
      class Database {}

      const container = new Container('test');
      container.register(provide(Database));

      const desc = container.describe({ validate: true });
      expect(desc.issues).toBeDefined();
      expect(desc.issues).toHaveLength(0);
    });
  });

  describe('async resolution concurrency', () => {
    specTest(
      'should run the async factory exactly once for concurrent resolveAsync calls',
      {
        feature: 'typescript/dependency-injection',
        requirement: 'singleton-identity',
        check: 'singleton-concurrent-async-once',
      },
      async () => {
        // Regression: without an in-flight Promise cache, two concurrent
        // resolutions both miss the instance cache and run the factory twice,
        // duplicating singletons (DB pools, clients, ...).
        let factoryRuns = 0;

        class Pool {
          readonly id = factoryRuns;
        }

        const container = new Container('test');
        container.register(
          provide(Pool, async () => {
            factoryRuns++;
            // Yield so a second caller can race in before the instance is cached.
            await Promise.resolve();
            return new Pool();
          }),
        );

        const provider = container.findProvider(Pool);
        if (!provider) throw new Error('provider not found');

        const [a, b] = await Promise.all([container.resolveAsync(provider), container.resolveAsync(provider)]);

        expect(factoryRuns).toBe(1);
        // Both callers receive the same singleton instance.
        expect(a).toBe(b);
      },
    );

    it('should de-duplicate many concurrent resolveAsync calls', async () => {
      let factoryRuns = 0;
      class Client {}

      const container = new Container('test');
      container.register(
        provide(Client, async () => {
          factoryRuns++;
          await Promise.resolve();
          return new Client();
        }),
      );

      const provider = container.findProvider(Client);
      if (!provider) throw new Error('provider not found');

      const instances = await Promise.all(Array.from({ length: 10 }, () => container.resolveAsync(provider)));

      expect(factoryRuns).toBe(1);
      // Every caller got the identical instance.
      expect(new Set(instances).size).toBe(1);
    });

    it('should return the cached instance on a later resolveAsync after the first settles', async () => {
      let factoryRuns = 0;
      class Service {}

      const container = new Container('test');
      container.register(
        provide(Service, async () => {
          factoryRuns++;
          await Promise.resolve();
          return new Service();
        }),
      );

      const provider = container.findProvider(Service);
      if (!provider) throw new Error('provider not found');

      const first = await container.resolveAsync(provider);
      const second = await container.resolveAsync(provider);

      expect(factoryRuns).toBe(1);
      expect(first).toBe(second);
    });
  });

  describe('sync factory returning a Promise', () => {
    it('throws a clear error instead of caching the pending Promise', () => {
      // Regression: `isAsyncFactory()` keys on the `async` keyword, so
      // a plain arrow that forwards a Promise is classified sync. Without the
      // guard, `instantiate()` caches the pending Promise and `get()` silently
      // returns an unresolved Promise of the wrong type.
      class Database {}

      const container = new Container('test');
      container.register(
        // Not declared `async`, but returns a Promise — the exact silent-failure case.
        provide(Database, () => Promise.resolve(new Database())),
      );

      expect(() => container.get(Database)).toThrow(/not declared async/);
      // The message names the offending token so the author can find it.
      expect(() => container.get(Database)).toThrow(/Database/);
    });

    it('does not cache a Promise as the resolved instance', () => {
      class Client {}

      const container = new Container('test');
      container.register(provide(Client, () => Promise.resolve(new Client())));

      let caught: unknown;
      try {
        container.get(Client);
      } catch (err) {
        caught = err;
      }
      expect(caught).toBeInstanceOf(Error);
      // Nothing was cached, so a re-resolve throws again rather than returning a Promise.
      expect(() => container.get(Client)).toThrow();
    });
  });
});
