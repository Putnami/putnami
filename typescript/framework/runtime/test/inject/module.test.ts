import { describe, expect, it } from 'bun:test';
import { Container } from '../../src/inject/container';
import { ContainerContext } from '../../src/inject/container-context';
import { NotRegisteredError, RequirementNotMetError } from '../../src/inject/errors';
import type { ContainerHolder } from '../../src/inject/inject.type';
import { provide } from '../../src/inject/provider';
import { specTest } from '../../src/spectest';

describe('Container child creation', () => {
  it('should build a child container from registrations', () => {
    class AuthService {}

    const parent = new Container('app');
    const child = parent.createChild('auth');
    child.register(provide(AuthService));

    expect(child.get(AuthService)).toBeInstanceOf(AuthService);
  });

  it('should resolve parent dependencies from child', () => {
    class Database {
      query() {
        return 'data';
      }
    }
    class UserRepo {
      constructor(public db: Database) {}
    }

    const parent = new Container('app');
    parent.register(provide(Database));

    const child = parent.createChild('users');
    child.register(provide(UserRepo, { deps: [Database] }));

    const repo = child.get(UserRepo);
    expect(repo.db.query()).toBe('data');
  });

  specTest(
    'a mounted private provider is unreachable from the root context',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'private-visibility',
      check: 'a-mounted-private-provider-is-unreachable-from-the-root',
    },
    async () => {
      class Internal {}
      class Public {}
      class NeedsInternal {
        constructor(public dep: Internal) {}
      }

      const holder: ContainerHolder = {
        getRegistrations: () => [
          provide(Internal, { visibility: 'private' }),
          provide(NeedsInternal, { deps: [Internal] }),
          provide(Public),
        ],
        getRequirements: () => [],
      };
      const ctx = new ContainerContext('test');
      ctx.mount(holder, 'mod');
      await ctx.start();

      // The module's own resolution still reaches its private provider…
      expect(ctx.get(NeedsInternal).dep).toBeInstanceOf(Internal);
      // …and the public provider is reachable from outside…
      expect(ctx.get(Public)).toBeInstanceOf(Public);
      // …but the private one resolves only inside its declaring container.
      expect(() => ctx.get(Internal)).toThrow(NotRegisteredError);
      expect(ctx.has(Internal)).toBe(false);

      await ctx.close();
    },
  );

  specTest(
    'a mounted private provider never leaks through a cross-container list',
    {
      feature: 'typescript/dependency-injection',
      requirement: 'private-visibility',
      check: 'a-mounted-private-provider-never-leaks-through-list',
    },
    async () => {
      class Internal {}
      class Public {}

      const holder: ContainerHolder = {
        getRegistrations: () => [
          provide(Internal, { visibility: 'private', tags: ['plugin'] }),
          provide(Public, { tags: ['plugin'] }),
        ],
        getRequirements: () => [],
      };
      const ctx = new ContainerContext('test');
      ctx.mount(holder, 'mod');
      await ctx.start();

      const listed = ctx.list({ tags: 'plugin' });
      expect(listed.some((item) => item instanceof Public)).toBe(true);
      expect(listed.some((item) => item instanceof Internal)).toBe(false);

      await ctx.scope(async (scope) => {
        expect(() => scope.get(Internal)).toThrow();
        const scoped = scope.list({ tags: 'plugin' });
        expect(scoped.some((item) => item instanceof Internal)).toBe(false);
      });

      await ctx.close();
    },
  );

  it('should keep private providers invisible to siblings', () => {
    class InternalHelper {}
    class PublicService {}

    const parent = new Container('app');

    const childA = parent.createChild('a');
    childA.register(provide(InternalHelper, { visibility: 'private' }));
    childA.register(provide(PublicService));

    // childA can see its own private provider
    expect(childA.get(InternalHelper)).toBeInstanceOf(InternalHelper);

    // Sibling cannot see it through parent
    const childB = parent.createChild('b');
    expect(() => childB.get(InternalHelper)).toThrow(NotRegisteredError);
  });

  it('should allow multiple children with independent registrations', () => {
    class AuthService {}
    class StoreService {}

    const parent = new Container('app');

    const childAuth = parent.createChild('auth');
    childAuth.register(provide(AuthService));

    const childStore = parent.createChild('store');
    childStore.register(provide(StoreService));

    expect(childAuth.get(AuthService)).toBeInstanceOf(AuthService);
    expect(childStore.get(StoreService)).toBeInstanceOf(StoreService);

    // Each child doesn't see sibling's providers
    expect(() => childAuth.get(StoreService)).toThrow(NotRegisteredError);
    expect(() => childStore.get(AuthService)).toThrow(NotRegisteredError);
  });
});

describe('ContainerContext mount (ContainerHolder)', () => {
  it('should mount a holder as a child container', async () => {
    class Database {}
    class AuthService {}

    const holder: ContainerHolder = {
      getRegistrations: () => [provide(AuthService, { deps: [Database] })],
      getRequirements: () => [Database],
    };

    const ctx = new ContainerContext('test');
    ctx.register(provide(Database));
    ctx.mount(holder, 'auth');

    await ctx.start();
    expect(ctx.get(AuthService)).toBeInstanceOf(AuthService);
    await ctx.close();
  });

  it('should validate requirements before mounting', () => {
    class Database {}

    const holder: ContainerHolder = {
      getRegistrations: () => [provide(class AuthService {})],
      getRequirements: () => [Database],
    };

    const ctx = new ContainerContext('test');
    // Database not registered in root
    expect(() => ctx.mount(holder, 'auth')).toThrow(RequirementNotMetError);
  });

  it('should keep mounted private providers invisible to other mounts', async () => {
    class InternalHelper {}
    class PublicService {}
    class OtherService {}

    const holderA: ContainerHolder = {
      getRegistrations: () => [provide(InternalHelper, { visibility: 'private' }), provide(PublicService)],
      getRequirements: () => [],
    };

    const holderB: ContainerHolder = {
      getRegistrations: () => [provide(OtherService)],
      getRequirements: () => [],
    };

    const ctx = new ContainerContext('test');
    ctx.mount(holderA, 'a');
    ctx.mount(holderB, 'b');

    await ctx.start();

    // PublicService from mount A is accessible
    expect(ctx.get(PublicService)).toBeInstanceOf(PublicService);
    // OtherService from mount B is accessible
    expect(ctx.get(OtherService)).toBeInstanceOf(OtherService);

    await ctx.close();
  });
});
