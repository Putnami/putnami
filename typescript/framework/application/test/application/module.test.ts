import { describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { composeModules, module, Module, type Plugin } from '../../src/application';

describe('Module', () => {
  describe('module()', () => {
    it('should create a module with a name', () => {
      const mod = module('auth');
      expect(mod).toBeInstanceOf(Module);
      expect(mod.name).toBe('auth');
    });
  });

  describe('use()', () => {
    it('should add a plugin', () => {
      const plugin: Plugin = {};
      const mod = module('test').use(plugin);
      expect(mod.getPlugins()).toHaveLength(1);
      expect(mod.getPlugins()[0]).toBe(plugin);
    });

    it('should add a sub-module', () => {
      const child = module('child');
      const parent = module('parent').use(child);
      expect(parent.getModules()).toHaveLength(1);
      expect(parent.getModules()[0]).toBe(child);
    });

    it('should chain multiple children', () => {
      const plugin1: Plugin = {};
      const plugin2: Plugin = {};
      const child = module('child');
      const mod = module('test').use(plugin1).use(child).use(plugin2);
      expect(mod.getPlugins()).toHaveLength(2);
      expect(mod.getModules()).toHaveLength(1);
    });

    it('should throw if child is null', () => {
      // biome-ignore lint/suspicious/noExplicitAny: Testing error case
      expect(() => module('test').use(null as any)).toThrow('Plugin or module not provided');
    });

    it('should throw if child is undefined', () => {
      // biome-ignore lint/suspicious/noExplicitAny: Testing error case
      expect(() => module('test').use(undefined as any)).toThrow('Plugin or module not provided');
    });
  });

  describe('provide()', () => {
    it('should store registrations', () => {
      class Service {}
      const mod = module('test').provide(Service);
      expect(mod.getRegistrations()).toHaveLength(1);
    });
  });

  describe('require()', () => {
    it('should store requirements', () => {
      class Database {}
      const mod = module('test').require(Database);
      expect(mod.getRequirements()).toHaveLength(1);
    });
  });

  describe('getPlugin()', () => {
    class TestPlugin implements Plugin {
      warmup = mock(() => Promise.resolve());
    }

    it('should find a local plugin by type', () => {
      const plugin = new TestPlugin();
      const mod = module('test').use(plugin);
      expect(mod.getPlugin(TestPlugin)).toBe(plugin);
    });

    it('should find a plugin from the parent module', () => {
      const plugin = new TestPlugin();
      const parent = module('parent').use(plugin);
      const child = module('child');
      parent.use(child);
      expect(child.getPlugin(TestPlugin)).toBe(plugin);
    });

    it('should find a plugin from a grandparent', () => {
      const plugin = new TestPlugin();
      const grandparent = module('gp').use(plugin);
      const parent = module('parent');
      const child = module('child');
      grandparent.use(parent);
      parent.use(child);
      expect(child.getPlugin(TestPlugin)).toBe(plugin);
    });

    it('should throw if plugin not found', () => {
      const mod = module('test');
      expect(() => mod.getPlugin(TestPlugin)).toThrow('Plugin TestPlugin not found');
    });

    specTest(
      'should not find plugins in sibling modules',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'requirement-validation',
        check: 'a-sibling-modules-plugin-is-not-visible',
      },
      () => {
        const plugin = new TestPlugin();
        const parent = module('parent');
        const sibling1 = module('sibling1').use(plugin);
        const sibling2 = module('sibling2');
        parent.use(sibling1).use(sibling2);
        // sibling2 cannot see sibling1's plugin
        expect(() => sibling2.getPlugin(TestPlugin)).toThrow('Plugin TestPlugin not found');
      },
    );
  });

  describe('ensurePlugin()', () => {
    class TestPlugin implements Plugin {
      warmup = mock(() => Promise.resolve());
    }

    it('should return existing plugin if found', async () => {
      const plugin = new TestPlugin();
      const mod = module('test').use(plugin);
      const result = await mod.ensurePlugin(TestPlugin);
      expect(result).toBe(plugin);
    });

    it('should create and warmup a new plugin if not found', async () => {
      const mod = module('test');
      const result = await mod.ensurePlugin(TestPlugin);
      expect(result).toBeInstanceOf(TestPlugin);
      expect(result.warmup).toHaveBeenCalledWith(mod);
    });

    it('should find plugin from parent without creating locally', async () => {
      const plugin = new TestPlugin();
      const parent = module('parent').use(plugin);
      const child = module('child');
      parent.use(child);
      const result = await child.ensurePlugin(TestPlugin);
      expect(result).toBe(plugin);
      // Should not have created a new one
      expect(child.getPlugins()).toHaveLength(0);
    });
  });

  describe('collectPlugins()', () => {
    it('should collect direct plugins', () => {
      const plugin1: Plugin = {};
      const plugin2: Plugin = {};
      const mod = module('test').use(plugin1).use(plugin2);
      const collected = mod.collectPlugins();
      expect(collected).toHaveLength(2);
      expect(collected[0]).toEqual({ plugin: plugin1, owner: mod });
      expect(collected[1]).toEqual({ plugin: plugin2, owner: mod });
    });

    it('should collect plugins from sub-modules depth-first', () => {
      const plugin1: Plugin = {};
      const plugin2: Plugin = {};
      const plugin3: Plugin = {};
      const child = module('child').use(plugin2);
      const parent = module('parent').use(plugin1).use(child).use(plugin3);

      const collected = parent.collectPlugins();
      expect(collected).toHaveLength(3);
      // Order: plugin1 (parent direct), plugin2 (from child), plugin3 (parent direct)
      expect(collected[0]).toEqual({ plugin: plugin1, owner: parent });
      expect(collected[1]).toEqual({ plugin: plugin2, owner: child });
      expect(collected[2]).toEqual({ plugin: plugin3, owner: parent });
    });

    it('should handle nested modules', () => {
      const p1: Plugin = {};
      const p2: Plugin = {};
      const p3: Plugin = {};
      const p4: Plugin = {};

      const grandchild = module('grandchild').use(p3);
      const child = module('child').use(p2).use(grandchild);
      const root = module('root').use(p1).use(child).use(p4);

      const collected = root.collectPlugins();
      expect(collected).toHaveLength(4);
      expect(collected[0].plugin).toBe(p1);
      expect(collected[1].plugin).toBe(p2);
      expect(collected[2].plugin).toBe(p3);
      expect(collected[3].plugin).toBe(p4);
    });
  });

  describe('onStop() and collectShutdownHooks()', () => {
    it('should collect shutdown hooks', () => {
      const hook = async () => {};
      const mod = module('test');
      mod.onStop(hook);
      const hooks = mod.collectShutdownHooks();
      expect(hooks).toHaveLength(1);
      expect(hooks[0]).toBe(hook);
    });

    it('should collect hooks from sub-modules', () => {
      const hook1 = async () => {};
      const hook2 = async () => {};
      const child = module('child');
      child.onStop(hook2);
      const parent = module('parent');
      parent.onStop(hook1);
      parent.use(child);

      const hooks = parent.collectShutdownHooks();
      expect(hooks).toHaveLength(2);
      // Child hooks collected before parent hooks
      expect(hooks[0]).toBe(hook2);
      expect(hooks[1]).toBe(hook1);
    });
  });

  describe('module composition', () => {
    it('should compose modules with plugins and providers', () => {
      class Database {}
      class AuthService {}

      const authModule = module('auth')
        .require(Database)
        .provide(AuthService, { deps: [Database] })
        .use({ warmup: async () => {} } as Plugin);

      expect(authModule.getRegistrations()).toHaveLength(1);
      expect(authModule.getRequirements()).toHaveLength(1);
      expect(authModule.getPlugins()).toHaveLength(1);
    });

    it('should support modules using modules', () => {
      const innerModule = module('inner').use({ warmup: async () => {} } as Plugin);
      const outerModule = module('outer')
        .use(innerModule)
        .use({ start: async () => {} } as Plugin);

      expect(outerModule.getModules()).toHaveLength(1);
      expect(outerModule.getPlugins()).toHaveLength(1);
      expect(outerModule.collectPlugins()).toHaveLength(2);
    });
  });

  describe('composeModules()', () => {
    it('should create a named module while preserving the child module tree', () => {
      const usersPlugin: Plugin = {};
      const codesPlugin: Plugin = {};
      const users = module('users').use(usersPlugin);
      const codes = module('codes').use(codesPlugin);

      const composed = composeModules([users, codes], { name: 'auth.server' });

      expect(composed).toBeInstanceOf(Module);
      expect(composed.name).toBe('auth.server');
      expect(composed.getModules()).toEqual([users, codes]);
      expect(composed.collectModules()).toEqual([users, codes]);
      expect(composed.collectContainerModules()).toEqual([]);
      expect(composed.collectPlugins()).toEqual([
        { plugin: usersPlugin, owner: users },
        { plugin: codesPlugin, owner: codes },
      ]);
    });

    it('should merge child registrations and remove requirements satisfied inside the composition', () => {
      class Database {}
      class AuthService {
        constructor(public db: Database) {}
      }

      const database = module('database').provide(Database);
      const auth = module('auth')
        .require(Database)
        .provide(AuthService, { deps: [Database] });

      const composed = composeModules([database, auth], { name: 'auth.server' });

      expect(composed.getRegistrations().map((registration) => registration.provider.token)).toEqual([
        Database,
        AuthService,
      ]);
      expect(composed.getRequirements()).toEqual([]);
    });

    it('should keep unsatisfied child requirements as de-duplicated external requirements', () => {
      class Database {}
      class Cache {}
      class AuthService {}
      class StoreService {}

      const auth = module('auth').require(Database).require(Cache).provide(AuthService);
      const store = module('store').require(Database).provide(StoreService);

      const composed = composeModules([auth, store], { name: 'features' });

      expect(composed.getRequirements()).toEqual([Database, Cache]);
    });

    it('should detect duplicate providers across composed modules', () => {
      class SharedService {}

      const left = module('left').provide(SharedService);
      const right = module('right').provide(SharedService);

      expect(() => composeModules([left, right], { name: 'features' })).toThrow(
        "Duplicate provider for SharedService in composed module 'features' from 'left' and 'right'",
      );
    });

    it('should require at least one module', () => {
      expect(() => composeModules([])).toThrow('composeModules() requires at least one module');
    });
  });

  describe('secure()', () => {
    it('should store security options', () => {
      const mod = module('admin').secure({ roles: ['admin'] });
      expect(mod.getSecurity()).toEqual({ roles: ['admin'] });
    });

    it('should store empty options for .secure() with no args', () => {
      const mod = module('protected').secure();
      expect(mod.getSecurity()).toEqual({});
    });

    it('should store a guard function', () => {
      const guard = (user: Record<string, unknown>) => user.role === 'admin';
      const mod = module('guarded').secure(guard);
      expect(mod.getSecurity()).toBe(guard);
    });

    it('should return undefined when no security is set', () => {
      const mod = module('public');
      expect(mod.getSecurity()).toBeUndefined();
    });

    it('should be chainable', () => {
      const mod = module('test')
        .secure({ roles: ['admin'] })
        .use({} as Plugin);
      expect(mod.getPlugins()).toHaveLength(1);
      expect(mod.getSecurity()).toEqual({ roles: ['admin'] });
    });
  });

  describe('path()', () => {
    it('should store and retrieve path', () => {
      const mod = module('tasks').path('/tasks');
      expect(mod.getPath()).toBe('/tasks');
    });

    it('should return undefined when no path is set', () => {
      const mod = module('test');
      expect(mod.getPath()).toBeUndefined();
    });
  });

  describe('ContainerHolder interface', () => {
    describe('getRegistrations()', () => {
      it('should return empty array when no providers are registered', () => {
        const mod = module('test');
        expect(mod.getRegistrations()).toEqual([]);
      });

      it('should return all registered providers', () => {
        class ServiceA {}
        class ServiceB {}
        const mod = module('test').provide(ServiceA).provide(ServiceB);

        const registrations = mod.getRegistrations();
        expect(registrations).toHaveLength(2);
        expect(registrations[0].__brand).toBe('Registration');
        expect(registrations[1].__brand).toBe('Registration');
      });

      it('should return registrations with correct tokens', () => {
        class Database {}
        class UserService {}
        const mod = module('test')
          .provide(Database)
          .provide(UserService, { deps: [Database] });

        const registrations = mod.getRegistrations();
        expect(registrations).toHaveLength(2);
        expect(registrations[0].provider.token).toBe(Database);
        expect(registrations[1].provider.token).toBe(UserService);
      });

      it('should preserve provide options in registrations', () => {
        class Service {}
        const mod = module('test').provide(Service, {
          visibility: 'private',
          tags: ['api'],
        });

        const registrations = mod.getRegistrations();
        expect(registrations).toHaveLength(1);
        expect(registrations[0].provider.visibility).toBe('private');
        expect(registrations[0].provider.tags).toEqual(['api']);
      });

      it('should support factory-based registrations', () => {
        class Config {
          constructor(public url: string) {}
        }
        const mod = module('test').provide(Config, () => new Config('postgres://localhost'));

        const registrations = mod.getRegistrations();
        expect(registrations).toHaveLength(1);
        expect(registrations[0].provider.token).toBe(Config);
      });
    });

    describe('getRequirements()', () => {
      it('should return empty array when no requirements declared', () => {
        const mod = module('test');
        expect(mod.getRequirements()).toEqual([]);
      });

      it('should return all declared requirements', () => {
        class Database {}
        class Cache {}
        const mod = module('test').require(Database).require(Cache);

        const requirements = mod.getRequirements();
        expect(requirements).toHaveLength(2);
        expect(requirements[0]).toBe(Database);
        expect(requirements[1]).toBe(Cache);
      });

      it('should not include provide tokens in requirements', () => {
        class Database {}
        class AuthService {}
        const mod = module('test')
          .require(Database)
          .provide(AuthService, { deps: [Database] });

        expect(mod.getRequirements()).toHaveLength(1);
        expect(mod.getRequirements()[0]).toBe(Database);

        expect(mod.getRegistrations()).toHaveLength(1);
        expect(mod.getRegistrations()[0].provider.token).toBe(AuthService);
      });
    });

    describe('collectModules()', () => {
      it('should return empty array when no sub-modules exist', () => {
        const mod = module('test');
        expect(mod.collectModules()).toEqual([]);
      });

      it('should collect direct sub-modules', () => {
        const child1 = module('child1');
        const child2 = module('child2');
        const parent = module('parent').use(child1).use(child2);

        const modules = parent.collectModules();
        expect(modules).toHaveLength(2);
        expect(modules[0]).toBe(child1);
        expect(modules[1]).toBe(child2);
      });

      it('should collect nested sub-modules depth-first', () => {
        const grandchild = module('grandchild');
        const child = module('child').use(grandchild);
        const root = module('root').use(child);

        const modules = root.collectModules();
        expect(modules).toHaveLength(2);
        // child comes first, then its child (grandchild)
        expect(modules[0]).toBe(child);
        expect(modules[1]).toBe(grandchild);
      });

      it('should collect deeply nested modules in correct order', () => {
        const gc1 = module('gc1');
        const gc2 = module('gc2');
        const child1 = module('child1').use(gc1);
        const child2 = module('child2').use(gc2);
        const root = module('root').use(child1).use(child2);

        const modules = root.collectModules();
        expect(modules).toHaveLength(4);
        expect(modules[0]).toBe(child1);
        expect(modules[1]).toBe(gc1);
        expect(modules[2]).toBe(child2);
        expect(modules[3]).toBe(gc2);
      });

      it('should not include plugins in collected modules', () => {
        const plugin: Plugin = {};
        const child = module('child');
        const parent = module('parent').use(plugin).use(child).use(plugin);

        const modules = parent.collectModules();
        expect(modules).toHaveLength(1);
        expect(modules[0]).toBe(child);
      });

      it('should collect modules with their registrations intact', () => {
        class Service {}
        const child = module('child').provide(Service);
        const parent = module('parent').use(child);

        const modules = parent.collectModules();
        expect(modules).toHaveLength(1);
        expect(modules[0].getRegistrations()).toHaveLength(1);
        expect(modules[0].getRegistrations()[0].provider.token).toBe(Service);
      });
    });
  });

  describe('feature()', () => {
    const definition = {
      id: 'auth/opaque-tokens',
      name: 'Opaque tokens',
      outcome: 'Clients exchange credentials for revocable opaque tokens',
      owner: 'auth',
    };

    it('should reject a second declaration on the same module', () => {
      const mod = module('tokens').feature(definition);

      expect(() => mod.feature({ ...definition, id: 'auth/sessions' })).toThrow(
        "Module 'tokens' already declares feature 'auth/opaque-tokens'",
      );
    });

    it('should keep the design-time composition without composing anything', () => {
      const google = module('google-oauth');
      const tokens = module('tokens').feature(definition, { modules: [google], sources: ['src/api/auth/token'] });

      expect(tokens.getFeatureComposition()).toEqual({ modules: [google], sources: ['src/api/auth/token'] });
      expect(tokens.collectModules()).toEqual([]);
      expect(tokens.getPlugins()).toEqual([]);
    });

    // A workload root owns the scanned routes a feature declared further down
    // selects, so design discovery must see the whole tree rather than only the
    // features this module inherits from its ancestors.
    it('should report a feature declared anywhere in the composed tree', () => {
      const tokens = module('tokens').feature(definition);
      const workload = module('app').use(module('auth').use(tokens));

      expect(workload.declaresAnyFeature()).toBe(true);
      expect(tokens.declaresAnyFeature()).toBe(true);
      expect(workload.getEffectiveFeature()).toBeUndefined();
      expect(module('app').use(module('auth')).declaresAnyFeature()).toBe(false);
    });
  });
});
