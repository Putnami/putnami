import { describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { HttpResponse } from '@putnami/application';
import { MiddlewareBuilder, isMiddlewareDefinition, middleware } from '../../src/ssr/middleware';

describe('middleware()', () => {
  it('returns a MiddlewareBuilder', () => {
    expect(middleware()).toBeInstanceOf(MiddlewareBuilder);
  });
});

describe('MiddlewareBuilder', () => {
  it('builds a valid MiddlewareDefinition', () => {
    const def = middleware().build();

    expect(isMiddlewareDefinition(def)).toBe(true);
    expect(def.stack).toEqual([]);
    expect(typeof def.handler).toBe('function');
  });

  it('chains secure() and adds to stack', () => {
    const def = middleware()
      .secure({ roles: ['admin'] })
      .build();

    expect(def.stack).toHaveLength(1);
  });

  it('chains rateLimit() and adds to stack', () => {
    const def = middleware().rateLimit({ max: 100 }).build();

    expect(def.stack).toHaveLength(1);
  });

  it('chains multiple middleware', () => {
    const def = middleware()
      .secure({ roles: ['user'] })
      .rateLimit({ max: 50 })
      .build();

    expect(def.stack).toHaveLength(2);
  });

  it('accepts custom middleware via use()', () => {
    const customMw = mock(async (_ctx: unknown, next: () => Promise<unknown>) => next());
    const def = middleware()
      .use(customMw as never)
      .build();

    expect(def.stack).toHaveLength(1);
    expect(def.stack[0]).toBe(customMw);
  });

  specTest(
    'composes handler that calls middleware in order',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'security-propagation',
      check: 'a-declared-stack-composes-in-order',
    },
    async () => {
      const calls: string[] = [];

      const mw1 = async (_ctx: unknown, next: () => Promise<HttpResponse | undefined>) => {
        calls.push('mw1-before');
        const res = await next();
        calls.push('mw1-after');
        return res;
      };

      const mw2 = async (_ctx: unknown, next: () => Promise<HttpResponse | undefined>) => {
        calls.push('mw2-before');
        const res = await next();
        calls.push('mw2-after');
        return res;
      };

      const def = middleware()
        .use(mw1 as never)
        .use(mw2 as never)
        .build();

      const next = mock(async () => {
        calls.push('handler');
        return undefined;
      });

      await def.handler({} as never, next);

      expect(calls).toEqual(['mw1-before', 'mw2-before', 'handler', 'mw2-after', 'mw1-after']);
    },
  );

  it('returns a new stack array (not shared reference)', () => {
    const builder = middleware().secure();
    const def1 = builder.build();
    const def2 = builder.build();

    expect(def1.stack).not.toBe(def2.stack);
    expect(def1.stack).toEqual(def2.stack);
  });
});

describe('isMiddlewareDefinition', () => {
  it('returns true for valid MiddlewareDefinition', () => {
    const def = middleware().build();
    expect(isMiddlewareDefinition(def)).toBe(true);
  });

  it('returns false for null', () => {
    expect(isMiddlewareDefinition(null)).toBe(false);
  });

  it('returns false for undefined', () => {
    expect(isMiddlewareDefinition(undefined)).toBe(false);
  });

  it('returns false for plain object', () => {
    expect(isMiddlewareDefinition({ handler: () => {} })).toBe(false);
  });

  it('returns false for a plain function', () => {
    expect(isMiddlewareDefinition(() => {})).toBe(false);
  });
});
