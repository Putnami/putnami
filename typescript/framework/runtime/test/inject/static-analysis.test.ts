import { describe, expect, it } from 'bun:test';
import { ContainerContext } from '../../src/inject/container-context';
import type { Provider, Token } from '../../src/inject/inject.type';
import { provide } from '../../src/inject/provider';
import { analyzeScopeReachability } from '../../src/inject/static-analysis';
import { named } from '../../src/inject/token';
import '../../src/inject/container-context-fork';

/** Build a `Provider`-map lookup from registrations for direct analyzer tests. */
function lookupFrom(...registrations: ReturnType<typeof provide>[]) {
  const map = new Map<Token, Provider>();
  for (const reg of registrations) {
    map.set(reg.provider.token, reg.provider);
  }
  return (token: Token) => map.get(token);
}

describe('provide() — depsComplete', () => {
  it('marks class providers as deps-complete', () => {
    class Db {}
    class Service {}
    expect(provide(Service, { deps: [Db] }).provider.depsComplete).toBe(true);
    expect(provide(Service).provider.depsComplete).toBe(true);
  });

  it('marks factory providers as incomplete by default', () => {
    class Db {}
    expect(provide(Db, () => new Db()).provider.depsComplete).toBe(false);
  });

  it('lets a factory opt into deps-complete', () => {
    const Url = named<string>('url');
    class Db {}
    expect(provide(Db, () => new Db(), { deps: [Url], depsComplete: true }).provider.depsComplete).toBe(true);
  });
});

describe('analyzeScopeReachability', () => {
  it('proves a pure singleton graph is static-safe', () => {
    class Config {}
    class Repo {}
    class Service {}
    const lookup = lookupFrom(provide(Config), provide(Repo, { deps: [Config] }), provide(Service, { deps: [Repo] }));

    const result = analyzeScopeReachability([Service], lookup);
    expect(result.scoped).toEqual([]);
    expect(result.opaque).toEqual([]);
    expect(result.decidable).toBe(true);
  });

  it('reaches a directly-injected scoped provider with a path', () => {
    class RequestCtx {}
    const lookup = lookupFrom(provide(RequestCtx, { scope: 'scoped' }));

    const result = analyzeScopeReachability([RequestCtx], lookup);
    expect(result.scoped).toHaveLength(1);
    expect(result.scoped[0].token).toBe(RequestCtx);
    expect(result.scoped[0].path).toEqual([RequestCtx]);
    expect(result.decidable).toBe(true);
  });

  it('reaches a transitively-scoped provider and records the resolution path', () => {
    class RequestCtx {}
    class Repo {}
    class Service {}
    const lookup = lookupFrom(
      provide(RequestCtx, { scope: 'scoped' }),
      provide(Repo, { deps: [RequestCtx] }),
      provide(Service, { deps: [Repo] }),
    );

    const result = analyzeScopeReachability([Service], lookup);
    expect(result.scoped.map((h) => h.token)).toEqual([RequestCtx]);
    expect(result.scoped[0].path).toEqual([Service, Repo, RequestCtx]);
  });

  it('treats a factory provider without declared deps as opaque (hybrid)', () => {
    class Db {}
    class Service {}
    const lookup = lookupFrom(
      provide(Db, () => new Db()), // factory → opaque
      provide(Service, { deps: [Db] }),
    );

    const result = analyzeScopeReachability([Service], lookup);
    expect(result.scoped).toEqual([]);
    expect(result.opaque).toContain(Db);
    expect(result.decidable).toBe(false);
  });

  it('treats an unregistered token as opaque', () => {
    const Missing = named('missing');
    const result = analyzeScopeReachability([Missing], lookupFrom());
    expect(result.opaque).toContain(Missing);
    expect(result.decidable).toBe(false);
  });

  it('still reports a scoped hit found through an opaque factory subtree', () => {
    class RequestCtx {}
    class Service {}
    const lookup = lookupFrom(
      provide(RequestCtx, { scope: 'scoped' }),
      provide(Service, () => new Service(), { deps: [RequestCtx] }), // factory but deps listed
    );

    const result = analyzeScopeReachability([Service], lookup);
    expect(result.scoped.map((h) => h.token)).toEqual([RequestCtx]);
    expect(result.opaque).toContain(Service); // factory subtree stays a hybrid
    expect(result.decidable).toBe(false);
  });

  it('terminates on dependency cycles', () => {
    class A {}
    class B {}
    const lookup = lookupFrom(provide(A, { deps: [B] }), provide(B, { deps: [A] }));
    const result = analyzeScopeReachability([A], lookup);
    expect(result.scoped).toEqual([]);
    expect(result.decidable).toBe(true);
  });
});

describe('ContainerContext.analyzeScopeReachability', () => {
  it('analyzes across the root container and mounted modules', () => {
    class Config {}
    class RequestUser {}
    class Greeter {}

    const ctx = new ContainerContext('app');
    ctx.register(provide(Config));
    ctx.register(provide(RequestUser, { scope: 'scoped' }));

    const moduleHolder = {
      getRegistrations: () => [provide(Greeter, { deps: [Config] })],
      getRequirements: () => [Config as Token],
    };
    ctx.mount(moduleHolder, 'greeting');

    // Greeter → Config: all singletons → proven safe.
    const safe = ctx.analyzeScopeReachability([Greeter]);
    expect(safe.scoped).toEqual([]);
    expect(safe.decidable).toBe(true);

    // RequestUser is request-scoped → reachable.
    const unsafe = ctx.analyzeScopeReachability([RequestUser]);
    expect(unsafe.scoped.map((h) => h.token)).toEqual([RequestUser]);
  });

  it('resolves module provider dependencies from the owning module before root', () => {
    class Shared {}
    class RootService {}
    class ModuleService {}

    const ctx = new ContainerContext('app');
    ctx.register(provide(Shared));
    ctx.register(provide(RootService, { deps: [Shared] }));

    const moduleHolder = {
      getRegistrations: () => [provide(Shared, { scope: 'scoped' }), provide(ModuleService, { deps: [Shared] })],
      getRequirements: () => [],
    };
    ctx.mount(moduleHolder, 'feature');

    const moduleResult = ctx.analyzeScopeReachability([ModuleService]);
    expect(moduleResult.scoped.map((h) => h.token)).toEqual([Shared]);
    expect(moduleResult.scoped[0].path).toEqual([ModuleService, Shared]);

    const rootResult = ctx.analyzeScopeReachability([RootService]);
    expect(rootResult.scoped).toEqual([]);
    expect(rootResult.decidable).toBe(true);
  });

  it('preserves depsComplete when analyzing a forked context', () => {
    class Config {}
    class Service {}

    const ctx = new ContainerContext('app');
    ctx.register(provide(Config));
    ctx.register(provide(Service, { deps: [Config] }));

    const fork = ctx.fork();
    const result = fork.analyzeScopeReachability([Service]);
    expect(result.scoped).toEqual([]);
    expect(result.opaque).toEqual([]);
    expect(result.decidable).toBe(true);
  });

  it('does not require the context to be started', () => {
    class Config {}
    const ctx = new ContainerContext('app');
    ctx.register(provide(Config));
    expect(() => ctx.analyzeScopeReachability([Config])).not.toThrow();
  });
});
