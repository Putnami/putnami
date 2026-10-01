import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { Provider, Token } from '../../../runtime/src/inject/inject.type';
import { provide } from '../../../runtime/src/inject/provider';
import { named } from '../../../runtime/src/inject/token';
import { isStaticRenderViolation, StaticRenderViolation } from '../../src/ssr/static';
import { proveStaticRouteSafety } from '../../src/ssr/static-di';

function lookupFrom(...registrations: ReturnType<typeof provide>[]) {
  const map = new Map<Token, Provider>();
  for (const reg of registrations) {
    map.set(reg.provider.token, reg.provider);
  }
  return (token: Token) => map.get(token);
}

describe('proveStaticRouteSafety', () => {
  it('proves a route whose loader injects nothing', () => {
    const proof = proveStaticRouteSafety({ route: '/about', roots: [] }, () => undefined);
    expect(proof).toEqual({ route: '/about', diProven: true });
  });

  specTest(
    'proves a route reaching only singletons with complete deps',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-route-reaching-only-singletons-is-proven-static',
    },
    () => {
      class Config {}
      class PostRepo {}
      const lookup = lookupFrom(provide(Config), provide(PostRepo, { deps: [Config] }));

      const proof = proveStaticRouteSafety({ route: '/blog', roots: [PostRepo] }, lookup);
      expect(proof.diProven).toBe(true);
    },
  );

  specTest(
    'throws StaticRenderViolation when a scoped provider is reachable',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-scoped-provider-reachable-from-a-static-route-is-a-violation',
    },
    () => {
      class RequestUser {}
      class Dashboard {}
      const lookup = lookupFrom(provide(RequestUser, { scope: 'scoped' }), provide(Dashboard, { deps: [RequestUser] }));

      let error: unknown;
      try {
        proveStaticRouteSafety({ route: '/dashboard', roots: [Dashboard] }, lookup);
      } catch (err) {
        error = err;
      }

      expect(isStaticRenderViolation(error)).toBe(true);
      const violation = error as StaticRenderViolation;
      expect(violation.route).toBe('/dashboard');
      expect(violation.accessed).toBe('RequestUser');
      expect(violation.resolutionPath).toEqual(['Dashboard', 'RequestUser']);
      expect(violation.message).toContain('request/session-scoped provider');
      expect(violation.message).toContain('Dashboard → RequestUser');
    },
  );

  specTest(
    'falls back to a hybrid (not proven) for an opaque factory dependency',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'an-opaque-factory-dependency-falls-back-to-hybrid',
    },
    () => {
      class Db {}
      class Service {}
      const lookup = lookupFrom(
        provide(Db, () => new Db()),
        provide(Service, { deps: [Db] }),
      );

      const proof = proveStaticRouteSafety({ route: '/feed', roots: [Service] }, lookup);
      expect(proof.diProven).toBe(false); // runtime guard backstops the opaque subtree
    },
  );

  it('falls back to a hybrid when the loader injects a dynamic selector', () => {
    class Plugin {}
    const lookup = lookupFrom(provide(Plugin));

    const proof = proveStaticRouteSafety({ route: '/plugins', roots: [Plugin], dynamicRoots: true }, lookup);
    expect(proof.diProven).toBe(false);
  });

  it('names a missing/unregistered provider as a hybrid, not a violation', () => {
    const Missing = named('externalService');
    const proof = proveStaticRouteSafety({ route: '/ext', roots: [Missing] }, () => undefined);
    expect(proof.diProven).toBe(false);
  });
});

describe('StaticRenderViolation.scope', () => {
  it('builds a DI-graph violation carrying the resolution path', () => {
    const violation = StaticRenderViolation.scope('/x', 'Session', ['Page', 'Session']);
    expect(violation).toBeInstanceOf(StaticRenderViolation);
    expect(violation.route).toBe('/x');
    expect(violation.accessed).toBe('Session');
    expect(violation.resolutionPath).toEqual(['Page', 'Session']);
    expect(isStaticRenderViolation(violation)).toBe(true);
  });

  it('keeps the original request-access constructor working', () => {
    const violation = new StaticRenderViolation('/y', 'user');
    expect(violation.message).toContain('accessed request-scoped data `ctx.user`');
    expect(violation.resolutionPath).toBeUndefined();
  });
});
